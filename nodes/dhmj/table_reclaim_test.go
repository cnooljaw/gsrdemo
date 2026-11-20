package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
