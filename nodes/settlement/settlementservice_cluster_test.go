package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type settlementTestMain struct {
	db    *sql.DB
	ready chan string
}

var _ gsr.Service = (*settlementTestMain)(nil)

func (service *settlementTestMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, newSettlementService(service.db))
	if err != nil {
		return err
	}
	if err := self.Node().RegisterService(settlementServiceName, id); err != nil {
		return err
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
func (service *settlementTestMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *settlementTestMain) OnStop(context.Context) error {
	return nil
}

func startSettlementTestNode(t *testing.T, db *sql.DB) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	main := &settlementTestMain{db: db, ready: make(chan string, 1)}
	directory := t.TempDir()
	go func() {
		done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: directory}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": "127.0.0.1:0"}}}, main)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("settlement shutdown timed out")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case address := <-main.ready:
		return address, stop
	case err := <-done:
		done <- err
		t.Fatalf("settlement startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("settlement startup timed out")
	}
	return "", stop
}

type settlementCaller struct {
	work   func(gsr.ServiceContext)
	cancel context.CancelFunc
}

var _ gsr.Service = (*settlementCaller)(nil)

func (service *settlementCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	service.work(self)
	service.cancel()
	self.Exit()
	return nil
}
func (service *settlementCaller) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *settlementCaller) OnStop(context.Context) error {
	return nil
}

func settlementRPC(t *testing.T, address string, command runtime.CommandID, args ...any) (any, error) {
	t.Helper()
	return businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", settlementServiceName, command, args...)
}

func businessRPC(t *testing.T, routes map[cluster.NodeID]string, node, name string, command runtime.CommandID, args ...any) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var value any
	var callErr error
	caller := &settlementCaller{cancel: cancel, work: func(self gsr.ServiceContext) {
		wait, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		value, callErr = self.Cluster().Call(wait, cluster.NodeID(node), cluster.ServiceName(name), command, args...)
	}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}, caller); err != nil {
		t.Fatal(err)
	}
	return value, callErr
}

// TestSettlementClusterContractAndFailureBoundary 保护真实 TCP 的命令分流、规范类型和数据库错误隔离，不把失败当作财务拒绝。
func TestSettlementClusterContractAndFailureBoundary(t *testing.T) {
	store, mock := mockStore(t)
	address, _ := startSettlementTestNode(t, store.db)
	if _, err := settlementRPC(t, address, settleBatchCommand, testBatch("bad", map[string]any{"changes": []any{}})); !errors.Is(err, errInvalidSettlement) {
		t.Fatalf("invalid batch: %v", err)
	}
	if _, err := settlementRPC(t, address, settleSingleCommand, testSingle("op", 1, 100, 0)); !errors.Is(err, errInvalidSettlement) {
		t.Fatalf("single requires version: %v", err)
	}
	single := testSingle("single", 1, 100, 0)
	single["version"] = uint64(2)
	group := map[string]any{"version": uint64(2), "operation_id": "group", "business": "demo", "business_ref": "round", "detail": map[string]any{}, "changes": []any{
		map[string]any{"user_id": uint64(1), "reason": "game_win_loss", "debit": int64(100), "credit": int64(0)}, map[string]any{"user_id": uint64(2), "reason": "game_win_loss", "debit": int64(0), "credit": int64(100)}}}
	for _, entry := range []struct {
		command runtime.CommandID
		request map[string]any
	}{{settleSingleCommand, single}, {settleGroupCommand, group}} {
		mock.ExpectBegin().WillReturnError(errors.New("injected SQL outage"))
		if _, err := settlementRPC(t, address, entry.command, entry.request); !errors.Is(err, errSettlementUnavailable) {
			t.Fatalf("write command %d did not reach its transaction: %v", entry.command, err)
		}
	}

	mock.ExpectQuery("SELECT amount, version").WithArgs(uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}).AddRow(1000, 1))
	value, err := settlementRPC(t, address, getBalanceCommand, uint64(1))
	if err != nil || value.(map[string]any)["amount"] != int64(1000) || value.(map[string]any)["version"] != uint64(1) {
		t.Fatalf("wire balance: %v %v", value, err)
	}
	mock.ExpectQuery("SELECT amount, version").WillReturnError(errors.New("private SQL diagnostic"))
	if _, err := settlementRPC(t, address, getBalanceCommand, uint64(1)); !errors.Is(err, errSettlementUnavailable) {
		t.Fatalf("database error escaped: %v", err)
	}
	mock.ExpectQuery("SELECT amount, version").WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}))
	if _, err := settlementRPC(t, address, getBalanceCommand, uint64(99)); !errors.Is(err, errWalletNotFound) {
		t.Fatalf("missing wallet was auto-funded: %v", err)
	}
	if _, err := settlementRPC(t, address, runtime.CommandID(5), "demo"); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatalf("removed Summary command remains callable: %v", err)
	}
}
