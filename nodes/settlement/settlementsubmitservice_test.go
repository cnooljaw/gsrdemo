package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type asyncSettlementResult struct {
	id      string
	receipt map[string]any
	code    string
}

type asyncSettlementCaller struct {
	results chan asyncSettlementResult
}

var _ gsr.Service = (*asyncSettlementCaller)(nil)

func (caller *asyncSettlementCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	return self.Node().RegisterService("results", self.Self())
}

func (caller *asyncSettlementCaller) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	switch command {
	case 1:
		return nil, self.Cluster().Send("settlement", settlementSubmitName, settleBatchCommand, args[0], "caller", "results")
	case settlementResultCommand:
		caller.results <- asyncSettlementResult{args[0].(string), args[1].(map[string]any), args[2].(string)}
		return nil, nil
	case 99:
		return true, nil
	}
	return nil, runtime.ErrUnknownCommand
}

func (caller *asyncSettlementCaller) OnStop(context.Context) error {
	return nil
}

type asyncSettlementMain struct {
	node         cluster.NodeID
	children     map[string]gsr.Service
	ready        chan struct{}
	startCommand runtime.CommandID
}

var _ gsr.Service = (*asyncSettlementMain)(nil)

func (main *asyncSettlementMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	ids := []runtime.ServiceID{}
	for name, child := range main.children {
		id, err := self.CreateService(ctx, child)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		if name != "" {
			if err := self.Node().RegisterService(cluster.ServiceName(name), id); err != nil {
				return err
			}
		}
	}
	if err := self.Node().OpenCluster(main.node); err != nil {
		return err
	}
	if main.startCommand != 0 {
		for _, id := range ids {
			if err := self.Send(id, main.startCommand); err != nil {
				return err
			}
		}
	}
	close(main.ready)
	self.Exit()
	return nil
}

func (main *asyncSettlementMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (main *asyncSettlementMain) OnStop(context.Context) error {
	return nil
}

func settlementAsyncPair(t *testing.T, db *sql.DB, workers int) (map[cluster.NodeID]string, <-chan asyncSettlementResult) {
	t.Helper()
	routes := map[cluster.NodeID]string{}
	for _, node := range []cluster.NodeID{"settlement", "caller"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		routes[node] = listener.Addr().String()
		_ = listener.Close()
	}
	results := make(chan asyncSettlementResult, 64)
	for _, main := range []*asyncSettlementMain{
		{node: "caller", children: map[string]gsr.Service{"": &asyncSettlementCaller{results}}, ready: make(chan struct{})},
		{node: "settlement", children: map[string]gsr.Service{"": newSettlementSubmitService(db, workers)}, ready: make(chan struct{})},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		config := gsr.Config{Runtime: gsr.RuntimeConfig{MailboxCapacity: settlementMailboxCapacity}, Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}
		go func() { done <- gsr.Run(ctx, config, main) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("异步结算节点未停止")
			}
		})
		select {
		case <-main.ready:
		case err := <-done:
			done <- err
			t.Fatal(err)
		case <-time.After(3 * time.Second):
			t.Fatal("异步结算节点未启动")
		}
	}
	return routes, results
}

// TestSettlementAsyncWorkersRunInParallel 保护慢 SQL 只占用一个执行 Service，调用方和另一个执行 Service 均可继续处理。
func TestSettlementAsyncWorkersRunInParallel(t *testing.T) {
	store, mock := mockStore(t)
	mock.MatchExpectationsInOrder(false)
	for _, id := range []string{"slow", "fast"} {
		mock.ExpectBegin()
		query := mock.ExpectQuery("SELECT request_hash, receipt_json").WithArgs(id)
		if id == "slow" {
			query.WillDelayFor(600 * time.Millisecond)
		}
		query.WillReturnError(errors.New("injected SQL unavailable"))
		mock.ExpectRollback()
	}
	routes, results := settlementAsyncPair(t, store.db, 2)
	for _, id := range []string{"slow", "fast"} {
		if _, err := businessRPC(t, routes, "caller", "results", 1, testBatch(id, testSingle("op-"+id, 1, 10, 0))); err != nil {
			t.Fatal(err)
		}
	}
	if value, err := businessRPC(t, routes, "caller", "results", 99); err != nil || value != true {
		t.Fatalf("调用方被结算阻塞: %v %v", value, err)
	}
	select {
	case result := <-results:
		if result.id != "fast" || result.code != "settlement.store_unavailable" {
			t.Fatalf("并行结果错误: %+v", result)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("独立执行 Service 被慢 SQL 阻塞")
	}
	select {
	case result := <-results:
		if result.id != "slow" || result.code != "settlement.store_unavailable" {
			t.Fatalf("慢任务结果错误: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("慢任务没有错误回执")
	}
}

// TestSettlementAsyncInvalidRequestReturnsError 保护可路由的非法结算请求通过结果消息明确拒绝，不执行 SQL。
func TestSettlementAsyncInvalidRequestReturnsError(t *testing.T) {
	store, _ := mockStore(t)
	routes, results := settlementAsyncPair(t, store.db, 2)
	if _, err := businessRPC(t, routes, "caller", "results", 1, map[string]any{"batch_id": "bad"}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results:
		if result.id != "bad" || result.code != "settlement.invalid_request" {
			t.Fatalf("非法请求回执错误: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("非法请求没有错误回执")
	}
}

// TestSettlementExpiredQueueDoesNotStartSQL 保护过载队列中的过期任务明确返回忙，不再启动迟到事务或误报财务拒绝。
func TestSettlementExpiredQueueDoesNotStartSQL(t *testing.T) {
	store, mock := mockStore(t)
	for _, id := range []string{"first", "second"} {
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT request_hash, receipt_json").WithArgs(id).WillDelayFor(1250 * time.Millisecond).WillReturnError(errors.New("injected slow SQL"))
		mock.ExpectRollback()
	}
	routes, results := settlementAsyncPair(t, store.db, 1)
	for _, id := range []string{"first", "second", "expired"} {
		if _, err := businessRPC(t, routes, "caller", "results", 1, testBatch(id, testSingle("op-"+id, 1, 10, 0))); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		select {
		case result := <-results:
			want := "settlement.store_unavailable"
			if result.id == "expired" {
				want = "settlement.busy"
			}
			if result.code != want {
				t.Fatalf("排队结果错误: %+v", result)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("排队请求未返回")
		}
	}
}
