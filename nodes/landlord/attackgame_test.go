package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func testLandlordGame(t *testing.T) *attackGame {
	t.Helper()
	g := newAttackGame("table-test")
	for i := range g.Players {
		g.Players[i] = landlordPlayer{UserID: uint64(i + 1), LoginID: "login", Membership: fmt.Sprintf("seat-%d", i), Ready: true}
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

// TestGameSeatChangesPreserveCurrentHand 保护游戏对象重连仅更新身份地址，旧登录不能准备，断线不删除原手牌。
func TestGameSeatChangesPreserveCurrentHand(t *testing.T) {
	game := newAttackGame("table-test")
	if err := game.join(1, "first-login", "access-01", "agent-a", "seat-a"); err != nil {
		t.Fatal(err)
	}
	seat, err := game.readySeat(1, "first-login")
	if err != nil || game.markReady(seat) {
		t.Fatal("未满三人仍能开局", err)
	}
	game.Players[seat].Hand = []int64{3, 4}
	game.Phase = "playing"
	if err := game.join(1, "new-login", "access-02", "agent-b", "seat-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := game.readySeat(1, "first-login"); err == nil {
		t.Fatal("旧登录仍可准备")
	}
	if err := game.join(2, "other-login", "access-01", "agent-c", "seat-c"); err == nil {
		t.Fatal("进行中牌局接纳了新玩家")
	}
	game.detach(seat)
	player := game.Players[seat]
	if player.LoginID != "new-login" || player.AccessNode != "" || player.AgentName != "" || !reflect.DeepEqual(player.Hand, []int64{3, 4}) {
		t.Fatal("断线丢失当前登录或原手牌", player)
	}
	if !game.remove(seat) || game.Players[seat].UserID != 0 {
		t.Fatal("释放最后一个座位没有清空成员")
	}
}
