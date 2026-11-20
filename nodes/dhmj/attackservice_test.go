package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

// TestTableSnapshotClonesSeatTiles 保护跨 Service 快照的花牌、副露及副露中的牌不引用牌桌状态。
func TestTableSnapshotClonesSeatTiles(t *testing.T) {
	table := &attackService{}
	table.game.hands[1] = []byte{0x12}
	table.game.flowers[0] = []byte{0x41}
	table.game.melds[0] = []meld{{action: concealedKong, source: 0, tiles: []byte{5, 5, 5, 5}}}
	view, err := table.handleSnapshot(nil, 1)
	if err != nil || !slices.Equal(view.Hand, []byte{0x12}) ||
		!slices.Equal(view.Flowers[0], []byte{0x41}) || len(view.Melds[0]) != 1 ||
		!slices.Equal(view.Melds[0][0].tiles, []byte{5, 5, 5, 5}) {
		t.Fatalf("快照未返回本人的暗手和其他座位的公开牌：%+v %v", view, err)
	}
	view.Flowers[0][0] = 0
	view.Melds[0][0].tiles[0] = 0
	view.Melds[0][0].action = pong
	got := table.game.melds[0][0]
	if table.game.flowers[0][0] != 0x41 || got.tiles[0] != 5 || got.action != concealedKong {
		t.Fatal("修改快照改变了牌桌里的花牌或副露")
	}
}

const testObserveCommand runtime.CommandID = 1

type tableTestMain struct {
	table           *attackService
	id              runtime.ServiceID
	done            chan error
	discard         roundEvent
	initialRevision uint64
	turnEvent       roundEvent
	discardTurn     uint64
}

func (m *tableTestMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	m.table.directory = self.Self()
	id, err := self.CreateService(ctx, m.table)
	if err != nil {
		return err
	}
	m.id = id
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	first, err := self.Call(ctx, id, tableSnapshotCommand, 0)
	if err != nil {
		return err
	}
	view := first.(tableView)
	if len(view.Hand) != 14 || view.HandCounts[1] != 13 || view.Active != 0 || view.Turn == 0 {
		return errors.New("庄家没有正确开局")
	}
	m.discardTurn = view.Turn
	m.initialRevision = view.Revision
	if view.SelfAction {
		m.discardTurn++ // 托管放弃自选窗口后才进入出牌轮次。
	}
	view.Hand[0] = 0xff
	second, err := self.Call(ctx, id, tableSnapshotCommand, 0)
	if err != nil {
		return err
	}
	if second.(tableView).Hand[0] == 0xff {
		return errors.New("快照泄漏暗手引用")
	}
	if _, err := self.Call(ctx, id, tableTrusteeCommand, 0, true); err != nil {
		return err
	}
	_, err = self.After(100*time.Millisecond, testObserveCommand)
	return err
}

func (m *tableTestMain) OnStop(context.Context) error { return nil }

func (m *tableTestMain) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == tableEventCommand && self.Source() == m.id {
		event := args[1].(roundEvent)
		switch event.kind {
		case discardEvent:
			m.discard = event
		case turnEvent:
			m.turnEvent = event
		}
		return nil, nil
	}
	if cmd != testObserveCommand {
		return nil, runtime.ErrUnknownCommand
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, err := self.Call(ctx, m.id, tableSnapshotCommand, 1)
	if err != nil {
		return nil, err
	}
	view := value.(tableView)
	if len(view.Out[0]) == 0 || m.discard.revision == 0 || m.turnEvent.revision <= m.discard.revision {
		_, err := self.After(100*time.Millisecond, testObserveCommand)
		return nil, err
	}
	if view.Out[0][0] != 0x37 || !view.Trustees[0] || len(view.Hand) != view.HandCounts[1] ||
		view.Revision != m.turnEvent.revision || m.turnEvent.operation.state.phase != view.OperationPhase ||
		m.discard.revision <= m.initialRevision ||
		m.discard.turn != m.discardTurn || m.discard.seat != 0 || m.discard.tile != 0x37 {
		m.done <- errors.New("Pulse 未用托管推进，或快照含错误座位的手牌")
	} else {
		m.done <- nil
	}
	self.Exit()
	return nil, nil
}

// TestAttackServiceRunsOwnTable 保护真实 GSR 邮箱、Timer 托管推进及快照所有权。
func TestAttackServiceRunsOwnTable(t *testing.T) {
	wall := wallWith(t, map[int]byte{52: 0x37})
	table, err := newAttackService(wall, 0, 0x31)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	run := make(chan error, 1)
	go func() {
		run <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}}, &tableTestMain{table: table, done: done})
	}()
	select {
	case err := <-done:
		cancel()
		<-run
		if err != nil {
			t.Fatal(err)
		}
	case err := <-run:
		t.Fatalf("牌桌提前退出：%v", err)
	case <-ctx.Done():
		cancel()
		t.Fatalf("牌桌未按时推进：%v", ctx.Err())
	}
}
