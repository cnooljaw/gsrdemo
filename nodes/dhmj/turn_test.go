package main

import (
	"slices"
	"testing"
	"time"
)

// TestDiscardTurn 保护出牌轮次、成员手牌与响应窗截止校验，以及碰后轮到碰牌者出牌。
func TestDiscardTurn(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {5, 5, 8, 9}}}
	g.allowDiscard(now)
	if g.discard(1, 1, 5, now) || g.discard(0, 0, 5, now) || g.discard(0, 1, 4, now) ||
		g.discard(0, 1, 5, now.Add(discardPeriod)) {
		t.Fatal("非法出牌被接受")
	}
	if !g.discard(0, 1, 5, now) || g.window == nil || !slices.Equal(g.out[0], []byte{5}) {
		t.Fatal("未打开响应窗口")
	}
	id := g.windowID
	if g.respond(1, id-1, pong, now) || g.respond(1, id, pong, now.Add(responsePeriod)) ||
		g.discard(0, 1, 9, now) {
		t.Fatal("旧窗口、到期回复或窗口内再次出牌被接受")
	}
	if !g.respond(1, id, pong, now) || g.window != nil || g.active != 1 || g.turn != 2 ||
		!slices.Equal(g.hands[1], []byte{8, 9}) || len(g.out[0]) != 0 ||
		len(g.melds[1]) != 1 || g.melds[1][0].action != pong || g.melds[1][0].source != 0 ||
		!slices.Equal(g.melds[1][0].tiles, []byte{5, 5, 5}) {
		t.Fatalf("碰牌后的状态有误：%+v", g)
	}
}

// TestResponseExpiryKeepsConfirmedWins 保护多胡窗口到期仅放弃未答复的座位。
func TestResponseExpiryKeepsConfirmedWins(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {3, 4, 7, 7}, {6, 7, 9, 9}, {4, 6, 1, 1}}}
	g.allowDiscard(now)
	if !g.discard(0, 1, 5, now) || g.window == nil ||
		!g.respond(1, g.windowID, claimHu, now) || !g.respond(2, g.windowID, claimHu, now) {
		t.Fatal("多胡操作未接纳")
	}
	if g.end != nil || g.expireResponse(now.Add(responsePeriod-time.Nanosecond)) {
		t.Fatal("提前结束窗口")
	}
	if !g.expireResponse(now.Add(responsePeriod)) || g.end == nil || g.end.kind != discardHu ||
		len(g.end.winners) != 2 || g.end.winners[0].seat != 1 || g.end.winners[1].seat != 2 ||
		g.expireResponse(now.Add(responsePeriod)) {
		t.Fatalf("到期未保留两名已确认胡牌者：%+v", g.end)
	}
}

// TestResponseAppliesChowKongAndPass 保护吃牌副露、直杠补摸、全过转下一家。
func TestResponseAppliesChowKongAndPass(t *testing.T) {
	now := time.Unix(100, 0)
	for _, tc := range []struct {
		action responseAction
		hand   []byte
		meld   []byte
		want   []byte
	}{
		{eatLeft, []byte{6, 7, 8, 9}, []byte{5, 6, 7}, []byte{8, 9}},
		{directKong, []byte{5, 5, 5, 8}, []byte{5, 5, 5, 5}, []byte{8, 0x34}},
		{pass, []byte{5, 5, 8, 9}, nil, []byte{5, 5, 8, 9, 0x34}},
	} {
		g := round{active: 0, wall: []byte{0x34}, hands: [4][]byte{{5, 9}, tc.hand}}
		g.hands[1] = slices.Clone(tc.hand)
		g.allowDiscard(now)
		if !g.discard(0, 1, 5, now) || !g.respond(1, g.windowID, tc.action, now) {
			t.Fatalf("无法执行动作 %d", tc.action)
		}
		if g.active != 1 || g.turn != 2 || !slices.Equal(g.hands[1], tc.want) {
			t.Fatalf("动作 %d 后的牌桌状态有误：%+v", tc.action, g)
		}
		if tc.meld == nil {
			if len(g.melds[1]) != 0 || len(g.out[0]) != 1 {
				t.Fatal("过不应消费弃牌")
			}
		} else if len(g.melds[1]) != 1 || !slices.Equal(g.melds[1][0].tiles, tc.meld) || len(g.out[0]) != 0 {
			t.Fatal("副露未消费弃牌")
		}
	}
}

// TestFirstFlowerRecheck 保护下家补起手花后可响应原弃牌，新窗口拒绝旧回复。
func TestFirstFlowerRecheck(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, wall: []byte{5, 0x34}, hands: [4][]byte{{5, 9}, {5, 0x41, 8, 9}}}
	g.allowDiscard(now)
	if !g.discard(0, 1, 5, now) || g.window == nil || g.windowID != 2 ||
		!g.window.offers[1].has(pong) || g.respond(1, 1, pong, now) {
		t.Fatal("补花未重开窗口，或旧窗口仍可操作")
	}
	if !g.respond(1, 2, pass, now) || g.window != nil || g.active != 1 || g.remaining() != 0 ||
		!slices.Equal(g.hands[1], []byte{5, 5, 8, 9, 0x34}) {
		t.Fatalf("补花后选择过未正确摸牌：%+v", g)
	}
}
