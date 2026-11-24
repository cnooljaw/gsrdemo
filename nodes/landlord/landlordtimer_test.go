package main

import (
	"errors"
	"testing"
	"time"
)

// TestAnimationRejectsPlayerAction 保护发牌及新轮动画期间请求不能抢先修改牌桌，开放后仍可正常操作。
func TestAnimationRejectsPlayerAction(t *testing.T) {
	game, _ := testBidding(t)
	game.Players[0].AccessNode, game.Players[0].AgentName = "access", "agent"
	self := &tableCleanupContext{}
	service := newAttackService(game.Name, landlordJournal{}, nil, "settlement")
	service.game = *game
	service.game.waitTurn(time.Now(), dealDelay)
	if _, err := service.handleCallLandlord(self, 1, "login", service.game.Turn, true); !errors.Is(err, errLandlordTurn) {
		t.Fatalf("动画期间接受叫地主: %v", err)
	}
	if changed, err := service.game.timeout(service.game.WaitUntil); !changed || err != nil {
		t.Fatal("未开放叫牌", err)
	}
	if accepted, err := service.handleCallLandlord(self, 1, "login", service.game.Turn, true); !accepted || err != nil {
		t.Fatal("开放后不能叫牌", err)
	}

	playing := testLandlordGame(t)
	playing.Players[0].Hand = []int64{3, 4}
	playing.Players[0].AccessNode, playing.Players[0].AgentName = "access", "agent"
	service.game = *playing
	service.game.waitTurn(time.Now(), newRoundDelay)
	if _, err := service.handlePlay(self, 1, "login", service.game.Turn, []any{int64(3)}); !errors.Is(err, errLandlordTurn) {
		t.Fatalf("动画期间接受出牌: %v", err)
	}
	if changed, err := service.game.timeout(service.game.WaitUntil); !changed || err != nil {
		t.Fatal("未开放出牌", err)
	}
	if accepted, err := service.handlePlay(self, 1, "login", service.game.Turn, []any{int64(3)}); !accepted || err != nil {
		t.Fatal("开放后不能出牌", err)
	}
}

// TestDealWaitsForServerTurn 保护发牌动画结束前不可操作，到期只开放叫牌而不自动选择。
func TestDealWaitsForServerTurn(t *testing.T) {
	game, deck := testBidding(t)
	now := time.Unix(100, 0)
	game.deal(deck, 0, now)
	turn := game.Turn
	if game.WaitUntil != now.Add(3*time.Second) || game.view(1)["can_act"] != false {
		t.Fatal("发牌后未等待服务器开放叫牌")
	}
	if changed, err := game.timeout(now.Add(3*time.Second - time.Nanosecond)); changed || err != nil {
		t.Fatal("发牌动画提前结束", err)
	}
	if changed, err := game.timeout(now.Add(3 * time.Second)); !changed || err != nil || !game.WaitUntil.IsZero() || game.Turn != turn || game.view(1)["can_act"] != true {
		t.Fatal("服务器没有按时开放当前叫牌", err)
	}
	if !game.Deadline.Equal(now.Add(3*time.Second + bidPeriod)) {
		t.Fatal("叫牌倒计时未从服务器开放时开始")
	}
}

// TestNewRoundWaitsForServerTurn 保护两家不出后等待动画，过期 Tick 仅开放原领出者。
func TestNewRoundWaitsForServerTurn(t *testing.T) {
	game := testLandlordGame(t)
	game.Players[0].Hand, game.Players[1].Hand, game.Players[2].Hand = []int64{3, 4}, []int64{5, 6}, []int64{7, 8}
	now := time.Unix(100, 0)
	if err := game.play(1, game.Turn, []int64{3}, now); err != nil {
		t.Fatal(err)
	}
	for _, user := range []uint64{2, 3} {
		if err := game.play(user, game.Turn, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	turn := game.Turn
	if game.Seat != 0 || game.WaitUntil != now.Add(500*time.Millisecond) || game.view(1)["can_act"] != false {
		t.Fatal("重新领出没有等待动画")
	}
	if changed, err := game.timeout(now.Add(500*time.Millisecond - time.Nanosecond)); changed || err != nil {
		t.Fatal("新一轮提前开放", err)
	}
	if changed, err := game.timeout(now.Add(500 * time.Millisecond)); !changed || err != nil || game.Turn != turn || game.Seat != 0 || game.view(1)["can_act"] != true {
		t.Fatal("服务器没有开放原领出者", err)
	}
}

// TestPlayTimeoutEntersTrustee 保护出牌到期先进入托管，随后按服务器时间自动执行且不可重复。
func TestPlayTimeoutEntersTrustee(t *testing.T) {
	game := testLandlordGame(t)
	game.Players[game.Seat].Hand = []int64{3, 4}
	deadline, turn := game.Deadline, game.Turn
	if changed, err := game.timeout(deadline.Add(-time.Nanosecond)); changed || err != nil {
		t.Fatalf("未到期推进了牌局: %v %v", changed, err)
	}
	if changed, err := game.timeout(deadline); !changed || err != nil || !game.Players[0].Trustee || game.Turn != turn {
		t.Fatalf("超时未先进入托管: changed=%v err=%v game=%+v", changed, err, game)
	}
	if changed, err := game.timeout(deadline.Add(499 * time.Millisecond)); changed || err != nil {
		t.Fatalf("托管动作提前执行: %v %v", changed, err)
	}
	if changed, err := game.timeout(deadline.Add(500 * time.Millisecond)); !changed || err != nil || game.Turn != turn+1 || game.Seat != 1 || len(game.Last) != 1 || game.Last[0] != 3 {
		t.Fatalf("托管领出没有恰好执行一次: %v %v %+v", changed, err, game)
	}
	if changed, err := game.timeout(deadline.Add(500 * time.Millisecond)); changed || err != nil {
		t.Fatalf("旧 Tick 推进了下一轮: %v %v", changed, err)
	}
}

// TestCancelTrusteeRestoresTurn 保护取消托管后旧的自动时间不能再执行，玩家获得有效操作时间。
func TestCancelTrusteeRestoresTurn(t *testing.T) {
	game := testLandlordGame(t)
	game.Players[0].Hand = []int64{3, 4}
	now := game.Deadline
	if _, err := game.timeout(now); err != nil {
		t.Fatal(err)
	}
	game.setTrustee(0, false, now.Add(100*time.Millisecond))
	if game.Players[0].Trustee || !game.Deadline.After(now.Add(500*time.Millisecond)) {
		t.Fatal("取消托管未恢复玩家出牌时间")
	}
	if changed, err := game.timeout(now.Add(500 * time.Millisecond)); changed || err != nil {
		t.Fatal("旧自动时间在取消后仍生效", err)
	}
}

// TestFinishedHasNoDeadline 保护最后一手之后的场景不再携带旧倒计时。
func TestFinishedHasNoDeadline(t *testing.T) {
	game := testLandlordGame(t)
	game.Players[0].Hand = []int64{3}
	if err := game.play(1, game.Turn, []int64{3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if game.Phase != "finished" || !game.Deadline.IsZero() || game.view(1)["deadline_ms"] != int64(0) {
		t.Fatal("结束后仍显示旧截止时间")
	}
}

// TestTrusteeActsOnItsOwnTurn 保护托管玩家叫抢立即默认否、出牌等待半秒并按当前轮次执行。
func TestTrusteeActsOnItsOwnTurn(t *testing.T) {
	game, _ := testBidding(t)
	game.Players[1].Trustee = true
	now := time.Unix(100, 0)
	if err := game.applyBid(1, game.Turn, true, now); err != nil {
		t.Fatal(err)
	}
	turn := game.Turn
	if game.Seat != 1 || !game.nextDeadline().Equal(now) {
		t.Fatal("托管玩家未立即进入叫抢")
	}
	if changed, err := game.timeout(now); !changed || err != nil || game.Robs[1] != -1 || game.Turn != turn+1 {
		t.Fatalf("托管叫抢未按不抢推进: changed=%v err=%v", changed, err)
	}
	game = testLandlordGame(t)
	game.Players[0].Hand = []int64{3, 4}
	game.Players[1].Trustee = true
	if err := game.play(1, game.Turn, []int64{3}, now); err != nil {
		t.Fatal(err)
	}
	turn = game.Turn
	if !game.nextDeadline().Equal(now.Add(trusteePlayDelay)) {
		t.Fatal("托管出牌时间不是半秒")
	}
	if changed, _ := game.timeout(now.Add(trusteePlayDelay - time.Nanosecond)); changed {
		t.Fatal("托管出牌提前")
	}
	if changed, err := game.timeout(now.Add(trusteePlayDelay)); !changed || err != nil || game.Turn != turn+1 || game.Seat != 2 || game.Passes != 1 {
		t.Fatalf("托管跟牌未按不出推进: changed=%v err=%v", changed, err)
	}
}

// TestRedealKeepsOfflineTrustee 保护重发牌只清在线托管，离线玩家继续由服务端代打。
func TestRedealKeepsOfflineTrustee(t *testing.T) {
	game, _ := testBidding(t)
	game.Players[0].Trustee = true
	game.Players[0].AccessNode, game.Players[0].AgentName = "access", "agent"
	game.Players[1].Trustee = true
	for range 3 {
		if err := game.applyBid(game.Players[game.Seat].UserID, game.Turn, false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if game.Redeals != 1 || game.Phase != "calling" {
		t.Fatal("无人叫地主没有重发")
	}
	if game.Players[0].Trustee || !game.Players[1].Trustee || game.Players[2].Trustee {
		t.Fatal("重发后在线或离线托管状态不正确")
	}
}
