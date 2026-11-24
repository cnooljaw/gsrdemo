package main

import (
	"slices"
	"testing"
	"time"
)

// TestDiscardTimeoutEnablesTrustee 保护普通摸牌连续超时两次才进入托管，自动打出摸到的牌。
func TestDiscardTimeoutEnablesTrustee(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, wall: []byte{8, 8}, hands: [4][]byte{{5, 9}, {0x31}}}
	g.openDrawnTurn(now, 5, false, 0)
	if g.tick(now.Add(discardPeriod-time.Nanosecond)) ||
		!g.tick(now.Add(discardPeriod)) || g.trustees[0] || !slices.Equal(g.out[0], []byte{5}) {
		t.Fatal("第一次摸牌超时处理错误")
	}
	// 下一圈轮到同一座位，已有一张牌再摸一张。
	g.active = 0
	g.hands[0] = append(g.hands[0], 4)
	g.openDrawnTurn(now.Add(discardPeriod), 4, false, 0)
	if !g.tick(now.Add(2*discardPeriod)) || !g.trustees[0] ||
		!slices.Equal(g.out[0], []byte{5, 4}) {
		t.Fatal("第二次摸牌超时未进入托管或打错牌")
	}
	var changes []roundEvent
	for i, event := range g.events {
		if event.kind == trusteeEvent {
			changes = append(changes, event)
			if i+1 == len(g.events) || g.events[i+1].kind != discardEvent {
				t.Fatal("自动托管应先于自动出牌")
			}
		}
	}
	if len(changes) != 1 || changes[0].seat != 0 || !changes[0].trustee {
		t.Fatalf("两次超时应只广播一次开启托管：%v", changes)
	}
	if !g.setTrustee(0, false, now.Add(2*discardPeriod)) || g.discardTimeouts[0] != 0 {
		t.Fatal("取消托管后未重新开始累计超时")
	}
}

// TestCancelTrusteeCancelsAutoDiscard 保护取消托管后旧的自动出牌时间不能推进回合。
func TestCancelTrusteeCancelsAutoDiscard(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}}}
	g.openDrawnTurn(now, 5, false, 0)
	if !g.setTrustee(0, true, now) || g.discard(0, g.turn, 9, now) ||
		!g.setTrustee(0, true, now.Add(time.Millisecond)) || g.revision != 1 ||
		!g.setTrustee(0, false, now.Add(time.Millisecond)) ||
		!g.setTrustee(0, false, now.Add(time.Millisecond)) || g.revision != 2 ||
		g.tick(now.Add(trusteeDiscardDelay)) || len(g.out[0]) != 0 ||
		!g.discard(0, g.turn, 9, now.Add(time.Second)) {
		t.Fatal("取消托管后旧自动出牌仍生效或未恢复人工出牌")
	}
	first, second := g.events[0], g.events[1]
	if first.kind != trusteeEvent || !first.trustee || second.kind != trusteeEvent || second.trustee ||
		first.seat != 0 || second.seat != 0 {
		t.Fatalf("手动开启和取消应各广播一次：%+v %+v", first, second)
	}
}

// TestTrusteePassesPendingResponse 保护托管只代答未提交的动作，已提交的胡不撤回。
func TestTrusteePassesPendingResponse(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {3, 4, 7, 7}, {6, 7, 9, 9}, {4, 6, 1, 1}}}
	g.allowDiscard(now)
	if !g.discard(0, g.turn, 5, now) || !g.respond(1, g.windowID, claimHu, now) ||
		!g.setTrustee(1, true, now) || !g.respond(2, g.windowID, claimHu, now) ||
		!g.setTrustee(3, true, now) || g.end == nil || len(g.end.winners) != 2 {
		t.Fatal("托管覆盖了已确认动作或未代答待处理动作")
	}
}

// TestAutoDiscardAfterChow 保护吃牌后自动出牌避开吃什么打什么，且不计入摸牌超时次数。
func TestAutoDiscardAfterChow(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{5, 9}, {2, 3, 4, 5}}}
	g.allowDiscard(now)
	if !g.discard(0, g.turn, 5, now) || !g.respond(1, g.windowID, eatRight, now) ||
		!g.tick(now.Add(discardPeriod)) || !slices.Equal(g.out[1], []byte{2}) || g.discardTimeouts[1] != 0 {
		t.Fatal("吃牌后的自动出牌选错或误计超时")
	}
}

// TestTrusteeSelfPassThenDiscard 保护托管自选窗口先过，再经过独立的自动出牌计时。
func TestTrusteeSelfPassThenDiscard(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, hands: [4][]byte{{7, 7}, {0x31}}, wall: []byte{8}}
	g.openDrawnTurn(now, 7, false, 0)
	g.setTrustee(0, true, now)
	if g.tick(now.Add(trusteeSelfDelay - time.Nanosecond)) {
		t.Fatal("托管提前放弃自选动作")
	}
	at := now.Add(trusteeSelfDelay)
	if !g.tick(at) || g.selfPending || len(g.out[0]) != 0 || g.tick(at) {
		t.Fatal("自选超时应先过，不能同一时刻出牌")
	}
	if !g.tick(at.Add(trusteeDiscardDelay)) || !slices.Equal(g.out[0], []byte{7}) {
		t.Fatal("托管未按出牌计时打出摸牌")
	}
}
