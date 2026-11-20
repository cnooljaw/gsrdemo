package main

import (
	"github.com/cnooljaw/gsr"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

func (game *comDhmj) handleMeldPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	revision, revisionOK := payload["revision"].(uint64)
	seat, seatOK := payload["seat"].(int64)
	index, indexOK := payload["meld_index"].(int64)
	if !revisionOK || revision == 0 || !seatOK || seat < 0 || seat >= 4 || !indexOK || index < 0 || index >= 4 {
		return errInvalidBackendReply
	}
	message := &demopb.DhmjMeldPush{}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	meld := message.GetMeld()
	if meld == nil || meld.SourceSeat < 0 || meld.SourceSeat >= 4 {
		return errInvalidBackendReply
	}
	var size int
	switch meld.Kind {
	case "eat", "pong":
		size = 3
	case "direct_kong", "concealed_kong", "added_kong":
		size = 4
	default:
		return errInvalidBackendReply
	}
	if len(meld.Tiles) != size {
		return errInvalidBackendReply
	}
	for _, tile := range meld.Tiles {
		if tile <= 0 || tile > 0x37 {
			return errInvalidBackendReply
		}
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_MELD_PUSH), message)
}

func (game *comDhmj) handleRoundEndPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	if revision, ok := payload["revision"].(uint64); !ok || revision == 0 {
		return errInvalidBackendReply
	}
	message := &demopb.DhmjRoundEndPush{}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	if !validDhmjRoundEnd(message.RoundEnd) {
		return errInvalidBackendReply
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_ROUND_END_PUSH), message)
}

func validDhmjRoundEnd(end *demopb.DhmjRoundEnd) bool {
	if end == nil {
		return false
	}
	var seen [4]bool
	for _, winner := range end.Winners {
		if winner == nil || winner.Seat < 0 || winner.Seat >= 4 || seen[winner.Seat] || winner.Point < 0 {
			return false
		}
		seen[winner.Seat] = true
	}
	if end.SourceSeat != nil && (end.GetSourceSeat() < 0 || end.GetSourceSeat() >= 4 || seen[end.GetSourceSeat()]) {
		return false
	}
	tile := end.GetTile()
	if end.Tile != nil && (tile <= 0 || tile > 0x37) {
		return false
	}
	switch end.Kind {
	case "draw":
		return len(end.Winners) == 0 && end.SourceSeat == nil && end.Tile == nil && !end.RobKong
	case "eight_flowers":
		return len(end.Winners) == 1 && end.SourceSeat == nil && end.Tile == nil && !end.RobKong
	case "self_hu":
		return len(end.Winners) == 1 && end.SourceSeat == nil && end.Tile != nil && !end.RobKong
	case "kong_flower":
		return len(end.Winners) == 1 && end.SourceSeat != nil && end.Tile != nil && !end.RobKong
	case "discard_hu":
		return len(end.Winners) > 0 && end.SourceSeat != nil && end.Tile != nil
	}
	return false
}
