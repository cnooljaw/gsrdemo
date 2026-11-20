package main

import (
	"slices"
	"time"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

type roundEventKind uint8

const (
	discardEvent roundEventKind = iota + 1
	drawEvent
	flowerEvent
	dealEvent
	turnEvent
	meldEvent
	roundEndEvent
)

type openingDeal struct {
	dealer int
	hands  [seatCount][]byte
}

type turnNotice struct {
	state            turnState
	serverMS         int64
	offers           [seatCount]actionOptions
	canWin           bool
	concealed, added []byte
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
	operation       *turnNotice
	meld            *meldChange
	end             *roundEnd
}

type meldChange struct {
	index int
	value meld
}

func (g *round) recordMeld(seat, index int) {
	value := g.melds[seat][index]
	value.tiles = slices.Clone(value.tiles)
	g.recordEvent(roundEvent{kind: meldEvent, seat: seat, meld: &meldChange{index: index, value: value}})
}

func (g *round) recordRoundEnd() {
	end := *g.end
	end.winners = slices.Clone(end.winners)
	g.recordEvent(roundEvent{kind: roundEndEvent, end: &end})
}

func (end roundEnd) wire() map[string]any {
	kinds := [...]string{draw: "draw", discardHu: "discard_hu", selfHu: "self_hu", kongFlower: "kong_flower", eightFlowers: "eight_flowers"}
	winners := make([]any, 0, len(end.winners))
	for _, winner := range end.winners {
		winners = append(winners, map[string]any{"seat": int64(winner.seat), "point": int64(winner.point)})
	}
	payload := map[string]any{"kind": kinds[end.kind], "rob_kong": end.robbedKong, "winners": winners}
	if end.kind == discardHu || end.kind == kongFlower {
		payload["source_seat"] = int64(end.source)
	}
	if end.tile != 0 {
		payload["tile"] = int64(end.tile)
	}
	return payload
}

func (g *round) recordTurn(state turnState, now time.Time) {
	notice := &turnNotice{state: state, serverMS: now.UnixMilli()}
	if state.phase == "respond" {
		for seat := range seatCount {
			notice.offers[seat] = g.offersFor(seat)
		}
	}
	if state.self {
		notice.canWin, notice.concealed, notice.added = g.selfOptions(state.active)
	}
	g.recordEvent(roundEvent{kind: turnEvent, operation: notice})
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
	case meldEvent:
		payload["meld_index"], payload["meld"] = int64(event.meld.index), event.meld.value.wire()
		return accessagent.DhmjMeldPushCommand, payload
	case roundEndEvent:
		delete(payload, "seat")
		payload["round_end"] = event.end.wire()
		return accessagent.DhmjRoundEndPushCommand, payload
	case turnEvent:
		n := event.operation
		s := n.state
		payload["seat"], payload["phase"], payload["active_seat"] = int64(recipient), s.phase, int64(s.active)
		payload["turn"], payload["window_id"], payload["window_tile"], payload["rob_kong"] = s.turn, s.windowID, int64(s.windowTile), s.robKong
		payload["server_ms"], payload["deadline_ms"] = n.serverMS, int64(0)
		if !s.deadline.IsZero() {
			payload["deadline_ms"] = s.deadline.UnixMilli()
		}
		payload["offers"] = wireOffers(n.offers[recipient])
		if recipient == s.active {
			payload["can_discard"], payload["can_self_win"] = s.discard, n.canWin
			payload["concealed_kongs"], payload["added_kongs"] = wireTiles(n.concealed), wireTiles(n.added)
		}
		return accessagent.DhmjTurnPushCommand, payload
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
