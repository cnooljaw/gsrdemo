package main

import (
	"reflect"
	"testing"
	"time"
)

func testBidding(t *testing.T) (*attackGame, []int64) {
	t.Helper()
	deck, err := shuffledDeck()
	if err != nil {
		t.Fatal(err)
	}
	game := newAttackGame("bid-test")
	for i := range game.Players {
		game.Players[i] = landlordPlayer{UserID: uint64(i + 1), LoginID: "login", Ready: true}
	}
	game.deal(deck, 0, time.Now())
	return &game, deck
}

// TestClassicBidding 保护经典叫抢顺序、跳过不叫者、首叫者最终抢回机会及每次抢翻倍。
func TestClassicBidding(t *testing.T) {
	cases := []struct {
		name       string
		choices    []bool
		landlord   int
		multiplier int64
	}{
		{"call_no_no", []bool{true, false, false}, 0, 1},
		{"call_rob_no_reclaim", []bool{true, true, false, true}, 0, 4},
		{"call_rob_no_giveup", []bool{true, true, false, false}, 1, 2},
		{"no_call_rob_reclaim", []bool{false, true, true, true}, 1, 4},
		{"no_no_call", []bool{false, false, true}, 2, 1},
		{"all_rob", []bool{true, true, true, true}, 0, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, _ := testBidding(t)
			for _, choice := range c.choices {
				uid := g.Players[g.Seat].UserID
				if redeal, err := g.bid(uid, g.Turn, choice, time.Now()); err != nil || redeal {
					t.Fatalf("bid: %v %v", redeal, err)
				}
			}
			if g.Phase != "playing" || g.Landlord != c.landlord || g.Multiplier != c.multiplier || g.Seat != c.landlord {
				t.Fatalf("unexpected result: %+v", g)
			}
			for i, p := range g.Players {
				want := 17
				if i == g.Landlord {
					want = 20
				}
				if len(p.Hand) != want {
					t.Fatal("底牌分配错误")
				}
			}
		})
	}
}

// TestBiddingRedealAndStaleCommands 保护底牌隐藏、三轮无人叫终止无账务，旧轮次跨重发牌也不能生效。
func TestBiddingRedealAndStaleCommands(t *testing.T) {
	g, deck := testBidding(t)
	oldTurn := g.Turn
	if len(g.view(1)["bottom"].([]any)) != 0 {
		t.Fatal("叫抢前泄露底牌")
	}
	for round := 0; round < 3; round++ {
		for i := 0; i < 3; i++ {
			redeal, err := g.bid(g.Players[g.Seat].UserID, g.Turn, false, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if redeal {
				g.deal(deck, (round+1)%3, time.Now())
			}
		}
	}
	if g.Phase != "waiting" || g.Winner != -1 || len(g.Bottom) != 0 {
		t.Fatal("无人叫时不应强行开局或产生胜负")
	}
	before := g.view(1)
	if _, err := g.bid(1, oldTurn, true, time.Now()); err == nil {
		t.Fatal("迟到的叫抢应被拒绝")
	}
	if !reflect.DeepEqual(before, g.view(1)) {
		t.Fatal("旧请求修改了状态")
	}
}

// TestBiddingRejectsWrongActor 保护叫抢时非当前玩家和重复轮次不修改任何可见状态。
func TestBiddingRejectsWrongActor(t *testing.T) {
	g, _ := testBidding(t)
	before := g.view(1)
	if _, err := g.bid(2, g.Turn, true, time.Now()); err == nil {
		t.Fatal("越权叫抢")
	}
	if !reflect.DeepEqual(before, g.view(1)) {
		t.Fatal("非法叫抢修改状态")
	}
	turn := g.Turn
	if _, err := g.bid(1, turn, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	before = g.view(1)
	if _, err := g.bid(1, turn, true, time.Now()); err == nil {
		t.Fatal("重复叫抢")
	}
	if !reflect.DeepEqual(before, g.view(1)) {
		t.Fatal("重复请求修改倍率或回合")
	}
}
