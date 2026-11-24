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
	if !enabled {
		g.discardTimeouts[seat] = 0
	}
	g.recordTrustee(seat, enabled)
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

// resumeManualControl 保留未到期的人工轮次，即使托管的自动执行时间已到。
func (g *round) resumeManualControl(seat int, now time.Time) {
	if !g.deadline.IsZero() && !now.Before(g.deadline) {
		g.tick(now)
	}
	if g.end == nil {
		g.discardTimeouts[seat] = 0
		g.setTrustee(seat, false, now)
	}
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
			g.recordTrustee(seat, true)
		}
	}
	g.applyDiscard(seat, index, now)
	return true
}

func (g *round) recordTrustee(seat int, enabled bool) {
	g.trustees[seat] = enabled
	g.recordEvent(roundEvent{kind: trusteeEvent, seat: seat, trustee: enabled})
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
