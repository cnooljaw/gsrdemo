package main

import "time"

// turnState 描述本次推进后生效的操作阶段，不记录单个玩家已提交的选择。
type turnState struct {
	phase          string
	active         int
	turn, windowID uint64
	windowTile     byte
	robKong        bool
	deadline       time.Time
	discard, self  bool
}

func (g *round) currentTurn() turnState {
	state := turnState{active: g.active, turn: g.turn}
	if g.end != nil {
		state.phase = "finished"
		return state
	}
	state.deadline = g.dueAt()
	if w := g.window; w != nil {
		state.phase = "respond"
		state.windowID, state.windowTile, state.robKong = g.windowID, w.tile, w.robKong
		state.deadline = g.deadline
		return state
	}
	state.phase = "discard"
	state.discard = g.mayDiscard(g.active)
	if g.selfPending {
		state.phase = "self"
		state.self = !g.trustees[g.active]
	}
	return state
}

func (g *round) selfOptions(seat int) (bool, []byte, []byte) {
	hand := g.hands[seat]
	var concealed, added []byte
	var seen [256]bool
	for _, tile := range hand {
		if tile >= 0x41 || seen[tile] {
			continue
		}
		seen[tile] = true
		if tileCount(hand, tile) == 4 {
			concealed = append(concealed, tile)
		}
		if g.pongIndex(seat, tile) >= 0 {
			added = append(added, tile)
		}
	}
	return canHu(hand), concealed, added
}

func (g *round) offersFor(seat int) actionOptions {
	if g.window != nil && !g.trustees[seat] && g.window.choices[seat] == unanswered {
		return g.window.offers[seat]
	}
	return 0
}

func wireOffers(options actionOptions) []any {
	offers := make([]any, 0)
	for action := eatRight; action <= claimHu; action++ {
		if options.has(action) {
			offers = append(offers, int64(action))
		}
	}
	return offers
}
