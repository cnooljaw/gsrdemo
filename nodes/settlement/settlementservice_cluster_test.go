package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
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

// 功能用例使用小邮箱；生产入口容量由 YAML 指定，压测按档位设置。
const settlementMailboxCapacity = 4

type settlementTestMain struct {
	db      *sql.DB
	workers int
	ready   chan string
}

var _ gsr.Service = (*settlementTestMain)(nil)

func (service *settlementTestMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if err := NewMain("settlement", service.db, service.workers).OnStart(ctx, self); err != nil {
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
	return startSettlementNode(t, db, defaultSettlementWorkers, settlementMailboxCapacity)
}

func startSettlementNode(t *testing.T, db *sql.DB, workers, mailbox int) (string, func()) {
	t.Helper()
	main := &settlementTestMain{db: db, workers: workers, ready: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	directory := t.TempDir()
	go func() {
		done <- gsr.Run(ctx, gsr.Config{Runtime: gsr.RuntimeConfig{MailboxCapacity: mailbox}, Logging: logging.Config{Directory: directory}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": "127.0.0.1:0"}}}, main)
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
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var value any
	var callErr error
	caller := &settlementCaller{cancel: cancel, work: func(self gsr.ServiceContext) {
		value, callErr = settlementCall(ctx, self, command, args...)
	}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": address}}}, caller); err != nil {
		t.Fatal(err)
	}
	return value, callErr
}

// settlementCall 供功能测试和压测直达固定公开入口；未知命令交由读入口验证拒绝契约。
func settlementCall(ctx context.Context, self gsr.ServiceContext, command runtime.CommandID, args ...any) (any, error) {
	name := settlementReadName
	if command == settleBatchCommand || command == settleSingleCommand || command == settleGroupCommand {
		name = settlementWriteName
	}
	return self.Cluster().Call(ctx, "settlement", cluster.ServiceName(name), command, args...)
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
	if err := gsr.Run(ctx, gsr.Config{Runtime: gsr.RuntimeConfig{MailboxCapacity: settlementMailboxCapacity}, Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}, caller); err != nil {
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

// TestSettlementReadWriteBoundaries 保护只读入口与受控 Call 写入口的命令边界，查询不能写账，写入口不接受查询。
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
	if _, err := businessRPC(t, routes, "settlement", "settlement-submit", settleBatchCommand, map[string]any{}); !errors.Is(err, runtime.ErrServiceNotFound) {
		t.Fatalf("旧 Send 入口仍存在: %v", err)
	}
}

type settlementBatchStarted struct {
	id      string
	started chan struct{}
}

func (argument settlementBatchStarted) Match(value driver.Value) bool {
	if value != argument.id {
		return false
	}
	select {
	case argument.started <- struct{}{}:
	default:
	}
	return true
}

// TestSettlementCallPoolSlowSQLIsolation 保护普通 Cluster Call 写入口的慢 SQL 不阻塞其他写入和独立查询，回执与错误直接返回各调用者。
func TestSettlementCallPoolSlowSQLIsolation(t *testing.T) {
	store, mock := mockStore(t)
	mock.MatchExpectationsInOrder(false)
	started := make(chan struct{}, 1)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT request_hash, receipt_json").
		WithArgs(settlementBatchStarted{"slow", started}).
		WillDelayFor(time.Second).
		WillReturnError(errors.New("injected SQL outage"))
	mock.ExpectRollback()

	request := testBatch("fast", testSingle("op-fast", 2, 10, 0))
	batch, err := decodeSettlementBatch(request)
	if err != nil {
		t.Fatal(err)
	}
	hash := batch.digest()
	receipt := settlementReceipt{BatchID: "fast", Results: []settlementResult{{OperationID: "op-fast", Status: "committed", OriginBatch: "fast"}}, Balances: []walletBalance{{UserID: 2, Amount: 990, Version: 2}}}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT request_hash, receipt_json").WithArgs("fast").
		WillReturnRows(sqlmock.NewRows([]string{"request_hash", "receipt_json"}).AddRow(hash[:], encoded))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT amount, version").WithArgs(uint64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}).AddRow(1000, 1))

	address, _ := startSettlementTestNode(t, store.db)
	slow := make(chan error, 1)
	go func() {
		defer close(slow)
		_, err := settlementRPC(t, address, settleBatchCommand, testBatch("slow", testSingle("op-slow", 1, 10, 0)))
		slow <- err
	}()
	// 即使前面的断言失败，也等当前调用结束后再关闭节点与数据库。
	defer func() {
		if err := <-slow; !errors.Is(err, errSettlementUnavailable) {
			t.Errorf("慢 SQL 未返回稳定错误: %v", err)
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("慢请求未进入 SQL")
	}
	begin := time.Now()
	value, err := settlementRPC(t, address, settleBatchCommand, request)
	if err != nil || value.(map[string]any)["batch_id"] != "fast" {
		t.Fatalf("快速写入未返回原回执: %v %v", value, err)
	}
	value, err = settlementRPC(t, address, getBalanceCommand, uint64(3))
	if err != nil || value.(map[string]any)["amount"] != int64(1000) {
		t.Fatalf("独立查询被阻塞: %v %v", value, err)
	}
	if elapsed := time.Since(begin); elapsed >= 500*time.Millisecond {
		t.Fatalf("其他写入或查询等待了慢 SQL: %v", elapsed)
	}
}
