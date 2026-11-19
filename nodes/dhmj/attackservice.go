package dhmj

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// 当前只供本节点使用。对 access 开放的命令由身份与路由接线另行定义。
//
//rpc:service dhmjattack private
const (
	tableSnapshotCommand runtime.CommandID = 1
	tableDiscardCommand  runtime.CommandID = 2
	tableRespondCommand  runtime.CommandID = 3
	tableSelfWinCommand  runtime.CommandID = 4
	tableKongCommand     runtime.CommandID = 5
	tablePassSelfCommand runtime.CommandID = 6
	tableTrusteeCommand  runtime.CommandID = 7
	tableTickCommand     runtime.CommandID = 8
)

var errInvalidTableAction = errors.New("定海麻将当前轮次或动作无效")

// attackService 独占单桌状态。
type attackService struct {
	game       round
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*attackService)(nil)

func newAttackService(wall []byte, dealer int, roundWind byte) (*attackService, error) {
	game, err := dealRound(wall, dealer, roundWind)
	if err != nil {
		return nil, err
	}
	service := &attackService{game: game}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		tableSnapshotCommand: service.handleSnapshot,
		tableDiscardCommand:  service.handleDiscard,
		tableRespondCommand:  service.handleRespond,
		tableSelfWinCommand:  service.handleSelfWin,
		tableKongCommand:     service.handleKong,
		tablePassSelfCommand: service.handlePassSelf,
		tableTrusteeCommand:  service.handleTrustee,
		tableTickCommand:     service.handleTick,
	})
	return service, nil
}

func (service *attackService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	service.game.startPlay(time.Now())
	_, err := self.CreateService(ctx, &tablePulse{target: self.Self()})
	return err
}

func (*attackService) OnStop(context.Context) error { return nil }

func (service *attackService) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if self.Source() == 0 {
		return nil, runtime.ErrUnknownCommand
	}
	return service.dispatcher.Handle(self, cmd, args...)
}

type tableView struct {
	Hand       []byte
	HandCounts [seatCount]int
	Out        [seatCount][]byte
	Trustees   [seatCount]bool
	Active     int
	Turn       uint64
	WindowID   uint64
	Offer      actionOptions
	SelfAction bool
	Finished   bool
	Deadline   time.Time
}

func (service *attackService) handleSnapshot(_ gsr.CommandContext, seat int) (tableView, error) {
	if !validSeat(seat) {
		return tableView{}, errInvalidTableAction
	}
	game := &service.game
	view := tableView{
		Hand: slices.Clone(game.hands[seat]), Trustees: game.trustees,
		Active: game.active, Turn: game.turn, Finished: game.end != nil,
		SelfAction: game.end == nil && game.window == nil && game.selfPending && seat == game.active && !game.trustees[seat],
	}
	for i := range seatCount {
		view.HandCounts[i] = len(game.hands[i])
		view.Out[i] = slices.Clone(game.out[i])
	}
	if !view.Finished {
		view.Deadline = game.dueAt()
	}
	if game.window != nil {
		view.WindowID = game.windowID
		view.Deadline = game.deadline
		if game.window.choices[seat] == unanswered {
			view.Offer = game.window.offers[seat]
		}
	}
	return view, nil
}

func (service *attackService) handleDiscard(_ gsr.CommandContext, seat int, turn uint64, tile byte) (bool, error) {
	return actionResult(service.game.discard(seat, turn, tile, time.Now()))
}

func (service *attackService) handleRespond(_ gsr.CommandContext, seat int, windowID uint64, action responseAction) (bool, error) {
	return actionResult(service.game.respond(seat, windowID, action, time.Now()))
}

func (service *attackService) handleSelfWin(_ gsr.CommandContext, seat int, turn uint64) (bool, error) {
	return actionResult(service.game.selfWin(seat, turn, time.Now()))
}

func (service *attackService) handleKong(_ gsr.CommandContext, seat int, turn uint64, tile byte) (bool, error) {
	return actionResult(service.game.declareKong(seat, turn, tile, time.Now()))
}

func (service *attackService) handlePassSelf(_ gsr.CommandContext, seat int, turn uint64) (bool, error) {
	return actionResult(service.game.passSelf(seat, turn, time.Now()))
}

func (service *attackService) handleTrustee(_ gsr.CommandContext, seat int, enabled bool) (bool, error) {
	return actionResult(service.game.setTrustee(seat, enabled, time.Now()))
}

func (service *attackService) handleTick(_ gsr.CommandContext) error {
	service.game.tick(time.Now())
	return nil
}

func actionResult(ok bool) (bool, error) {
	if !ok {
		return false, errInvalidTableAction
	}
	return true, nil
}
