package main

import (
	"slices"
	"testing"
	"time"
)

// TestAddedKongWaitsForRob 保护补杠等待其他座位回应，抢胡时不升副露、不补摸。
func TestAddedKongWaitsForRob(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {3, 4, 7, 7}}, wall: []byte{0x34}}
	g.melds[0] = []meld{{action: pong, source: 2, tiles: []byte{5, 5, 5}}}
	g.openDrawnTurn(now, 9, false, 0)
	if !g.declareKong(0, g.turn, 5, now) || g.window == nil ||
		!g.window.offers[1].has(claimHu) || len(g.melds[0][0].tiles) != 3 ||
		g.remaining() != 1 || !slices.Equal(g.hands[0], []byte{5, 9}) {
		t.Fatal("补杠在抢杠窗口裁决前已修改牌桌")
	}
	if !g.respond(1, g.windowID, claimHu, now) || g.end == nil ||
		!g.end.robbedKong || g.end.source != 0 || len(g.melds[0][0].tiles) != 3 ||
		g.remaining() != 1 || g.winDetail[1].robKong != 1 || !slices.Equal(g.hands[0], []byte{9}) {
		t.Fatal("抢杠终局未保留原碰或误摸补牌")
	}
	if len(g.events) != 0 || g.end.tile != 5 {
		t.Fatalf("抢杠错误地广播副露变更或丢失胡牌张：%+v", g.events)
	}
}

// TestAddedKongAllPass 保护抢杠超时都过后升级原副露、保留供牌者并且只补摸一次。
func TestAddedKongAllPass(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {3, 4, 7, 7}}, wall: []byte{0x34}}
	g.melds[0] = []meld{{action: pong, source: 2, tiles: []byte{5, 5, 5}}}
	g.openDrawnTurn(now, 9, false, 0)
	if !g.declareKong(0, g.turn, 5, now) || !g.expireResponse(now.Add(responsePeriod)) ||
		g.window != nil || g.remaining() != 0 || !slices.Equal(g.hands[0], []byte{9, 0x34}) ||
		g.melds[0][0].action != addedKong || g.melds[0][0].source != 2 || len(g.melds[0][0].tiles) != 4 ||
		g.expireResponse(now.Add(responsePeriod)) {
		t.Fatalf("补杠超时落地失败：%+v", g)
	}
	if len(g.events) != 2 || g.events[0].kind != meldEvent || g.events[0].meld.index != 0 ||
		g.events[0].meld.value.action != addedKong || g.events[0].meld.value.source != 2 || g.events[1].kind != drawEvent {
		t.Fatalf("补杠未先更新原碰位置再补牌：%+v", g.events)
	}
}

// TestConcealedKongAndSelfHu 保护暗杠后补牌仍是本人回合，杠上开花优先计台。
func TestConcealedKongAndSelfHu(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 1, hands: [4][]byte{nil, {5, 5, 5, 5, 0x34}}, wall: []byte{0x34}}
	g.openDrawnTurn(now, 5, false, 1)
	turn := g.turn
	if g.declareKong(0, turn, 5, now) || g.declareKong(1, turn, 4, now) ||
		!g.declareKong(1, turn, 5, now) || g.turn == turn ||
		len(g.melds[1]) != 1 || g.melds[1][0].action != concealedKong {
		t.Fatal("暗杠没有合法地推进本座回合")
	}
	if g.selfWin(1, turn, now) || !g.selfWin(1, g.turn, now) || g.end.kind != selfHu ||
		g.winDetail[1].kongBloom != 2 || g.winDetail[1].selfDraw != 0 ||
		!slices.Equal(g.hands[1], []byte{0x34, 0x34}) {
		t.Fatal("杠后自摸没有正确结局或改变了摊牌暗手")
	}
	if len(g.events) != 2 || g.events[0].kind != meldEvent || g.events[1].kind != drawEvent || g.events[1].seat != 1 ||
		g.events[1].tile != 0x34 || g.events[1].turn != g.turn {
		t.Fatalf("暗杠后的补牌事件错误：%+v", g.events)
	}
}

// TestDirectKongBloomKeepsProvider 保护直杠补出的自摸保留供杠人，实际付款仍按直杠开花计算。
func TestDirectKongBloomKeepsProvider(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {5, 5, 5, 8}}, wall: []byte{8}}
	g.allowDiscard(now)
	if !g.discard(0, g.turn, 5, now) || !g.respond(1, g.windowID, directKong, now) ||
		!g.selfWin(1, g.turn, now) || g.end.kind != kongFlower || g.end.source != 0 ||
		g.winDetail[1].kongBloom != 2 {
		t.Fatal("直杠开花未保留正确的供杠座位")
	}
	got, err := roundPayments(roomScore{tier: 5, base: 100}, *g.end)
	if len(g.events) != 3 || g.events[0].kind != discardEvent || g.events[1].kind != meldEvent || g.events[2].kind != drawEvent ||
		g.events[1].meld.value.source != 0 || g.events[2].seat != 1 || g.events[2].revision != g.events[0].revision+2 {
		t.Fatalf("直杠补牌事件未接在弃牌之后：%+v", g.events)
	}
	if err != nil || !slices.Equal(got, []payment{{0, 1, 2500}}) {
		t.Fatalf("直杠开花付款错误：%v, %v", got, err)
	}
}

// TestSelfWindowExpiryOpensDiscard 保护自选动作到期只打开出牌，旧轮次不能继续自摸。
func TestSelfWindowExpiryOpensDiscard(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 1, hands: [4][]byte{nil, {7, 7}}}
	g.openDrawnTurn(now, 7, false, 1)
	turn := g.turn
	if g.expireSelf(now) || !g.expireSelf(now.Add(responsePeriod)) ||
		g.selfWin(1, turn, now.Add(responsePeriod)) || g.end != nil || g.turn == turn ||
		!g.deadline.Equal(now.Add(responsePeriod+discardPeriod)) {
		t.Fatal("自选动作到期未切换到独立出牌轮次")
	}
}
