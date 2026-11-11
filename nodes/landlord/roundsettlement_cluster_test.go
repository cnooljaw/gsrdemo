package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
	"github.com/redis/go-redis/v9"
)

func startLandlordRedis(t *testing.T) *redis.Client {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, port, _ := net.SplitHostPort(address)
	command := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	client := redis.NewClient(&redis.Options{Addr: address, Protocol: 2, DisableIdentity: true, MaxRetries: -1,
		ContextTimeoutEnabled: true, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for client.Ping(context.Background()).Err() != nil {
		if time.Now().After(deadline) {
			t.Fatal("temporary Redis did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return client
}

type landlordCleanupFault struct {
	blocked atomic.Bool
	slow    bool
}

var _ redis.Hook = (*landlordCleanupFault)(nil)

func (*landlordCleanupFault) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*landlordCleanupFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (fault *landlordCleanupFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "hdel" && fault.blocked.Load() {
			if fault.slow {
				<-ctx.Done()
				return ctx.Err()
			}
			return errors.New("test Redis cleanup unavailable")
		}
		return next(ctx, cmd)
	}
}

type landlordSettlementBackend struct {
	journal         landlordJournal
	status          string
	calls           atomic.Int32
	firstError      error
	firstDelay      time.Duration
	badFirstReceipt bool
	started         chan struct{}
	requests        chan map[string]any
}

var _ gsr.Service = (*landlordSettlementBackend)(nil)

func (*landlordSettlementBackend) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*landlordSettlementBackend) OnStop(context.Context) error                      { return nil }
func (backend *landlordSettlementBackend) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd != settlementwrite.SettleEqualGroupCommand || len(args) != 1 {
		return nil, runtime.ErrUnknownCommand
	}
	request := args[0].(map[string]any)
	id := request["operation_id"].(string)
	if !backend.journal.client.HExists(context.Background(), backend.journal.key, id).Val() {
		return nil, errors.New("settlement called before journal saved")
	}
	call := backend.calls.Add(1)
	if backend.requests != nil {
		select {
		case backend.requests <- request:
		default:
		}
	}
	if call == 1 {
		if backend.started != nil {
			close(backend.started)
		}
		if backend.firstDelay != 0 {
			time.Sleep(backend.firstDelay)
		}
		if backend.firstError != nil {
			return nil, backend.firstError
		}
		if backend.badFirstReceipt {
			return map[string]any{}, nil
		}
	}
	balances := []any{}
	actual := []any{}
	for _, change := range request["changes"].([]any) {
		fields := change.(map[string]any)
		actual = append(actual, map[string]any{"user_id": fields["user_id"], "debit": fields["debit"], "credit": fields["credit"]})
		balances = append(balances, map[string]any{"user_id": change.(map[string]any)["user_id"], "amount": int64(10000), "version": uint64(2)})
	}
	receipt := map[string]any{"results": []any{map[string]any{"operation_id": id, "status": backend.status, "reason": "", "actual_changes": actual}}, "balances": balances}
	return receipt, nil
}

// landlordResultPushRecorder 观察真实 Cluster 推送；只统计局末消息。
type landlordResultPushRecorder struct{ pending, terminal int64 }

var _ gsr.Service = (*landlordResultPushRecorder)(nil)

func (*landlordResultPushRecorder) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*landlordResultPushRecorder) OnStop(context.Context) error                      { return nil }
func (recorder *landlordResultPushRecorder) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == 99 {
		return map[string]any{"pending": recorder.pending, "terminal": recorder.terminal}, nil
	}
	if cmd != accessagent.LandlordSnapshotPushCommand {
		return nil, runtime.ErrUnknownCommand
	}
	view := args[1].(map[string]any)
	if view["phase"] == "finished" {
		receipt := view["settlement"].(map[string]any)
		if receipt["status"] == "pending" {
			recorder.pending++
		} else {
			recorder.terminal++
		}
	}
	return nil, nil
}

// landlordSettlementProbe 仅增加只读观察命令，实际 Play、Leave、Pulse 和结算均使用生产实现。
type landlordSettlementProbe struct{ *attackService }

func (probe *landlordSettlementProbe) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	// 测试 Main 注册桌名；周期推进使用生产 Pulse。
	_, err := self.CreateService(ctx, &landlordPulse{target: self.Self()})
	return err
}

func (probe *landlordSettlementProbe) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == 99 {
		return probe.settlement.snapshotFor(1), nil
	}
	return probe.attackService.Handle(self, cmd, args...)
}

// TestLandlordReceiptSurvivesCleanupFailure 保护局末只广播一次最终结算，Redis 清理失败不掩盖终态，空桌仍清理且不重复结算。
func TestLandlordReceiptSurvivesCleanupFailure(t *testing.T) {
	for _, scenario := range []struct {
		name, status string
		timeout      bool
	}{
		{"出牌成功", "committed", false},
		{"超时出牌成功", "committed", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status := scenario.status
			journal := landlordJournal{client: startLandlordRedis(t), key: "test:landlord:pending"}
			fault := &landlordCleanupFault{}
			fault.blocked.Store(true)
			journal.client.AddHook(fault)
			backend := &landlordSettlementBackend{journal: journal, status: status}
			routes := map[cluster.NodeID]string{}
			startLandlordTestNode(t, routes, "settlement", map[string]gsr.Service{settlementwrite.ServiceName: backend})
			startLandlordTestNode(t, routes, "access", map[string]gsr.Service{"agent": &landlordResultPushRecorder{}})
			game := testLandlordGame(t)
			for i := range game.Players {
				game.Players[i].AccessNode, game.Players[i].AgentName = "access", "agent"
			}
			game.Players[game.Seat].Hand = []int64{3}
			uid, turn := game.Players[game.Seat].UserID, game.Turn
			if scenario.timeout {
				game.Deadline = time.Now().Add(-time.Second)
			}
			service := newAttackService(game.Name, journal, nil, "settlement")
			service.game = *game
			startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{
				game.Name: &landlordSettlementProbe{service}, landlordName: &landlordTestBackend{},
			})
			var view map[string]any
			if scenario.timeout {
				deadline := time.Now().Add(2 * time.Second)
				for {
					view = landlordTestValue(t, routes, "landlord", game.Name, roomSnapshotCommand, uid, "login").(map[string]any)
					if view["phase"] == "finished" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("超时出牌未结束牌局")
					}
					time.Sleep(20 * time.Millisecond)
				}
			} else {
				view = landlordTestValue(t, routes, "landlord", game.Name, roomPlayCommand, uid, "login", turn, []any{int64(3)}).(map[string]any)
			}
			if receipt := view["settlement"].(map[string]any); receipt["status"] != status || receipt["balance"] != int64(10000) {
				t.Fatalf("有效回执被 Redis 清理失败掩盖: %v", receipt)
			}
			deadline := time.Now().Add(time.Second)
			for {
				pushes := landlordTestValue(t, routes, "access", "agent", 99).(map[string]any)
				if pushes["pending"] != int64(0) {
					t.Fatalf("结算前仍广播待确认结果: %v", pushes)
				}
				if pushes["terminal"] == int64(3) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("三人未各收到一份终态结果: %v", pushes)
				}
				time.Sleep(10 * time.Millisecond)
			}
			for uid := uint64(1); uid <= 3; uid++ {
				landlordTestValue(t, routes, "landlord", game.Name, roomLeaveCommand, uid, "login")
			}
			// 所有玩家已离开，清理期间桌仍存在，且终态不退回 pending。
			if receipt := landlordTestValue(t, routes, "landlord", game.Name, 99).(map[string]any); receipt["status"] != status {
				t.Fatal(receipt)
			}
			if !journal.client.HExists(context.Background(), journal.key, "landlord-"+game.Name).Val() {
				t.Fatal("失败的清理丢掉恢复记录")
			}
			fault.blocked.Store(false)
			deadline = time.Now().Add(3 * time.Second)
			for {
				_, err := landlordTestRPC(t, routes, "landlord", game.Name, 99)
				if err != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Redis 恢复后空桌没有完成清理并退出")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if backend.calls.Load() != 1 || journal.client.HLen(context.Background(), journal.key).Val() != 0 {
				t.Fatalf("清理重试重复结算或保留终态记录: calls=%d", backend.calls.Load())
			}
		})
	}
}

// TestSlowCleanupKeepsSnapshotResponsive 保护 Redis 慢故障期间，已确认牌桌的快照不被周期清理重试积压饿死。
func TestSlowCleanupKeepsSnapshotResponsive(t *testing.T) {
	journal := landlordJournal{client: startLandlordRedis(t), key: "review:cleanup"}
	fault := &landlordCleanupFault{slow: true}
	fault.blocked.Store(true)
	journal.client.AddHook(fault)
	defer fault.blocked.Store(false)
	game := testLandlordGame(t)
	game.Phase, game.Winner = "finished", game.Landlord
	service := newAttackService(game.Name, journal, nil, "settlement")
	service.game = *game
	service.settlement.prepareOperation(*game)
	service.settlement.operation.Status = "committed"
	service.settlement.cleanupPending = true
	routes := map[cluster.NodeID]string{}
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{game.Name: &landlordSettlementProbe{service}})
	time.Sleep(2 * time.Second)
	_, err := landlordTestRPC(t, routes, "landlord", game.Name, roomSnapshotCommand, uint64(1), "login")
	if err != nil {
		t.Fatalf("Redis 清理每秒超时，250ms Tick 积压导致本桌快照超时: %v", err)
	}
}

// TestLandlordUnconfirmedCallRetriesOriginalOperation 保护临时错误和坏回执只重提原请求，永久错误保留 Redis 并停止自动提交。
func TestLandlordUnconfirmedCallRetriesOriginalOperation(t *testing.T) {
	for _, scenario := range []struct {
		name                 string
		err                  error
		malformed, permanent bool
	}{
		{"database_unavailable", runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "test SQL outage"), false, false},
		{"mailbox_full", runtime.ErrMailboxFull, false, false},
		{"malformed_receipt", nil, true, false},
		{"identity_conflict", runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.identity_conflict", "test identity conflict"), false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			journal := landlordJournal{client: startLandlordRedis(t), key: "test:landlord:pending"}
			backend := &landlordSettlementBackend{journal: journal, status: "committed", firstError: scenario.err,
				badFirstReceipt: scenario.malformed, requests: make(chan map[string]any, 2)}
			routes := map[cluster.NodeID]string{}
			startLandlordTestNode(t, routes, "settlement", map[string]gsr.Service{settlementwrite.ServiceName: backend})
			game := testLandlordGame(t)
			for i := range game.Players {
				game.Players[i].AccessNode, game.Players[i].AgentName = "access", "agent"
			}
			game.Players[game.Seat].Hand = []int64{3}
			service := newAttackService(game.Name, journal, nil, "settlement")
			service.game = *game
			startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{game.Name: &landlordSettlementProbe{service}})
			view := landlordTestValue(t, routes, "landlord", game.Name, roomPlayCommand,
				game.Players[game.Seat].UserID, "login", game.Turn, []any{int64(3)}).(map[string]any)
			if view["settlement"].(map[string]any)["status"] != "pending" || !journal.client.HExists(context.Background(), journal.key, "landlord-"+game.Name).Val() {
				t.Fatal("未确认结果被当作终态或丢失原请求")
			}
			if scenario.permanent {
				time.Sleep(1500 * time.Millisecond)
				if backend.calls.Load() != 1 || journal.client.HLen(context.Background(), journal.key).Val() != 1 {
					t.Fatal("永久错误仍重复提交或丢失原请求")
				}
				return
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				view = landlordTestValue(t, routes, "landlord", game.Name, roomSnapshotCommand, uint64(1), "login").(map[string]any)
				if view["settlement"].(map[string]any)["status"] == "committed" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("原请求未恢复")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if backend.calls.Load() != 2 || journal.client.HLen(context.Background(), journal.key).Val() != 0 {
				t.Fatal("重试次数或终态清理错误")
			}
			if !reflect.DeepEqual(<-backend.requests, <-backend.requests) {
				t.Fatal("重试改变了原 ID 或内容")
			}
		})
	}
}

// TestLandlordSettlementCallTimeoutKeepsOtherTablesResponsive 保护局末 Call 的超时小于接入等待，等待期间其他桌可工作，之后按原请求恢复。
func TestLandlordSettlementCallTimeoutKeepsOtherTablesResponsive(t *testing.T) {
	journal := landlordJournal{client: startLandlordRedis(t), key: "test:landlord:pending"}
	backend := &landlordSettlementBackend{journal: journal, status: "committed", firstDelay: 2100 * time.Millisecond,
		started: make(chan struct{}), requests: make(chan map[string]any, 2)}
	routes := map[cluster.NodeID]string{}
	startLandlordTestNode(t, routes, "settlement", map[string]gsr.Service{settlementwrite.ServiceName: backend})
	game := testLandlordGame(t)
	for i := range game.Players {
		game.Players[i].AccessNode, game.Players[i].AgentName = "access", "agent"
	}
	game.Players[game.Seat].Hand = []int64{3}
	service := newAttackService(game.Name, journal, nil, "settlement")
	service.game = *game
	other := newAttackService("other-table", journal, nil, "settlement")
	other.game = *testLandlordGame(t)
	other.game.Name = "other-table"
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{
		game.Name: &landlordSettlementProbe{service}, other.game.Name: &landlordSettlementProbe{other},
	})
	type response struct {
		value any
		err   error
	}
	done := make(chan response, 1)
	go func() {
		defer close(done)
		value, err := landlordTestRPC(t, routes, "landlord", game.Name, roomPlayCommand,
			game.Players[game.Seat].UserID, "login", game.Turn, []any{int64(3)})
		done <- response{value, err}
	}()
	defer func() {
		for range done {
		}
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("局末 Call 未到达结算")
	}
	begin := time.Now()
	landlordTestValue(t, routes, "landlord", other.game.Name, roomSnapshotCommand, uint64(1), "login")
	if time.Since(begin) >= 500*time.Millisecond {
		t.Fatal("其他桌被结算等待阻塞")
	}
	select {
	case result := <-done:
		if result.err != nil || result.value == nil || result.value.(map[string]any)["settlement"].(map[string]any)["status"] != "pending" {
			t.Fatalf("接入等待先于内层超时或丢失待结果: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("局末 Call 未有限返回")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		view := landlordTestValue(t, routes, "landlord", game.Name, roomSnapshotCommand, uint64(1), "login").(map[string]any)
		if view["settlement"].(map[string]any)["status"] == "committed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("超时后未恢复原请求")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if backend.calls.Load() != 2 || !reflect.DeepEqual(<-backend.requests, <-backend.requests) {
		t.Fatal("超时重试改变了请求或发生额外提交")
	}
}
