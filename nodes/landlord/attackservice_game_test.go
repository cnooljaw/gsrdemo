package main

import (
	"reflect"
	"testing"
	"time"
)

func testLandlordGame(t *testing.T) *attackGame {
	t.Helper()
	g := newAttackGame("table-test")
	for i := range g.Players {
		g.Players[i] = landlordPlayer{UserID: uint64(i + 1), LoginID: "login", Ready: true}
	}
	deck, err := shuffledDeck()
	if err != nil {
		t.Fatal(err)
	}
	g.deal(deck, 0, time.Now())
	for _, choice := range []bool{true, false, false} {
		if _, err := g.bid(g.Players[g.Seat].UserID, g.Turn, choice, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	g.Turn = 1 // 本文件只验证出牌规则；叫抢的单调轮次由专门用例保护。
	return &g
}

// TestLandlordDealAndPrivacy 保护 54 张牌不重复发放且各玩家只能看到本人手牌。
func TestLandlordDealAndPrivacy(t *testing.T) {
	g := testLandlordGame(t)
	seen := map[int64]bool{}
	for i, p := range g.Players {
		want := 17
		if i == 0 {
			want = 20
		}
		if len(p.Hand) != want {
			t.Fatal("发牌数量", i)
		}
		for _, c := range p.Hand {
			if seen[c] {
				t.Fatal("重复发牌")
			}
			seen[c] = true
		}
	}
	if len(seen) != 54 || len(g.Bottom) != 3 {
		t.Fatal("牌不守恒")
	}
	view := g.view(2)
	if len(view["hand"].([]any)) != 17 {
		t.Fatal("本人手牌缺失")
	}
	for _, value := range view["players"].([]any) {
		if _, exists := value.(map[string]any)["hand"]; exists {
			t.Fatal("暴露了其他人手牌")
		}
	}
}

// TestLandlordInvalidPlayDoesNotChangeState 保护越权、过期轮次、非本人牌、非法牌型和首出不出都不能修改牌局。
func TestLandlordInvalidPlayDoesNotChangeState(t *testing.T) {
	for _, c := range []struct {
		uid, turn uint64
		cards     []int64
	}{{2, 1, []int64{3}}, {1, 0, []int64{3}}, {1, 1, nil}, {1, 1, []int64{0x4f, 0x4f}}, {1, 1, []int64{0x43}}} {
		g := testLandlordGame(t)
		before := g.view(1)
		if err := g.play(c.uid, c.turn, c.cards, time.Now()); err == nil {
			t.Fatal("应拒绝")
		}
		if !reflect.DeepEqual(before, g.view(1)) {
			t.Fatal("非法操作改变了牌局")
		}
	}
}

// TestLandlordPassAndTimeout 保护两家不出后领出权返回上一出牌者，超时只执行合法默认动作。
func TestLandlordPassAndTimeout(t *testing.T) {
	g := testLandlordGame(t)
	now := time.Now()
	g.Players[0].Hand = []int64{3, 4}
	g.Players[1].Hand = []int64{5, 6}
	g.Players[2].Hand = []int64{7, 8}
	if err := g.play(1, 1, []int64{3}, now); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint64{2, 3} {
		if err := g.play(uid, g.Turn, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	if g.Seat != 0 || len(g.Last) != 0 {
		t.Fatal("两次不出未重置领出")
	}
	if err := g.play(1, g.Turn, g.defaultPlay(), now); err != nil {
		t.Fatal(err)
	}
	if g.Phase != "finished" || g.Winner != 0 {
		t.Fatal("未判定胜负")
	}
}

// TestBombMultiplierIsAppliedOnce 保护合法炸弹/王炸翻倍，重复轮次与溢出拒绝不改变手牌或倍率。
func TestBombMultiplierIsAppliedOnce(t *testing.T) {
	g := testLandlordGame(t)
	g.Players[0].Hand = []int64{3, 0x13, 0x23, 0x33, 4}
	turn := g.Turn
	if err := g.play(1, turn, []int64{3, 0x13, 0x23, 0x33}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g.Multiplier != 2 {
		t.Fatal("炸弹未翻倍")
	}
	if err := g.play(1, turn, []int64{3, 0x13, 0x23, 0x33}, time.Now()); err == nil || g.Multiplier != 2 {
		t.Fatal("重复炸弹改变倍率")
	}
	g.Players[1].Hand = []int64{0x4e, 0x4f, 5}
	if err := g.play(2, g.Turn, []int64{0x4e, 0x4f}, time.Now()); err != nil || g.Multiplier != 4 {
		t.Fatal("王炸未翻倍", err)
	}
	g = testLandlordGame(t)
	g.Multiplier = maxMultiplier
	g.Players[0].Hand = []int64{3, 0x13, 0x23, 0x33, 4}
	before := g.view(1)
	if err := g.play(1, g.Turn, []int64{3, 0x13, 0x23, 0x33}, time.Now()); err == nil || !reflect.DeepEqual(before, g.view(1)) {
		t.Fatal("倍率溢出改变状态")
	}
}
