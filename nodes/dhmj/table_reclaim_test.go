package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

type reclaimFixture struct {
	*directory
	table   *table
	attack  *attackService
	settled chan struct{}
}

const testStartReclaimTableCommand runtime.CommandID = 100

func (f *reclaimFixture) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == testStartReclaimTableCommand {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		f.attack.name, f.attack.directory = f.table.name, self.Self()
		id, err := self.CreateService(ctx, f.attack)
		f.table.service = id
		return nil, err
	}
	value, err := f.directory.Handle(self, cmd, args...)
	if err == nil && cmd == tableSettledCommand {
		close(f.settled)
	}
	return value, err
}

// TestAllOfflineTableContinuesToRoundEnd 保护四人局中离开后牌桌仍由 Pulse 推进，并保留原桌成员。
func TestAllOfflineTableContinuesToRoundEnd(t *testing.T) {
	routes := make(map[cluster.NodeID]string)
	startDirectoryFixture(t, routes, "online", onlinerpc.ServiceName, directoryBackend{
		logins: map[uint64]string{1: "new"},
	})
	d := newDirectory(defaultRoomRules, settlementJournal{})
	var original *table
	for uid := uint64(1); uid <= seatCount; uid++ {
		joined, _, _, err := d.seatPlayer(uid, "old")
		if err != nil {
			t.Fatal(err)
		}
		original = joined
	}
	attack, err := newAttackService(wallWith(t, nil), 0, 0x31)
	if err != nil {
		t.Fatal(err)
	}
	// 下一次摸牌耗尽牌墙，终局必须由离线后的自动出牌触发。
	attack.game = round{
		turn: 1, deadline: time.Now().Add(discardPeriod), lastDraw: 5,
		hands: [seatCount][]byte{{5, 9}, {0x31}, {0x32}, {0x33}},
	}
	f := &reclaimFixture{directory: d, table: original, attack: attack, settled: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}
	verified := false
	err = gsr.Run(ctx, cfg, fixtureMain{onStart: func(ctx context.Context, self gsr.ServiceContext) error {
		id, err := self.CreateService(ctx, f)
		if err != nil {
			return err
		}
		if _, err := self.Call(ctx, id, testStartReclaimTableCommand); err != nil {
			return err
		}
		for uid := uint64(1); uid <= seatCount; uid++ {
			if left, err := self.Call(ctx, id, leaveCommand, uid, "old"); err != nil || left != true {
				return fmt.Errorf("玩家 %d 离开未确认：%v %v", uid, left, err)
			}
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			value, err := self.Call(ctx, original.service, tableSnapshotCommand, 0)
			if err != nil {
				return err
			}
			view := value.(tableView)
			if view.Trustees != [seatCount]bool{true, true, true, true} {
				return fmt.Errorf("四个离线座位未进入托管：%v", view.Trustees)
			}
			if view.Finished {
				if view.RoundEnd["kind"] != "draw" || !slices.Equal(view.Out[0], []byte{5}) {
					return fmt.Errorf("未由托管出牌推进至流局：%+v", view)
				}
				break
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		result, err := self.Call(ctx, id, joinCommand, uint64(1), "new", "access-01", "agent")
		if err != nil {
			return err
		}
		scene := result.(map[string]any)
		if scene["table_name"] != original.name || scene["phase"] != "finished" || len(scene["players"].([]any)) != seatCount {
			return fmt.Errorf("全员离线后丢失原桌成员：%v", scene)
		}
		verified = true
		self.Exit()
		cancel()
		return nil
	}})
	if !verified || (err != nil && !errors.Is(err, context.Canceled)) {
		t.Fatalf("全员离线联调未通过：%v", err)
	}
}

// TestSettledTableClosesAfterRejoinAndLeave 保护在线玩家收到本人回执、离线玩家重登恢复及空桌退出。
func TestSettledTableClosesAfterRejoinAndLeave(t *testing.T) {
	routes := make(map[cluster.NodeID]string)
	startDirectoryFixture(t, routes, "online", onlinerpc.ServiceName, directoryBackend{logins: map[uint64]string{
		1: "new", 2: "new", 3: "new", 4: "new",
	}})
	pushes := make(chan []any, seatCount)
	startDirectoryFixture(t, routes, "access-01", "agent", agentPushSink{command: accessagent.DhmjSettlementPushCommand, received: pushes, includeRoundEnd: true})
	d := newDirectory(defaultRoomRules, settlementJournal{})
	var oldTable *table
	var users [seatCount]uint64
	for i := range users {
		uid := uint64(i + 1)
		users[i] = uid
		joined, seat, _, err := d.seatPlayer(uid, "old")
		if err != nil || seat != i {
			t.Fatalf("无法准备原桌：%v", err)
		}
		player := &joined.players[seat]
		player.access, player.agent = "access-01", "agent"
		player.connected = i == 0 || i == seatCount-1
		if player.connected {
			player.login = fmt.Sprintf("current-%d", uid)
		}
		oldTable = joined
	}
	attack, err := newAttackService(wallWith(t, nil), 0, 0x31)
	if err != nil {
		t.Fatal(err)
	}
	end := roundEnd{kind: draw, dealer: 0}
	op, err := newRoundOperation(users, defaultRoomRules, end)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := op.receipt(drawReceipt(op))
	if err != nil {
		t.Fatal(err)
	}
	attack.game.end = &end
	attack.game.flowers[0] = []byte{0x41, 0x42}
	attack.game.melds[0] = []meld{{action: concealedKong, source: 0, tiles: []byte{5, 5, 5, 5}}}
	seatZeroTiles := map[string]any{
		"flowers": []any{int64(0x41), int64(0x42)},
		"melds": []any{map[string]any{
			"kind": "concealed_kong", "source_seat": int64(0), "tiles": []any{int64(5), int64(5), int64(5), int64(5)},
		}},
	}
	attack.settlement = &roundSettlement{operation: op, receipt: &receipt, cleared: true}
	f := &reclaimFixture{directory: d, table: oldTable, attack: attack, settled: make(chan struct{})}
	oldName := oldTable.name
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var verified bool
	err = gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}, fixtureMain{
		onStart: func(ctx context.Context, self gsr.ServiceContext) error {
			id, err := self.CreateService(ctx, f)
			if err != nil {
				return err
			}
			if _, err := self.Call(ctx, id, testStartReclaimTableCommand); err != nil {
				return err
			}
			select {
			case <-f.settled:
			case <-ctx.Done():
				return ctx.Err()
			}
			for _, seat := range []int{0, seatCount - 1} {
				want := []any{fmt.Sprintf("current-%d", users[seat]), map[string]any{
					"table_name": oldName, "revision": uint64(1), "round_end": end.wire(),
				}}
				select {
				case got := <-pushes:
					if !reflect.DeepEqual(got, want) {
						return fmt.Errorf("终局裁决没有先于资金回执发出：%v", got)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			for _, seat := range []int{0, seatCount - 1} {
				want := []any{fmt.Sprintf("current-%d", users[seat]), map[string]any{
					"table_name": oldName,
					"settlement": receipt[seat].wire(op.ID),
				}}
				select {
				case got := <-pushes:
					if !reflect.DeepEqual(got, want) {
						return fmt.Errorf("定海麻将结算推送错误：%v，预期 %v", got, want)
					}
				case <-ctx.Done():
					return fmt.Errorf("座位 %d 未收到结算推送：%w", seat, ctx.Err())
				}
			}
			for _, uid := range users {
				value, err := self.Call(ctx, id, joinCommand, uid, "new", "access-01", "agent")
				if err != nil {
					return err
				}
				snapshot := value.(map[string]any)
				silver, _ := snapshot["settlement"].(map[string]any)
				if snapshot["table_name"] != oldName || silver["status"] != "committed" || silver["user_id"] != uid {
					return fmt.Errorf("重登未返回原桌的本人结算：%v", snapshot)
				}
				if !reflect.DeepEqual(snapshot["round_end"], end.wire()) {
					return fmt.Errorf("重登未恢复终局裁决：%v", snapshot["round_end"])
				}
				// 第一座离开后，其副露和花牌仍是本局的公开牌面。
				areas, _ := snapshot["seat_tiles"].([]any)
				if len(areas) != seatCount || !reflect.DeepEqual(areas[0], seatZeroTiles) {
					return fmt.Errorf("重登未恢复四个座位的公开牌面：%v", areas)
				}
				if _, err := self.Call(ctx, id, leaveCommand, uid, "old"); !errors.Is(err, errStaleLogin) {
					return fmt.Errorf("旧登录未被拒绝：%v", err)
				}
				if _, err := self.Call(ctx, id, leaveCommand, uid, "new"); err != nil {
					return err
				}
			}
			// 源码中的 Close 消息先于来自本测试的探测发送；探测若已入队，退出仍应终止该 Call。
			_, err = self.Call(ctx, oldTable.service, tableSnapshotCommand, 0)
			if !errors.Is(err, runtime.ErrServiceNotFound) && !errors.Is(err, runtime.ErrServiceClosed) &&
				!errors.Is(err, runtime.ErrServiceStopping) {
				return fmt.Errorf("最后一人离开后牌桌仍可调用：%v", err)
			}
			value, err := self.Call(ctx, id, joinCommand, users[0], "new", "access-01", "agent")
			if err != nil {
				return err
			}
			if snapshot := value.(map[string]any); snapshot["table_name"] == oldName || snapshot["phase"] != "waiting" {
				return fmt.Errorf("释放后未分配新桌：%v", snapshot)
			}
			verified = true
			self.Exit()
			cancel()
			return nil
		},
	})
	if !verified || (err != nil && !errors.Is(err, context.Canceled)) {
		t.Fatalf("麻将空桌回收联调未通过：%v", err)
	}
}
