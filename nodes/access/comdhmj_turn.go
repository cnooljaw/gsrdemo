package main

import (
	"github.com/cnooljaw/gsr"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

func (game *comDhmj) handleTurnPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	seat, seatOK := payload["seat"].(int64)
	active, activeOK := payload["active_seat"].(int64)
	revision, revisionOK := payload["revision"].(uint64)
	if !seatOK || seat < 0 || seat >= 4 || !activeOK || active < 0 || active >= 4 || !revisionOK || revision == 0 {
		return errInvalidBackendReply
	}
	if seat != game.seat {
		return nil
	}
	message := &demopb.DhmjTurnPush{}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	if !validDhmjTurn(message) {
		return errInvalidBackendReply
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_TURN_PUSH), message)
}

func validDhmjTurn(turn *demopb.DhmjTurnPush) bool {
	if turn.ServerMs <= 0 || (turn.Phase != "finished" && (turn.Turn == 0 || turn.DeadlineMs <= 0)) {
		return false
	}
	for _, offer := range turn.Offers {
		if offer < 1 || offer > 7 {
			return false
		}
	}
	hasSelfOptions := turn.CanSelfWin || len(turn.ConcealedKongs) != 0 || len(turn.AddedKongs) != 0
	if turn.Phase == "respond" {
		return turn.WindowId != 0 && turn.WindowTile > 0 && turn.WindowTile <= 0x37 &&
			!hasSelfOptions && !turn.CanDiscard
	}
	if turn.WindowId != 0 || turn.WindowTile != 0 || turn.RobKong || len(turn.Offers) != 0 {
		return false
	}
	switch turn.Phase {
	case "discard":
		return !hasSelfOptions && (!turn.CanDiscard || turn.Seat == turn.ActiveSeat)
	case "self":
		return turn.Seat == turn.ActiveSeat || (!hasSelfOptions && !turn.CanDiscard)
	case "finished":
		return turn.DeadlineMs == 0 && !hasSelfOptions && !turn.CanDiscard
	default:
		return false
	}
}
