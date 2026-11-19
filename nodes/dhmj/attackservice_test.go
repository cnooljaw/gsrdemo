package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

const testObserveCommand runtime.CommandID = 1

type tableTestMain struct {
	table *attackService
	id    runtime.ServiceID
	done  chan error
}

func (m *tableTestMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
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

func (m *tableTestMain) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
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
	if len(view.Out[0]) == 0 {
		_, err := self.After(100*time.Millisecond, testObserveCommand)
		return nil, err
	}
	if view.Out[0][0] != 0x37 || !view.Trustees[0] || len(view.Hand) != view.HandCounts[1] {
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
