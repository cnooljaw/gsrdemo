package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// TestSettlementSlowSQLIsLocal 保护慢请求不阻塞独立读写入口或另一结算节点。
func TestSettlementSlowSQLIsLocal(t *testing.T) {
	for _, kind := range []string{"write", "read"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{})
			var once sync.Once
			pattern := "SELECT request_hash, receipt_json"
			if kind == "read" {
				pattern = "SELECT receipt_json FROM"
			}
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expected, actual string) error {
				err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
				if err == nil && strings.Contains(actual, pattern) {
					once.Do(func() { close(started) })
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
			name, command, args := settlementWriteName, settleBatchCommand, []any{testBatch("slow", testSingle("slow-op", 1, 100, 0))}
			if kind == "write" {
				mock.ExpectBegin()
				mock.ExpectRollback()
			} else {
				name, command, args = settlementReadName, getBatchCommand, []any{"slow"}
			}
			mock.ExpectQuery(pattern).WithArgs("slow").WillDelayFor(650 * time.Millisecond).WillReturnError(errors.New("injected slow SQL"))
			mock.ExpectQuery("SELECT amount, version").WithArgs(uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}).AddRow(1000, 1))
			mock.ExpectBegin().WillReturnError(errors.New("independent write reached SQL"))
			address, _ := startSettlementTestNode(t, db)
			otherAddress, _ := startSettlementTestNode(t, db)
			routes := map[cluster.NodeID]string{"settlement": address}
			done := make(chan error, 1)
			go func() { _, err := businessRPC(t, routes, "settlement", name, command, args...); done <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("slow SQL did not start")
			}
			// 同类请求使用另一个节点；异类请求仍走原节点，均应在慢 SQL 返回前到达数据库。
			begin := time.Now()
			readRoutes, writeRoutes := routes, routes
			otherRoutes := map[cluster.NodeID]string{"settlement": otherAddress}
			if kind == "write" {
				writeRoutes = otherRoutes
			} else {
				readRoutes = otherRoutes
			}
			value, err := businessRPC(t, readRoutes, "settlement", settlementReadName, getBalanceCommand, uint64(2))
			if err != nil || value.(map[string]any)["amount"] != int64(1000) {
				t.Fatalf("read blocked: %v %v", value, err)
			}
			if _, err := businessRPC(t, writeRoutes, "settlement", settlementWriteName, settleBatchCommand, testBatch("fast", testSingle("fast-op", 2, 100, 0))); !errors.Is(err, errSettlementUnavailable) {
				t.Fatalf("write did not reach independent SQL: %v", err)
			}
			if elapsed := time.Since(begin); elapsed >= 300*time.Millisecond {
				t.Errorf("unrelated requests waited for slow SQL: %v", elapsed)
			}
			if err := <-done; !errors.Is(err, errSettlementUnavailable) {
				t.Fatalf("slow SQL result: %v", err)
			}
		})
	}
}

type settlementPanicFixture struct {
	inner gsr.Service
	name  string
}

var _ gsr.Service = (*settlementPanicFixture)(nil)

func (service *settlementPanicFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return service.inner.OnStart(ctx, self)
}
func (service *settlementPanicFixture) OnStop(ctx context.Context) error {
	return service.inner.OnStop(ctx)
}
func (service *settlementPanicFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == 998 {
		panic("injected executor panic")
	}
	if command == 997 {
		return service.name, nil
	}
	return service.inner.Handle(self, command, args...)
}

type settlementPanicMain struct {
	db    *sql.DB
	ready chan string
}

var _ gsr.Service = (*settlementPanicMain)(nil)

func (service *settlementPanicMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for _, child := range []struct {
		name    string
		service gsr.Service
	}{
		{settlementWriteName, newSettlementService(service.db)},
		{settlementReadName, newSettlementQueryService(service.db)},
	} {
		if _, err := self.CreateService(ctx, &settlementPanicFixture{inner: child.service, name: child.name}); err != nil {
			return err
		}
	}
	if err := self.Node().OpenCluster("settlement"); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(self.Node().ClusterListenAddress())
	if err != nil {
		return err
	}
	service.ready <- net.JoinHostPort("127.0.0.1", port)
	self.Exit()
	return nil
}
func (service *settlementPanicMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *settlementPanicMain) OnStop(context.Context) error { return nil }

// TestSettlementWorkerPanicStaysLocal 保护入口 panic 不影响另一入口和另一节点，失败入口不自动重放或重建。
func TestSettlementWorkerPanicStaysLocal(t *testing.T) {
	for _, kind := range []string{"write", "read"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := mockStore(t)
			main := &settlementPanicMain{db: store.db, ready: make(chan string, 1)}
			address, _ := startSettlementMain(t, main, main.ready)
			otherMain := &settlementPanicMain{db: store.db, ready: make(chan string, 1)}
			otherAddress, _ := startSettlementMain(t, otherMain, otherMain.ready)
			routes := map[cluster.NodeID]string{"settlement": address}
			target, other := settlementWriteName, settlementReadName
			if kind == "read" {
				target, other = other, target
			}
			if _, err := businessRPC(t, routes, "settlement", target, 998); !errors.Is(err, runtime.ErrServicePanicked) {
				t.Fatalf("injection did not panic: %v", err)
			}
			value, err := businessRPC(t, routes, "settlement", other, 997)
			if err != nil || value != other {
				t.Fatalf("other entry stopped: %v %v", value, err)
			}
			value, err = businessRPC(t, map[cluster.NodeID]string{"settlement": otherAddress}, "settlement", target, 997)
			if err != nil || value != target {
				t.Fatalf("other node stopped: %v %v", value, err)
			}
			if _, err := businessRPC(t, routes, "settlement", target, 997); err == nil {
				t.Fatal("failed entry unexpectedly restarted")
			}
		})
	}
}

// TestSettlementReadWriteBoundaries 保护公开的两个固定入口各司其职，查询入口不能写账，写入口不承担读取等待。
func TestSettlementReadWriteBoundaries(t *testing.T) {
	store, _ := mockStore(t)
	address, _ := startSettlementTestNode(t, store.db)
	routes := map[cluster.NodeID]string{"settlement": address}
	if _, err := businessRPC(t, routes, "settlement", "settlement-read", settleBatchCommand, testBatch("bad", testSingle("op", 1, 100, 0))); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatal(err)
	}
	if _, err := businessRPC(t, routes, "settlement", "settlement-write", getBalanceCommand, uint64(1)); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatal(err)
	}
}
