package dhmj

import "slices"

// 响应优先级沿用 baseattackd，数值仅在规则层内使用。
type responseAction uint8

const (
	unanswered responseAction = iota
	eatRight
	eatMiddle
	eatLeft
	directKong
	pong
	claimHu
	pass
)

type actionOptions uint8

func (o actionOptions) has(action responseAction) bool {
	return o&(1<<action) != 0
}

func (o *actionOptions) add(action responseAction) {
	*o |= 1 << action
}

type response struct {
	seat   int
	action responseAction
}

// discardWindow 保存一次出牌的可选动作与各座位提交的唯一选择。
type discardWindow struct {
	source  int
	tile    byte
	offers  [seatCount]actionOptions
	choices [seatCount]responseAction
}

func newDiscardWindow(hands [seatCount][]byte, source int, tile byte) discardWindow {
	w := discardWindow{source: source, tile: tile}
	for distance := 1; distance < seatCount; distance++ {
		seat := (source + distance) % seatCount
		w.offers[seat] = discardOptions(hands[seat], tile, distance == 1)
	}
	return w
}

func discardOptions(hand []byte, tile byte, nextSeat bool) actionOptions {
	var options actionOptions
	copies := tileCount(hand, tile)
	if copies >= 2 {
		options.add(pong)
	}
	if copies == 3 {
		options.add(directKong)
	}
	if canHu(append(slices.Clone(hand), tile)) {
		options.add(claimHu)
	}
	if !nextSeat || tile>>4 >= 3 {
		return options
	}
	rank := tile & 15
	if rank >= 3 && tileCount(hand, tile-2) > 0 && tileCount(hand, tile-1) > 0 {
		options.add(eatRight)
	}
	if rank >= 2 && rank <= 8 && tileCount(hand, tile-1) > 0 && tileCount(hand, tile+1) > 0 {
		options.add(eatMiddle)
	}
	if rank <= 7 && tileCount(hand, tile+1) > 0 && tileCount(hand, tile+2) > 0 {
		options.add(eatLeft)
	}
	return options
}

func (w *discardWindow) submit(seat int, action responseAction) bool {
	if !validSeat(seat) || w.offers[seat] == 0 || w.choices[seat] != unanswered {
		return false
	}
	if action != pass && !w.offers[seat].has(action) {
		return false
	}
	w.choices[seat] = action
	return true
}

// resolve 先看已提交的最高动作，再检查未答复者能否改变结果。
func (w *discardWindow) resolve() (result []response, ready bool) {
	best := unanswered
	bestDistance := seatCount
	for distance := 1; distance < seatCount; distance++ {
		seat := (w.source + distance) % seatCount
		action := w.choices[seat]
		if action == unanswered || action == pass || action < best {
			continue
		}
		if action > best {
			best, bestDistance = action, distance
			result = append(result[:0], response{seat, action})
		} else if action == claimHu {
			result = append(result, response{seat, action})
		}
	}
	for distance := 1; distance < seatCount; distance++ {
		seat := (w.source + distance) % seatCount
		if w.choices[seat] != unanswered {
			continue
		}
		for action := claimHu; action >= eatRight; action-- {
			if !w.offers[seat].has(action) {
				continue
			}
			if action > best || (action == best && (best == claimHu || distance < bestDistance)) {
				return nil, false
			}
			break
		}
	}
	return result, true
}
