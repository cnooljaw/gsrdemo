package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

type reclaimFixture struct {
	*directory
	table   *table
	attack  *attackService
	settled chan struct{}
}

func (f *reclaimFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if err := f.directory.OnStart(ctx, self); err != nil {
		return err
	}
	f.attack.name, f.attack.directory = f.table.name, self.Self()
	id, err := self.CreateService(ctx, f.attack)
	f.table.service = id
	return err
}

func (f *reclaimFixture) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	value, err := f.directory.Handle(self, cmd, args...)
	if err == nil && cmd == tableSettledCommand {
		close(f.settled)
	}
	return value, err
}

// TestSettledTableClosesAfterRejoinAndLeave 保护结算消息、生效后的四人重登回执、空桌退出及旧路由清理。
func TestSettledTableClosesAfterRejoinAndLeave(t *testing.T) {
	routes := make(map[cluster.NodeID]string)
	startDirectoryFixture(t, routes, "online", onlinerpc.ServiceName, directoryBackend{logins: map[uint64]string{
		1: "new", 2: "new", 3: "new", 4: "new",
	}})
	d := newDirectory(defaultRoomRules, settlementJournal{})
	var oldTable *table
	var users [seatCount]uint64
	for i := range users {
		uid := uint64(i + 1)
		users[i] = uid
		joined, seat, err := d.seatPlayer(uid, "old")
		if err != nil || seat != i {
			t.Fatalf("无法准备原桌：%v", err)
		}
		joined.players[seat].connected = false
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
			select {
			case <-f.settled:
			case <-ctx.Done():
				return ctx.Err()
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
