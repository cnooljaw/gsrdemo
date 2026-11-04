package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
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

type landlordCleanupFault struct{ blocked atomic.Bool }

var _ redis.Hook = (*landlordCleanupFault)(nil)

func (*landlordCleanupFault) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*landlordCleanupFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (fault *landlordCleanupFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "hdel" && fault.blocked.Load() {
			return errors.New("test Redis cleanup unavailable")
		}
		return next(ctx, cmd)
	}
}

type landlordSettlementBackend struct {
	journal landlordJournal
	status  string
	calls   atomic.Int32
}

var _ gsr.Service = (*landlordSettlementBackend)(nil)

func (*landlordSettlementBackend) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*landlordSettlementBackend) OnStop(context.Context) error                      { return nil }
func (backend *landlordSettlementBackend) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd != 7 {
		return nil, runtime.ErrUnknownCommand
	}
	request := args[0].(map[string]any)
	id := request["operation_id"].(string)
	if !backend.journal.client.HExists(context.Background(), backend.journal.key, id).Val() {
		return nil, errors.New("settlement called before journal saved")
	}
	backend.calls.Add(1)
	balances := []any{}
	for _, change := range request["changes"].([]any) {
		balances = append(balances, map[string]any{"user_id": change.(map[string]any)["user_id"], "amount": int64(10000), "version": uint64(2)})
	}
	receipt := map[string]any{"results": []any{map[string]any{"operation_id": id, "status": backend.status, "reason": ""}}, "balances": balances}
	return nil, self.Cluster().Send(cluster.NodeID(args[1].(string)), cluster.ServiceName(args[2].(string)), roomSettlementReceiptCommand, id, receipt, "")
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
	if cmd != landlordSnapshotPushCommand {
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
	// 测试 Main 已负责注册桌名，回执地址和 Pulse 使用生产组装。
	if err := probe.registerSettlementReply(self); err != nil {
		return err
	}
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
		{"出牌拒绝", "rejected", false},
		{"超时出牌成功", "committed", true},
		{"超时出牌拒绝", "rejected", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			status := scenario.status
			journal := landlordJournal{client: startLandlordRedis(t), key: "test:landlord:pending"}
			fault := &landlordCleanupFault{}
			fault.blocked.Store(true)
			journal.client.AddHook(fault)
			backend := &landlordSettlementBackend{journal: journal, status: status}
			routes := map[cluster.NodeID]string{}
			startLandlordTestNode(t, routes, "settlement", map[string]gsr.Service{"settlement-submit": backend})
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
			resultDeadline := time.Now().Add(2 * time.Second)
			for view["settlement"].(map[string]any)["status"] == "pending" {
				if time.Now().After(resultDeadline) {
					t.Fatal("异步结算回执未到达")
				}
				time.Sleep(10 * time.Millisecond)
				view = landlordTestValue(t, routes, "landlord", game.Name, roomSnapshotCommand, uid, "login").(map[string]any)
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
