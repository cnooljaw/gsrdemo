package main

import (
	"context"
	"slices"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// 牌桌命令只供本节点使用。目录核对登录并派发。
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
	tableCloseCommand    runtime.CommandID = 9
)

var errInvalidTableAction = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.invalid_turn", "定海麻将当前轮次或动作无效")

// attackService 独占单桌状态。
type attackService struct {
	game        round
	dispatcher  *gsr.Dispatcher
	users       [seatCount]uint64
	rules       roomRules
	journal     settlementJournal
	settlement  *roundSettlement
	scoreFailed bool
	lastTurn    turnState
	name        string
	directory   runtime.ServiceID
	notified    bool
	closing     bool
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
		tableCloseCommand:    service.handleClose,
	})
	return service, nil
}

func (service *attackService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	service.game.startPlay(time.Now())
	_, err := self.CreateService(ctx, &pulseService{target: self.Self(), command: tableTickCommand, period: pulsePeriod})
	if err == nil {
		service.sendEvents(self)
	}
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
	Hand                       []byte
	HandCounts                 [seatCount]int
	Out                        [seatCount][]byte
	Flowers                    [seatCount][]byte
	Melds                      [seatCount][]meld
	Trustees                   [seatCount]bool
	Active                     int
	Turn                       uint64
	Revision                   uint64
	WindowID                   uint64
	WindowTile                 byte
	OperationPhase             string
	RobKong, CanDiscard        bool
	Offer                      actionOptions
	SelfAction                 bool
	CanSelfWin                 bool
	ConcealedKongs, AddedKongs []byte
	Finished                   bool
	Deadline                   time.Time
	Settlement                 map[string]any
	RoundEnd                   map[string]any
}

func (service *attackService) handleSnapshot(_ gsr.CommandContext, seat int) (tableView, error) {
	if !validSeat(seat) {
		return tableView{}, errInvalidTableAction
	}
	game := &service.game
	state := game.currentTurn()
	view := tableView{
		Hand: slices.Clone(game.hands[seat]), Trustees: game.trustees,
		Active: state.active, Turn: state.turn, Revision: game.revision, Finished: state.phase == "finished",
		OperationPhase: state.phase, WindowID: state.windowID, WindowTile: state.windowTile, RobKong: state.robKong,
		Deadline: state.deadline, Offer: game.offersFor(seat),
		SelfAction: state.self && seat == state.active, CanDiscard: state.discard && seat == state.active,
	}
	for i := range seatCount {
		view.HandCounts[i] = len(game.hands[i])
		view.Out[i] = slices.Clone(game.out[i])
		view.Flowers[i] = slices.Clone(game.flowers[i])
		view.Melds[i] = slices.Clone(game.melds[i])
		for j := range view.Melds[i] {
			meld := &view.Melds[i][j]
			meld.tiles = slices.Clone(meld.tiles)
		}
	}
	if view.Finished {
		view.RoundEnd = game.end.wire()
	}
	if s := service.settlement; view.Finished && s != nil {
		view.Settlement = map[string]any{"operation_id": s.operation.ID, "status": "pending"}
		if s.receipt != nil {
			view.Settlement = s.receipt[seat].wire(s.operation.ID)
		}
	}
	if view.SelfAction {
		view.CanSelfWin, view.ConcealedKongs, view.AddedKongs = game.selfOptions(seat)
	}
	return view, nil
}

func (service *attackService) handleDiscard(self gsr.CommandContext, seat int, turn uint64, tile byte) (bool, error) {
	return service.actionResult(self, service.game.discard(seat, turn, tile, time.Now()))
}

func (service *attackService) sendEvents(self gsr.ServiceContext) {
	state := service.game.currentTurn()
	if state != service.lastTurn {
		if state.phase == "finished" && service.lastTurn.phase != "finished" {
			service.game.recordRoundEnd()
		}
		service.game.recordTurn(state, time.Now())
		service.lastTurn = state
	}
	for _, event := range service.game.events {
		if service.directory == 0 {
			break
		}
		if err := self.Send(service.directory, tableEventCommand, service.name, event); err != nil {
			_ = self.Logger().Error("定海麻将牌桌事件未接纳", "table_name", service.name, "revision", event.revision, "error", err.Error())
		}
	}
	clear(service.game.events)
	service.game.events = service.game.events[:0]
}

func (service *attackService) handleRespond(self gsr.CommandContext, seat int, windowID uint64, action responseAction) (bool, error) {
	return service.actionResult(self, service.game.respond(seat, windowID, action, time.Now()))
}

func (service *attackService) handleSelfWin(self gsr.CommandContext, seat int, turn uint64) (bool, error) {
	return service.actionResult(self, service.game.selfWin(seat, turn, time.Now()))
}

func (service *attackService) handleKong(self gsr.CommandContext, seat int, turn uint64, tile byte) (bool, error) {
	return service.actionResult(self, service.game.declareKong(seat, turn, tile, time.Now()))
}

func (service *attackService) handlePassSelf(self gsr.CommandContext, seat int, turn uint64) (bool, error) {
	return service.actionResult(self, service.game.passSelf(seat, turn, time.Now()))
}

func (service *attackService) handleTrustee(self gsr.CommandContext, seat int, enabled bool) (bool, error) {
	return service.actionResult(self, service.game.setTrustee(seat, enabled, time.Now()))
}

func (service *attackService) handleTick(self gsr.CommandContext) error {
	service.game.tick(time.Now())
	service.sendEvents(self)
	if service.game.end == nil || service.scoreFailed {
		return nil
	}
	if service.settlement == nil {
		if service.journal.client == nil {
			return nil
		}
		operation, err := newRoundOperation(service.users, service.rules, *service.game.end)
		if err != nil {
			service.scoreFailed = true
			_ = self.Logger().Error("定海麻将终局无法生成结算请求", "error", err.Error())
			return err
		}
		service.settlement = &roundSettlement{operation: operation}
	}
	service.settlement.advance(self, service.journal)
	if service.closing && service.settlement.cleared {
		self.Exit()
		return nil
	}
	if !service.notified && service.directory != 0 && service.settlement.receipt != nil {
		if err := self.Send(service.directory, tableSettledCommand, service.name, service.settlement.operation.ID, *service.settlement.receipt); err != nil {
			return err // 邮箱满时保留未通知状态，下一次 Pulse 重试。
		}
		service.notified = true
	}
	return nil
}

func (service *attackService) handleClose(self gsr.CommandContext) error {
	if self.Source() != service.directory || service.settlement == nil || service.settlement.receipt == nil {
		return runtime.ErrUnknownCommand
	}
	service.closing = true
	if service.settlement.cleared {
		self.Exit()
		return nil
	}
	return nil
}

func (service *attackService) actionResult(self gsr.ServiceContext, ok bool) (bool, error) {
	if !ok {
		return false, errInvalidTableAction
	}
	service.sendEvents(self)
	return true, nil
}
