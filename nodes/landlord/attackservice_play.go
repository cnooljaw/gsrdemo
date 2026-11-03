package main

import (
	"slices"
	"time"

	"github.com/cnooljaw/gsr"
)

func (service *attackService) handlePlay(self gsr.CommandContext, uid uint64, login string, turn uint64, values []any) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	cards := make([]int64, len(values))
	if len(values) > 20 {
		return nil, errLandlordCards
	}
	for i, v := range values {
		card, ok := v.(int64)
		if !ok {
			return nil, errLandlordCards
		}
		cards[i] = card
	}
	now := time.Now()
	if !now.Before(service.game.Deadline) {
		return nil, errLandlordTurn
	}
	if err := service.game.play(uid, turn, cards, now); err != nil {
		return nil, err
	}
	service.finishGame()
	service.broadcast(self)
	return service.view(uid), nil
}

func (service *attackService) finishGame() {
	if service.game.Phase == "finished" && service.game.Winner >= 0 && service.result == nil {
		service.result = resultForGame(service.game)
	}
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
