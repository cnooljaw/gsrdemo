package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	landlordrpc "github.com/cnooljaw/gsrdemo/common/rpc/landlord"
	"github.com/cnooljaw/gsrdemo/common/rpc/landlordattack"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

// reservationDirectoryProbe 只在 owner 回调内推进预留期限，避免测试等待三十秒或跨 Service 改状态。
type reservationDirectoryProbe struct{ *landlordService }

var _ gsr.Service = (*reservationDirectoryProbe)(nil)

func (probe *reservationDirectoryProbe) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == 98 || cmd == 99 {
		room := args[0].(string)
		table, exists := probe.tables[room]
		if cmd == 99 {
			for uid := range table.Pending {
				table.Pending[uid] = time.Now().Add(-time.Second)
			}
			return nil, nil
		}
		return map[string]any{"exists": exists, "count": int64(table.Count), "pending": int64(len(table.Pending))}, nil
	}
	return probe.landlordService.Handle(self, cmd, args...)
}

type reservationTableProbe struct {
	*attackService
	joinDelay time.Duration
	joined    chan struct{}
}

var _ gsr.Service = (*reservationTableProbe)(nil)

func (*reservationTableProbe) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	_, err := self.CreateService(ctx, &landlordPulse{target: self.Self()})
	return err
}

func (probe *reservationTableProbe) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	value, err := probe.attackService.Handle(self, cmd, args...)
	if cmd == landlordattack.RoomJoinCommand && err == nil && probe.joinDelay != 0 {
		close(probe.joined)
		time.Sleep(probe.joinDelay)
	}
	return value, err
}

// startReservationTable 先建立有效预留再启动牌桌，使首次 Pulse 与测试网络启动耗时无关。
func startReservationTable(t *testing.T, users int, login string, delay time.Duration) (map[cluster.NodeID]string, string, map[uint64]string, <-chan struct{}) {
	t.Helper()
	const room = "reservation-table"
	directory := newLandlordService(landlordJournal{}, "settlement")
	directory.tables[room] = landlordTable{}
	memberships := make(map[uint64]string)
	for uid := uint64(1); uid <= uint64(users); uid++ {
		route, err := directory.handleResolve(nil, uid, login)
		if err != nil {
			t.Fatal(err)
		}
		memberships[uid] = route["membership"].(string)
	}
	joined := make(chan struct{})
	table := &reservationTableProbe{attackService: newAttackService(room, landlordJournal{}, nil, "settlement"), joinDelay: delay, joined: joined}
	routes := make(map[cluster.NodeID]string)
	startLandlordTestNode(t, routes, "online", map[string]gsr.Service{onlinerpc.ServiceName: &landlordTestBackend{logins: map[uint64]string{}}})
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{landlordrpc.ServiceName: &reservationDirectoryProbe{directory}, room: table})
	return routes, room, memberships, joined
}

// TestUnjoinedReservationReclaimsLiveTable 保护明确拒绝和完全未发 Join 的预留到期后释放目录与空桌，迟到 Join 不能复活旧预留。
func TestUnjoinedReservationReclaimsLiveTable(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name, login := "join_never_sent", "login"
		if rejected {
			name, login = "join_rejected", "old-login"
		}
		t.Run(name, func(t *testing.T) {
			routes, room, memberships, _ := startReservationTable(t, 3, login, 0)
			if rejected {
				for uid := uint64(1); uid <= 3; uid++ {
					_, err := landlordTestRPC(t, routes, "landlord", room, landlordattack.RoomJoinCommand, uid, login, "access-01", "agent", memberships[uid])
					if !errors.Is(err, errLandlordStale) {
						t.Fatalf("未明确拒绝旧登录: %v", err)
					}
				}
			}
			landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, 99, room)
			if !rejected {
				_, err := landlordTestRPC(t, routes, "landlord", room, landlordattack.RoomJoinCommand, uint64(1), login, "access-01", "agent", memberships[1])
				if !errors.Is(err, errLandlordStale) && !errors.Is(err, runtime.ErrServiceNotFound) && !errors.Is(err, runtime.ErrServiceClosed) {
					t.Fatalf("迟到 Join 没有被拒绝: %v", err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				_, err := landlordTestRPC(t, routes, "landlord", room, landlordattack.RoomSnapshotCommand, uint64(1), login)
				if errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("过期空桌未退出: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			view := landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, 98, room).(map[string]any)
			if view["exists"] != false || view["count"] != int64(0) {
				t.Fatal("空桌退出却残留目录预留")
			}
			landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, landlordrpc.ResolveCommand, uint64(4), "login")
		})
	}
}

// TestTimedOutJoinKeepsActualMember 保护 Join 已执行但真实 Call 超时后，Pulse 仍依据实际成员确认入座，不按过期预留删除。
func TestTimedOutJoinKeepsActualMember(t *testing.T) {
	routes, room, memberships, joined := startReservationTable(t, 1, "login", 1500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	caller := &landlordTestCaller{ctx: ctx, cancel: cancel, node: "landlord", name: room, cmd: landlordattack.RoomJoinCommand,
		args: []any{uint64(1), "login", "access-01", "agent", memberships[1]}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}, caller); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(caller.err, context.DeadlineExceeded) {
		t.Fatalf("Join 没有发生真实等待超时: %v", caller.err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("测试 Join 未在超时前实际完成")
	}
	landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, 99, room)
	view := landlordTestValue(t, routes, "landlord", room, landlordattack.RoomSnapshotCommand, uint64(1), "login").(map[string]any)
	if len(view["players"].([]any)) != 1 {
		t.Fatal("Call 超时后的真实成员被删除")
	}
	directory := landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, 98, room).(map[string]any)
	if directory["count"] != int64(1) || directory["pending"] != int64(0) {
		t.Fatal("实际入座没有解除预留期限")
	}
}
