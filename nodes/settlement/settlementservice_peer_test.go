package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
)

type settlementPeerResult struct {
	index   int
	elapsed time.Duration
	err     error
}
type settlementPeerRequest struct {
	index   int
	name    string
	command runtime.CommandID
	args    []any
	wait    <-chan struct{}
	issued  chan<- struct{}
	results chan<- settlementPeerResult
}

var _ gsr.Service = (*settlementPeerRequest)(nil)

func (request *settlementPeerRequest) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(time.Millisecond, 1)
	return err
}
func (request *settlementPeerRequest) OnStop(context.Context) error { return nil }
func (request *settlementPeerRequest) Handle(self gsr.CommandContext, _ runtime.CommandID, _ ...any) (any, error) {
	if request.wait != nil {
		<-request.wait
	}
	if request.issued != nil {
		request.issued <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	begin := time.Now()
	_, err := self.Cluster().Call(ctx, "settlement", cluster.ServiceName(request.name), request.command, request.args...)
	cancel()
	request.results <- settlementPeerResult{request.index, time.Since(begin), err}
	self.Exit()
	return nil, nil
}

type settlementPeerMain struct{ requests []*settlementPeerRequest }

var _ gsr.Service = (*settlementPeerMain)(nil)

func (service *settlementPeerMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for _, request := range service.requests {
		if _, err := self.CreateService(ctx, request); err != nil {
			return err
		}
	}
	self.Exit()
	return nil
}
func (service *settlementPeerMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *settlementPeerMain) OnStop(context.Context) error { return nil }

// TestSettlementBusyEntryDoesNotExhaustPeer 保护同一来源节点共用连接时，慢入口积压不会占满所有入站 Call 槽位。
func TestSettlementBusyEntryDoesNotExhaustPeer(t *testing.T) {
	started, allowed, issued := make(chan struct{}), make(chan struct{}), make(chan struct{}, 40)
	var startOnce, allowOnce sync.Once
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
		if err == nil && strings.Contains(actual, "SELECT request_hash") {
			startOnce.Do(func() { close(started) })
		}
		return err
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	mock.MatchExpectationsInOrder(false)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT request_hash").WithArgs("slow-peer").WillDelayFor(650 * time.Millisecond).WillReturnError(errors.New("slow SQL"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT amount, version").WithArgs(uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}).AddRow(1000, 1))
	address, _ := startSettlementTestNode(t, db)
	writes, reads := routing.Services()
	results := make(chan settlementPeerResult, 42)
	requests := []*settlementPeerRequest{{index: 0, name: writes[0], command: settleBatchCommand, args: []any{testBatch("slow-peer", testSingle("slow-op", 1, 100, 0))}, results: results}}
	for index := range 40 {
		// 未知业务命令仍需进入同一 Mailbox；排队后无需额外 SQL，便于精确观察背压。
		requests = append(requests, &settlementPeerRequest{index: index + 2, name: writes[0], command: 99, wait: started, issued: issued, results: results})
	}
	requests = append(requests, &settlementPeerRequest{index: 1, name: reads[1], command: getBalanceCommand, args: []any{uint64(2)}, wait: allowed, results: results})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	directory := t.TempDir()
	go func() {
		done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: directory}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": address}}}, &settlementPeerMain{requests: requests})
	}()
	defer func() {
		startOnce.Do(func() { close(started) })
		allowOnce.Do(func() { close(allowed) })
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("caller shutdown timed out")
		}
	}()
	for range 40 {
		select {
		case <-issued:
		case <-time.After(time.Second):
			t.Fatal("flood not issued")
		}
	}
	// 留出源端编码入队时间，健康请求经相同 NodeSender/TCP 连接排在这批请求后面。
	time.Sleep(20 * time.Millisecond)
	allowOnce.Do(func() { close(allowed) })
	full := 0
	for range requests {
		select {
		case result := <-results:
			if errors.Is(result.err, runtime.ErrMailboxFull) {
				full++
			}
			if result.index == 1 && (result.err != nil || result.elapsed >= 300*time.Millisecond) {
				t.Errorf("healthy request blocked by peer admission: %v %v", result.elapsed, result.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("peer requests did not finish")
		}
	}
	if full == 0 {
		t.Error("overloaded entry did not apply early backpressure")
	}
}
