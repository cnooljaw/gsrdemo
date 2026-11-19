package dhmj

import (
	"slices"
	"time"
)

const discardPeriod = 20 * time.Second
const responsePeriod = 10 * time.Second

type meld struct {
	action responseAction
	source int
	tiles  []byte
}

func (g *round) startPlay(now time.Time) {
	if g.turn != 0 || g.end != nil {
		return
	}
	g.replaceInitialFlowers(g.dealer)
	g.drawTurn(now)
}

func (g *round) allowDiscard(now time.Time) {
	g.turn++
	g.deadline = now.Add(discardPeriod)
}

func (g *round) drawTurn(now time.Time) {
	g.drawForTurn(now, false, g.active)
}

func (g *round) discard(seat int, turn uint64, tile byte, now time.Time) bool {
	if !validSeat(seat) || seat != g.active || turn == 0 || turn != g.turn || g.end != nil ||
		g.window != nil || !now.Before(g.deadline) || len(g.hands[seat])%3 != 2 || tile >= 0x41 {
		return false
	}
	index := slices.Index(g.hands[seat], tile)
	if index < 0 {
		return false
	}
	g.hands[seat] = slices.Delete(g.hands[seat], index, index+1)
	g.selfPending, g.lastDraw = false, 0
	g.out[seat] = append(g.out[seat], tile)
	g.openResponse(newDiscardWindow(g.hands, seat, tile), now)
	return true
}

func (g *round) openResponse(w discardWindow, now time.Time) {
	g.windowID++
	g.window = &w
	g.deadline = now.Add(responsePeriod)
	g.resolveResponse(now)
}

func (g *round) respond(seat int, windowID uint64, action responseAction, now time.Time) bool {
	if g.window == nil || g.windowID != windowID || !now.Before(g.deadline) || !g.window.submit(seat, action) {
		return false
	}
	g.resolveResponse(now)
	return true
}

func (g *round) expireResponse(now time.Time) bool {
	if g.window == nil || now.Before(g.deadline) {
		return false
	}
	for seat, offer := range g.window.offers {
		if offer != 0 && g.window.choices[seat] == unanswered {
			g.window.choices[seat] = pass
		}
	}
	g.resolveResponse(now)
	return true
}

func (g *round) resolveResponse(now time.Time) {
	actions, ready := g.window.resolve()
	if !ready {
		return
	}
	w := *g.window
	g.window = nil
	g.deadline = time.Time{}
	if len(actions) == 0 && w.robKong {
		g.completeAddedKong(w, now)
	} else if len(actions) == 0 {
		g.advanceAfterPass(w, now)
	} else if actions[0].action == claimHu {
		g.finishDiscardHu(w, actions)
	} else {
		g.takeDiscard(w, actions[0], now)
	}
}

func (g *round) advanceAfterPass(w discardWindow, now time.Time) {
	next := (w.source + 1) % seatCount
	if !g.drawn[next] {
		// 下家尚未首摸时先补起手花。补花后仅下家可再响应这张弃牌。
		flowers := g.replaceInitialFlowers(next)
		if g.end != nil {
			return
		}
		if len(flowers) > 0 {
			if offer := discardOptions(g.hands[next], w.tile, true); offer != 0 {
				recheck := discardWindow{source: w.source, tile: w.tile}
				recheck.offers[next] = offer
				g.openResponse(recheck, now)
				return
			}
		}
	}
	g.active = next
	g.drawTurn(now)
}

func (g *round) takeDiscard(w discardWindow, chosen response, now time.Time) {
	var used []byte
	switch chosen.action {
	case eatRight:
		used = []byte{w.tile - 2, w.tile - 1}
	case eatMiddle:
		used = []byte{w.tile - 1, w.tile + 1}
	case eatLeft:
		used = []byte{w.tile + 1, w.tile + 2}
	case pong:
		used = []byte{w.tile, w.tile}
	case directKong:
		used = []byte{w.tile, w.tile, w.tile}
	}
	seat := chosen.seat
	for _, tile := range used {
		index := slices.Index(g.hands[seat], tile)
		g.hands[seat] = slices.Delete(g.hands[seat], index, index+1)
	}
	g.out[w.source] = g.out[w.source][:len(g.out[w.source])-1]
	tiles := append(used, w.tile)
	slices.Sort(tiles)
	g.melds[seat] = append(g.melds[seat], meld{chosen.action, w.source, tiles})
	g.active = seat
	var flowers []flowerReplacement
	if !g.drawn[seat] {
		flowers = g.replaceInitialFlowers(seat)
	}
	if g.end != nil {
		return
	}
	if chosen.action == directKong {
		g.drawForTurn(now, true, w.source)
		return
	}
	if len(flowers) != 0 {
		// 原版吃碰后补起手花，会重新检查胡和自杠动作。
		hand := g.hands[seat]
		g.openDrawnTurn(now, hand[len(hand)-1], false, seat)
		return
	}
	g.allowDiscard(now)
}

func (g *round) claimCounts() (claims [seatCount][seatCount]int) {
	for seat, melds := range g.melds {
		for _, m := range melds {
			if m.source != seat {
				claims[seat][m.source]++
			}
		}
	}
	return claims
}

func (g *round) finishDiscardHu(w discardWindow, actions []response) {
	end := roundEnd{kind: discardHu, dealer: g.dealer, source: w.source, robbedKong: w.robKong, claims: g.claimCounts()}
	if w.robKong {
		g.removeFromHand(w.source, w.tile)
	}
	for _, action := range actions {
		seat := action.seat
		// 原版单胡先转到赢家，多胡仍保留出牌者为活动座位。
		wind := g.winds[w.source]
		if len(actions) == 1 {
			wind = g.winds[seat]
		}
		point := g.recordWin(seat, g.hands[seat], w.tile, wind, discardHu, false, w.robKong)
		end.winners = append(end.winners, hu{seat, point})
	}
	g.end = &end
}
