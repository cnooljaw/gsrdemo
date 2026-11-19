package main

import (
	"slices"
	"time"
)

const trusteeDiscardDelay = 300 * time.Millisecond
const trusteeSelfDelay = time.Second

func (g *round) setTrustee(seat int, enabled bool, now time.Time) bool {
	if !validSeat(seat) || g.end != nil {
		return false
	}
	if g.trustees[seat] == enabled {
		return true
	}
	g.trustees[seat] = enabled
	if g.window != nil {
		if enabled && now.Before(g.deadline) && g.window.submit(seat, pass) {
			g.resolveResponse(now)
		}
	} else if seat == g.active {
		g.scheduleTrustee(now)
	}
	return true
}

func (g *round) scheduleTrustee(now time.Time) {
	g.trusteeAt = time.Time{}
	if !g.trustees[g.active] || g.deadline.IsZero() {
		return
	}
	delay := trusteeDiscardDelay
	if g.selfPending {
		delay = trusteeSelfDelay
	}
	g.trusteeAt = now.Add(delay)
}

func (g *round) dueAt() time.Time {
	if !g.trusteeAt.IsZero() && g.trusteeAt.Before(g.deadline) {
		return g.trusteeAt
	}
	return g.deadline
}

// tick 推进当前已到期的动作，每次最多推进一个到期边界。
func (g *round) tick(now time.Time) bool {
	if g.end != nil {
		return false
	}
	if g.window != nil {
		return g.expireResponse(now)
	}
	if g.selfPending {
		return g.expireSelf(now)
	}
	due := g.dueAt()
	if due.IsZero() || now.Before(due) {
		return false
	}

	seat := g.active
	hand := g.hands[seat]
	tile := g.autoDiscardTile(hand)
	index := slices.Index(hand, tile)
	if index < 0 {
		return false
	}
	if !g.trustees[seat] && g.afterClaim == unanswered {
		g.discardTimeouts[seat]++
		if g.discardTimeouts[seat] == 2 {
			g.trustees[seat] = true
		}
	}
	g.applyDiscard(seat, index, now)
	return true
}

func (g *round) autoDiscardTile(hand []byte) byte {
	if g.afterClaim == unanswered && g.lastDraw != 0 {
		return g.lastDraw
	}
	var highest, allowed byte
	for _, tile := range hand {
		if tile >= 0x41 {
			continue
		}
		highest = max(highest, tile)
		if g.allowedAutoDiscard(tile) {
			allowed = max(allowed, tile)
		}
	}
	if allowed != 0 {
		return allowed
	}
	return highest
}

func (g *round) allowedAutoDiscard(tile byte) bool {
	claimed := g.claimedTile
	switch g.afterClaim {
	case eatRight:
		return tile != claimed && tile != claimed+3
	case eatMiddle:
		return tile != claimed
	case eatLeft:
		return tile != claimed && tile != claimed-3
	default:
		return true
	}
}
