package main

import (
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
