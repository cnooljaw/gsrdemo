package main

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	roomJoinCommand     runtime.CommandID = 1
	roomReadyCommand    runtime.CommandID = 2
	roomPlayCommand     runtime.CommandID = 3
	roomSnapshotCommand runtime.CommandID = 4
	roomLeaveCommand    runtime.CommandID = 5
	roomBidCommand      runtime.CommandID = 6
	roomRobCommand      runtime.CommandID = 7
	roomTickCommand     runtime.CommandID = 10
	landlordPushCommand runtime.CommandID = 110
)

// attackService 独占一桌的状态；局末 Redis/Settlement 等待只影响本桌。
type attackService struct {
	game       attackGame
	journal    landlordJournal
	result     *landlordResult
	recovery   bool
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*attackService)(nil)

func newAttackService(name string, journal landlordJournal, pending *landlordResult) *attackService {
	service := &attackService{game: newAttackGame(name), journal: journal, result: pending, recovery: pending != nil}
	if pending != nil {
		service.game.Phase = "finished"
	}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		roomJoinCommand:     service.handleJoin,
		roomReadyCommand:    service.handleReady,
		roomPlayCommand:     service.handlePlay,
		roomBidCommand:      service.handleCallLandlord,
		roomRobCommand:      service.handleRobLandlord,
		roomSnapshotCommand: service.handleSnapshot,
		roomLeaveCommand:    service.handleLeave,
		roomTickCommand:     service.handleTick,
	})
	return service
}

func (service *attackService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if !service.recovery {
		if err := self.Node().RegisterService(cluster.ServiceName(service.game.Name), self.Self()); err != nil {
			return err
		}
	}
	_, err := self.CreateService(ctx, &landlordPulse{target: self.Self()})
	return err
}

func (service *attackService) OnStop(context.Context) error {
	return nil
}

func (service *attackService) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, cmd, args...)
}

func (service *attackService) handleTick(self gsr.CommandContext) error {
	// 唤醒来自本地的独立 Pulse；远程 Command 的 Source 为零。
	if self.Source() == 0 {
		return runtime.ErrUnknownCommand
	}
	now := time.Now()
	if (service.game.Phase == "calling" || service.game.Phase == "robbing") && !now.Before(service.game.Deadline) {
		if err := service.applyBid(self, service.game.Players[service.game.Seat].UserID, service.game.Turn, false); err != nil {
			return err
		}
		service.broadcast(self)
	}
	if service.game.Phase == "playing" && !now.Before(service.game.Deadline) {
		uid := service.game.Players[service.game.Seat].UserID
		if err := service.game.play(uid, service.game.Turn, service.game.defaultPlay(), now); err != nil {
			return err
		}
		service.finishGame()
		service.broadcast(self)
	}
	if service.result != nil && service.result.Status == "pending" {
		if err := service.settle(self); err != nil {
			_ = self.Logger().Error("斗地主待结算保留原操作", "operation_id", service.result.ID, "error", err.Error())
			return nil
		}
		if service.recovery {
			self.Exit()
			return nil
		}
		service.broadcast(self)
	}
	return nil
}

// landlordPulse 只负责唤醒本桌；先续订自身 Timer，再非阻塞 Send，满队列不切断后续超时推进。
type landlordPulse struct{ target runtime.ServiceID }

var _ gsr.Service = (*landlordPulse)(nil)

func (pulse *landlordPulse) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(250*time.Millisecond, 1)
	return err
}

func (pulse *landlordPulse) OnStop(context.Context) error {
	return nil
}

func (pulse *landlordPulse) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != 1 || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	if _, err := self.After(250*time.Millisecond, 1); err != nil {
		return nil, err
	}
	err := self.Send(pulse.target, roomTickCommand)
	if err != nil && !errors.Is(err, runtime.ErrMailboxFull) {
		self.Exit()
		return nil, err
	}
	return nil, nil
}
