package main

import (
	"github.com/cnooljaw/gsr"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// dhmjTileEventFields 校验摸牌、补花共用的座位和牌墙字段，包括合法零值的类型。
func dhmjTileEventFields(payload map[string]any) bool {
	revision, revisionOK := payload["revision"].(uint64)
	seat, seatOK := payload["seat"].(int64)
	remaining, remainingOK := payload["remaining"].(int64)
	return revisionOK && revision > 0 && seatOK && seat >= 0 && seat < 4 &&
		remainingOK && remaining >= 0 && remaining <= 144
}

func (game *comDhmj) handleDrawPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	turn, ok := payload["turn"].(uint64)
	if !ok || turn == 0 || !dhmjTileEventFields(payload) {
		return errInvalidBackendReply
	}
	message := &demopb.DhmjDrawPush{}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	if message.Seat != game.seat {
		message.Tile = nil
	} else if message.GetTile() <= 0 || message.GetTile() > 0x37 {
		return errInvalidBackendReply
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_DRAW_PUSH), message)
}

func (game *comDhmj) handleFlowerPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	if !dhmjTileEventFields(payload) {
		return errInvalidBackendReply
	}
	message := &demopb.DhmjFlowerPush{}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	if len(message.Flowers) == 0 || len(message.Flowers) > 8 {
		return errInvalidBackendReply
	}
	for _, flower := range message.Flowers {
		if flower < 0x41 || flower > 0x48 {
			return errInvalidBackendReply
		}
	}
	if message.Seat != game.seat {
		message.Replacements = nil
	} else {
		if len(message.Flowers) != len(message.Replacements) {
			return errInvalidBackendReply
		}
		for _, tile := range message.Replacements {
			if tile < 0 || tile > 0x48 {
				return errInvalidBackendReply
			}
		}
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_FLOWER_PUSH), message)
}
