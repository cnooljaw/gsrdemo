package main

import (
	"math/rand/v2"
	"time"

	"github.com/cnooljaw/gsr"
)

func (service *attackService) bid(self gsr.CommandContext, uid uint64, login string, turn uint64, yes bool) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	if !time.Now().Before(service.game.Deadline) {
		return nil, errLandlordTurn
	}
	if err := service.applyBid(self, uid, turn, yes); err != nil {
		return nil, err
	}
	service.broadcast(self)
	return service.view(uid), nil
}

func (service *attackService) applyBid(self gsr.ServiceContext, uid, turn uint64, yes bool) error {
	game := &service.game
	var deck []int64
	// 先准备新牌，随机源出错时不接纳这次导致重发牌的决定。
	if game.Phase == "calling" && !yes && game.Redeals < 2 && game.Calls[(game.Seat+1)%3] == -1 && game.Calls[(game.Seat+2)%3] == -1 {
		var err error
		deck, err = shuffledDeck()
		if err != nil {
			return err
		}
	}
	redeal, err := game.bid(uid, turn, yes, time.Now())
	if err != nil {
		return err
	}
	if redeal {
		game.deal(deck, rand.IntN(3), time.Now())
	}
	return nil
}

func (service *attackService) handleCallLandlord(self gsr.CommandContext, uid uint64, login string, turn uint64, yes bool) (map[string]any, error) {
	if service.game.Phase != "calling" {
		return nil, errLandlordTurn
	}
	return service.bid(self, uid, login, turn, yes)
}

func (service *attackService) handleRobLandlord(self gsr.CommandContext, uid uint64, login string, turn uint64, yes bool) (map[string]any, error) {
	if service.game.Phase != "robbing" {
		return nil, errLandlordTurn
	}
	return service.bid(self, uid, login, turn, yes)
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
