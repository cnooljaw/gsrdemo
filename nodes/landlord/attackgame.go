package main

import (
	"math/rand/v2"
	"slices"
	"time"
)

const turnPeriod = 15 * time.Second
const bidPeriod = 10 * time.Second
const baseSilver int64 = 10

// 一副牌最多 14 次炸弹/王炸，叫抢最多 3 次；为倍率与金额建立有限上界。
const maxMultiplier int64 = 1 << 17

type landlordPlayer struct {
	UserID                                     uint64
	LoginID, AccessNode, AgentName, Membership string
	Ready                                      bool
	Hand                                       []int64
}

// attackGame 是本桌私有值对象，只归 AttackService 管理。
type attackGame struct {
	Name, Phase                          string
	Players                              [3]landlordPlayer
	Bottom, Last                         []int64
	Seat, LastSeat, Passes, Winner       int
	Landlord, Caller, Candidate, Redeals int
	Calls, Robs                          [3]int64
	Multiplier                           int64
	Turn                                 uint64
	Deadline                             time.Time
}

func newAttackGame(name string) attackGame {
	return attackGame{Name: name, Phase: "waiting", LastSeat: -1, Winner: -1, Landlord: -1, Caller: -1, Candidate: -1, Multiplier: 1}
}

// deal 不重置 Turn；旧手牌的迟到叫抢不能作用到重发后的牌。
func (game *attackGame) deal(deck []int64, first int, now time.Time) {
	for i := range game.Players {
		game.Players[i].Hand = slices.Clone(deck[i*17 : (i+1)*17])
		sortHand(game.Players[i].Hand)
	}
	game.Bottom = slices.Clone(deck[51:])
	game.Last = nil
	game.Calls, game.Robs = [3]int64{}, [3]int64{}
	game.Landlord, game.Caller, game.Candidate, game.LastSeat, game.Winner = -1, -1, -1, -1, -1
	game.Multiplier, game.Passes = 1, 0
	game.Phase, game.Seat, game.Deadline = "calling", first, now.Add(bidPeriod)
	game.Turn++
}

func (game *attackGame) member(uid uint64, login string) (int, error) {
	for i, p := range game.Players {
		if p.UserID == uid && p.LoginID == login && uid != 0 {
			return i, nil
		}
	}
	return -1, errLandlordStale
}

func (game *attackGame) join(uid uint64, login, node, agent, membership string) error {
	seat := -1
	for i, p := range game.Players {
		if p.UserID == uid {
			seat = i
			break
		}
	}
	if seat < 0 {
		if game.Phase != "waiting" {
			return errLandlordBusy
		}
		for i, p := range game.Players {
			if p.UserID == 0 {
				seat = i
				break
			}
		}
	}
	if seat < 0 {
		return errLandlordBusy
	}
	player := &game.Players[seat]
	player.UserID, player.LoginID, player.AccessNode, player.AgentName, player.Membership = uid, login, node, agent, membership
	return nil
}

func (game *attackGame) readySeat(uid uint64, login string) (int, error) {
	seat, err := game.member(uid, login)
	if err != nil {
		return -1, err
	}
	if game.Phase != "waiting" {
		return -1, errLandlordBusy
	}
	return seat, nil
}

func (game *attackGame) markReady(seat int) bool {
	game.Players[seat].Ready = true
	for _, player := range game.Players {
		if player.UserID == 0 || !player.Ready {
			return false
		}
	}
	return true
}

func (game *attackGame) detach(seat int) {
	game.Players[seat].AccessNode, game.Players[seat].AgentName = "", ""
	if game.Phase == "waiting" {
		game.Players[seat].Ready = false
	}
}

func (game *attackGame) remove(seat int) bool {
	game.Players[seat] = landlordPlayer{}
	for _, player := range game.Players {
		if player.UserID != 0 {
			return false
		}
	}
	return true
}

// timeout 只推进已到期的游戏规则；外部通知与结算由 Service 编排。
func (game *attackGame) timeout(now time.Time) (bool, error) {
	if now.Before(game.Deadline) {
		return false, nil
	}
	switch game.Phase {
	case "calling", "robbing":
		return true, game.applyBid(game.Players[game.Seat].UserID, game.Turn, false, now)
	case "playing":
		return true, game.play(game.Players[game.Seat].UserID, game.Turn, game.defaultPlay(), now)
	}
	return false, nil
}

func (game *attackGame) applyBid(uid, turn uint64, yes bool, now time.Time) error {
	var deck []int64
	// 先准备新牌，随机源出错时不接纳这次导致重发牌的决定。
	if game.Phase == "calling" && !yes && game.Redeals < 2 && game.Calls[(game.Seat+1)%3] == -1 && game.Calls[(game.Seat+2)%3] == -1 {
		var err error
		deck, err = shuffledDeck()
		if err != nil {
			return err
		}
	}
	redeal, err := game.bid(uid, turn, yes, now)
	if err != nil {
		return err
	}
	if redeal {
		game.deal(deck, rand.IntN(3), now)
	}
	return nil
}

func (game *attackGame) bid(uid, turn uint64, yes bool, now time.Time) (bool, error) {
	if (game.Phase != "calling" && game.Phase != "robbing") || game.Turn != turn || game.Players[game.Seat].UserID != uid {
		return false, errLandlordTurn
	}
	seat := game.Seat
	if game.Phase == "calling" {
		if !yes {
			game.Calls[seat] = -1
			game.Turn++
			if game.Calls == [3]int64{-1, -1, -1} {
				if game.Redeals < 2 {
					game.Redeals++
					return true, nil
				}
				game.Phase = "waiting"
				game.Redeals = 0
				game.Bottom = nil
				game.Deadline = time.Time{}
				for i := range game.Players {
					game.Players[i].Ready = false
					game.Players[i].Hand = nil
				}
				return false, nil
			}
			game.Seat = (seat + 1) % 3
			game.Deadline = now.Add(bidPeriod)
			return false, nil
		}
		game.Calls[seat] = 1
		game.Caller, game.Candidate = seat, seat
		game.Phase = "robbing"
	} else {
		if yes {
			if game.Multiplier > maxMultiplier/2 {
				return false, errLandlordInput
			}
			game.Multiplier *= 2
			game.Candidate = seat
			game.Robs[seat] = 1
		} else {
			game.Robs[seat] = -1
		}
	}
	game.Turn++
	for offset := 1; offset <= 3; offset++ {
		next := (seat + offset) % 3
		if game.Calls[next] == -1 || game.Robs[next] != 0 {
			continue
		}
		if next == game.Caller && game.Candidate == game.Caller {
			continue
		}
		game.Seat = next
		game.Deadline = now.Add(bidPeriod)
		return false, nil
	}
	game.Landlord = game.Candidate
	player := &game.Players[game.Landlord]
	player.Hand = append(player.Hand, game.Bottom...)
	sortHand(player.Hand)
	game.Seat, game.Phase, game.Deadline = game.Landlord, "playing", now.Add(turnPeriod)
	return false, nil
}

func (game *attackGame) play(userID, turn uint64, cards []int64, now time.Time) error {
	if game.Phase != "playing" || game.Players[game.Seat].UserID != userID || game.Turn != turn {
		return errLandlordTurn
	}
	hand := game.Players[game.Seat].Hand
	if len(cards) == 0 {
		if len(game.Last) == 0 || game.LastSeat == game.Seat {
			return errLandlordCards
		}
	} else {
		pattern := classify(cards)
		if !beats(pattern, classify(game.Last)) {
			return errLandlordCards
		}
		remaining := slices.Clone(hand)
		for _, card := range cards {
			i := slices.Index(remaining, card)
			if i < 0 {
				return errLandlordCards
			}
			remaining = slices.Delete(remaining, i, i+1)
		}
		if pattern.Kind == "bomb" || pattern.Kind == "rocket" {
			if game.Multiplier > maxMultiplier/2 {
				return errLandlordInput
			}
			game.Multiplier *= 2
		}
		game.Players[game.Seat].Hand = remaining
		game.Last, game.LastSeat, game.Passes = slices.Clone(cards), game.Seat, 0
		if len(remaining) == 0 {
			game.Phase, game.Winner = "finished", game.Seat
			game.Turn++
			return nil
		}
	}
	if len(cards) == 0 {
		game.Passes++
		if game.Passes == 2 {
			game.Last, game.LastSeat, game.Passes = nil, -1, 0
		}
	}
	game.Seat = (game.Seat + 1) % 3
	game.Turn++
	game.Deadline = now.Add(turnPeriod)
	return nil
}

func (game *attackGame) defaultPlay() []int64 {
	if len(game.Last) > 0 {
		return nil
	}
	return []int64{game.Players[game.Seat].Hand[0]}
}

func cardList(cards []int64) []any {
	values := make([]any, len(cards))
	for i, c := range cards {
		values[i] = c
	}
	return values
}

func (game *attackGame) view(userID uint64) map[string]any {
	players := make([]any, 0, 3)
	hand := []any{}
	seat := int64(-1)
	for i, p := range game.Players {
		if p.UserID == 0 {
			continue
		}
		players = append(players, map[string]any{"user_id": p.UserID, "seat": int64(i), "ready": p.Ready, "remaining": int64(len(p.Hand)), "call_choice": game.Calls[i], "rob_choice": game.Robs[i]})
		if p.UserID == userID {
			seat = int64(i)
			hand = cardList(p.Hand)
		}
	}
	bottom := []any{}
	if game.Landlord >= 0 {
		bottom = cardList(game.Bottom)
	}
	deadline := int64(0)
	if !game.Deadline.IsZero() {
		deadline = game.Deadline.UnixMilli()
	}
	return map[string]any{"room_name": game.Name, "phase": game.Phase, "seat": seat, "players": players, "hand": hand,
		"bottom": bottom, "last_cards": cardList(game.Last), "last_seat": int64(game.LastSeat), "turn_seat": int64(game.Seat),
		"turn": game.Turn, "deadline_ms": deadline, "landlord_seat": int64(game.Landlord), "candidate_seat": int64(game.Candidate), "multiplier": game.Multiplier, "redeals": int64(game.Redeals), "winner_seat": int64(game.Winner)}
}
