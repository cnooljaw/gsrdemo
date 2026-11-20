package main

import (
	"slices"
	"time"
)

func (g *round) drawForTurn(now time.Time, kong bool, provider int) {
	g.afterClaim = unanswered
	drawn := g.drawActive()
	g.recordFlowers(g.active, drawn.flowers, true)
	if drawn.playable == 0 {
		return
	}
	// 原版摸花后补到胡牌张也按自家杠上开花计台。
	if len(drawn.flowers) != 0 {
		kong, provider = true, g.active
	}
	g.openDrawnTurn(now, drawn.playable, kong, provider)
	g.recordEvent(roundEvent{kind: drawEvent, seat: g.active, turn: g.turn, tile: drawn.playable})
}

func (g *round) openDrawnTurn(now time.Time, tile byte, kong bool, provider int) {
	g.lastDraw, g.kongBloom, g.kongProvider = tile, kong, provider
	g.allowDiscard(now)
	g.selfPending = g.hasSelfAction()
	if g.selfPending {
		g.deadline = now.Add(responsePeriod)
		g.scheduleTrustee(now)
	}
}

func (g *round) hasSelfAction() bool {
	hand := g.hands[g.active]
	if canHu(hand) {
		return true
	}
	for _, tile := range hand {
		if tile < 0x41 && (tileCount(hand, tile) == 4 || g.pongIndex(g.active, tile) >= 0) {
			return true
		}
	}
	return false
}

func (g *round) canChooseSelf(seat int, turn uint64, now time.Time) bool {
	return g.end == nil && g.window == nil && g.selfPending && seat == g.active && !g.trustees[seat] &&
		turn == g.turn && now.Before(g.deadline)
}

func (g *round) selfWin(seat int, turn uint64, now time.Time) bool {
	if !g.canChooseSelf(seat, turn, now) || !canHu(g.hands[seat]) {
		return false
	}
	hand := slices.Clone(g.hands[seat])
	index := slices.Index(hand, g.lastDraw)
	if index == -1 {
		return false
	}
	hand = slices.Delete(hand, index, index+1)
	kind := selfHu
	if g.kongBloom && g.kongProvider != seat {
		kind = kongFlower
	}
	point := g.recordWin(seat, hand, g.lastDraw, g.winds[seat], kind, g.kongBloom, false)
	g.end = &roundEnd{kind: kind, dealer: g.dealer, source: g.kongProvider, tile: g.lastDraw, winners: []hu{{seat, point}}, claims: g.claimCounts()}
	g.selfPending = false
	g.deadline = time.Time{}
	return true
}

func (g *round) passSelf(seat int, turn uint64, now time.Time) bool {
	if !g.canChooseSelf(seat, turn, now) {
		return false
	}
	g.closeSelfWindow(now)
	return true
}

func (g *round) expireSelf(now time.Time) bool {
	if g.end != nil || g.window != nil || !g.selfPending || now.Before(g.dueAt()) {
		return false
	}
	g.closeSelfWindow(now)
	return true
}

func (g *round) closeSelfWindow(now time.Time) {
	g.selfPending = false
	g.allowDiscard(now)
}

func (g *round) declareKong(seat int, turn uint64, tile byte, now time.Time) bool {
	if !g.canChooseSelf(seat, turn, now) || tile >= 0x41 || !slices.Contains(g.hands[seat], tile) {
		return false
	}
	if tileCount(g.hands[seat], tile) == 4 {
		g.selfPending = false
		for range 4 {
			g.removeFromHand(seat, tile)
		}
		g.melds[seat] = append(g.melds[seat], meld{concealedKong, seat, []byte{tile, tile, tile, tile}})
		g.recordMeld(seat, len(g.melds[seat])-1)
		g.deadline = time.Time{}
		g.drawForTurn(now, true, seat)
		return true
	}
	if g.pongIndex(seat, tile) < 0 {
		return false
	}

	// 抢杠窗口开始前只登记意图，原碰与第四张牌都留在原处。
	g.selfPending = false
	w := discardWindow{source: seat, tile: tile, robKong: true}
	for other, hand := range g.hands {
		if other != seat && canHu(append(slices.Clone(hand), tile)) {
			w.offers[other].add(claimHu)
		}
	}
	g.openResponse(w, now)
	return true
}

func (g *round) pongIndex(seat int, tile byte) int {
	for i, m := range g.melds[seat] {
		if m.action == pong && m.tiles[0] == tile {
			return i
		}
	}
	return -1
}

func (g *round) completeAddedKong(w discardWindow, now time.Time) {
	index := g.pongIndex(w.source, w.tile)
	m := &g.melds[w.source][index]
	g.removeFromHand(w.source, w.tile)
	m.action = addedKong
	m.tiles = append(m.tiles, w.tile)
	g.recordMeld(w.source, index)
	g.drawForTurn(now, true, w.source)
}

func (g *round) removeFromHand(seat int, tile byte) {
	index := slices.Index(g.hands[seat], tile)
	g.hands[seat] = slices.Delete(g.hands[seat], index, index+1)
}

func (g *round) recordWin(seat int, hand []byte, tile, wind byte, kind endKind, bloom, rob bool) int {
	var visible []byte
	for _, m := range g.melds[seat] {
		visible = append(visible, m.tiles...)
	}
	points := scoreWin(hand, visible, tile, winContext{
		kind: kind, activeWind: wind, roundWind: g.roundWind,
		flowers: g.flowers[seat], dealer: seat == g.dealer,
		discarded: len(g.out[seat]) > 0, lastTile: g.remaining() == 0,
		kongBloom: bloom, robbedKong: rob,
	})
	g.winDetail[seat] = points
	return points.total()
}
