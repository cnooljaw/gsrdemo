package main

import (
	"context"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

var errInvalidActionInput = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.invalid_action", "非法麻将动作参数")

func (d *directory) callTable(self gsr.CommandContext, uid uint64, login string, cmd runtime.CommandID, args ...any) (bool, error) {
	table, seat, err := d.currentSeat(uid, login)
	if err != nil {
		return false, err
	}
	if table.service == 0 {
		return false, errRoomBusy
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	value, err := self.Call(ctx, table.service, cmd, append([]any{seat}, args...)...)
	if err != nil {
		return false, err
	}
	if value != true {
		return false, errInvalidReply
	}
	return true, nil
}

func playableTile(value int64) (byte, bool) {
	if value < 1 || value > 0x37 {
		return 0, false
	}
	tile := byte(value)
	_, ok := countHand([]byte{tile})
	return tile, ok
}

func (d *directory) handleDiscard(self gsr.CommandContext, uid uint64, login string, turn uint64, value int64) (bool, error) {
	tile, valid := playableTile(value)
	if !valid {
		return false, errInvalidActionInput
	}
	return d.callTable(self, uid, login, tableDiscardCommand, turn, tile)
}

func (d *directory) handleRespond(self gsr.CommandContext, uid uint64, login string, windowID uint64, action int64) (bool, error) {
	if action < int64(eatRight) || action > int64(pass) {
		return false, errInvalidActionInput
	}
	return d.callTable(self, uid, login, tableRespondCommand, windowID, responseAction(action))
}

func (d *directory) handleSelfWin(self gsr.CommandContext, uid uint64, login string, turn uint64) (bool, error) {
	return d.callTable(self, uid, login, tableSelfWinCommand, turn)
}

func (d *directory) handleKong(self gsr.CommandContext, uid uint64, login string, turn uint64, value int64) (bool, error) {
	tile, valid := playableTile(value)
	if !valid {
		return false, errInvalidActionInput
	}
	return d.callTable(self, uid, login, tableKongCommand, turn, tile)
}

func (d *directory) handlePassSelf(self gsr.CommandContext, uid uint64, login string, turn uint64) (bool, error) {
	return d.callTable(self, uid, login, tablePassSelfCommand, turn)
}

func (d *directory) handleTrustee(self gsr.CommandContext, uid uint64, login string, enabled bool) (bool, error) {
	return d.callTable(self, uid, login, tableTrusteeCommand, enabled)
}
