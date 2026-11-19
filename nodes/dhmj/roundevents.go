package main

import (
	"slices"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

type roundEventKind uint8

const (
	discardEvent roundEventKind = iota + 1
	drawEvent
	flowerEvent
	dealEvent
)

type openingDeal struct {
	dealer int
	hands  [seatCount][]byte
}

// roundEvent 保存状态变化当时的数据，按同一序号流在回合推进后发送。
type roundEvent struct {
	kind            roundEventKind
	revision, turn  uint64
	seat, remaining int
	tile            byte
	flowers         []flowerReplacement
	fromDraw        bool
	deal            *openingDeal
}

func (g *round) recordDeal() {
	deal := &openingDeal{dealer: g.dealer}
	for seat, hand := range g.hands {
		deal.hands[seat] = slices.Clone(hand)
	}
	g.recordEvent(roundEvent{kind: dealEvent, deal: deal})
}

func (g *round) recordEvent(event roundEvent) {
	g.revision++
	event.revision = g.revision
	event.remaining = g.remaining()
	g.events = append(g.events, event)
}

func (g *round) recordFlowers(seat int, flowers []flowerReplacement, fromDraw bool) {
	if len(flowers) != 0 {
		g.recordEvent(roundEvent{kind: flowerEvent, seat: seat, flowers: flowers, fromDraw: fromDraw})
	}
}

func (event roundEvent) wire(tableName string, recipient int) (runtime.CommandID, map[string]any) {
	payload := map[string]any{"table_name": tableName, "revision": event.revision, "seat": int64(event.seat)}
	switch event.kind {
	case dealEvent:
		payload["seat"], payload["dealer_seat"] = int64(recipient), int64(event.deal.dealer)
		payload["hand"], payload["remaining"] = wireTiles(event.deal.hands[recipient]), int64(event.remaining)
		return accessagent.DhmjDealPushCommand, payload
	case discardEvent:
		payload["turn"], payload["tile"] = event.turn, int64(event.tile)
		return accessagent.DhmjDiscardPushCommand, payload
	case drawEvent:
		payload["turn"], payload["remaining"] = event.turn, int64(event.remaining)
		if recipient == event.seat {
			payload["tile"] = int64(event.tile)
		}
		return accessagent.DhmjDrawPushCommand, payload
	case flowerEvent:
		flowers := make([]any, 0, len(event.flowers))
		for _, pair := range event.flowers {
			flowers = append(flowers, int64(pair.flower))
		}
		payload["flowers"], payload["remaining"], payload["from_draw"] = flowers, int64(event.remaining), event.fromDraw
		if recipient == event.seat {
			replacements := make([]any, 0, len(event.flowers))
			for _, pair := range event.flowers {
				replacements = append(replacements, int64(pair.replacement))
			}
			payload["replacements"] = replacements
		}
		return accessagent.DhmjFlowerPushCommand, payload
	default:
		panic("未知的定海麻将牌桌事件")
	}
}
