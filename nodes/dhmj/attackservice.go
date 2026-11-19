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
	Offer                      actionOptions
	SelfAction                 bool
	CanSelfWin                 bool
	ConcealedKongs, AddedKongs []byte
	Finished                   bool
	Deadline                   time.Time
	Settlement                 map[string]any
}

func (service *attackService) handleSnapshot(_ gsr.CommandContext, seat int) (tableView, error) {
	if !validSeat(seat) {
		return tableView{}, errInvalidTableAction
	}
	game := &service.game
	view := tableView{
		Hand: slices.Clone(game.hands[seat]), Trustees: game.trustees,
		Active: game.active, Turn: game.turn, Revision: game.lastDiscard.revision, Finished: game.end != nil,
		SelfAction: game.end == nil && game.window == nil && game.selfPending && seat == game.active && !game.trustees[seat],
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
	if !view.Finished {
		view.Deadline = game.dueAt()
	} else if s := service.settlement; s != nil {
		view.Settlement = map[string]any{"operation_id": s.operation.ID, "status": "pending"}
		if s.receipt != nil {
			view.Settlement = s.receipt[seat].wire(s.operation.ID)
		}
	}
	if view.SelfAction {
		view.CanSelfWin = canHu(view.Hand)
		var seen [256]bool
		for _, tile := range view.Hand {
			if tile >= 0x41 || seen[tile] {
				continue
			}
			seen[tile] = true
			if tileCount(view.Hand, tile) == 4 {
				view.ConcealedKongs = append(view.ConcealedKongs, tile)
			}
			if game.pongIndex(seat, tile) >= 0 {
				view.AddedKongs = append(view.AddedKongs, tile)
			}
		}
	}
	if game.window != nil {
		view.WindowTile = game.window.tile
		view.WindowID = game.windowID
		view.Deadline = game.deadline
		if game.window.choices[seat] == unanswered {
			view.Offer = game.window.offers[seat]
		}
	}
	return view, nil
}

func (service *attackService) handleDiscard(self gsr.CommandContext, seat int, turn uint64, tile byte) (bool, error) {
	if !service.game.discard(seat, turn, tile, time.Now()) {
		return false, errInvalidTableAction
	}
	service.sendDiscard(self)
	return true, nil
}

func (service *attackService) sendDiscard(self gsr.ServiceContext) {
	if service.directory == 0 {
		return
	}
	event := service.game.lastDiscard
	if err := self.Send(service.directory, tableDiscardedCommand, service.name, event); err != nil {
		_ = self.Logger().Error("定海麻将出牌事件未接纳", "table_name", service.name, "revision", event.revision, "error", err.Error())
	}
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

func (service *attackService) handleTick(self gsr.CommandContext) error {
	revision := service.game.lastDiscard.revision
	service.game.tick(time.Now())
	if service.game.lastDiscard.revision != revision {
		service.sendDiscard(self)
	}
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

func actionResult(ok bool) (bool, error) {
	if !ok {
		return false, errInvalidTableAction
	}
	return true, nil
}
