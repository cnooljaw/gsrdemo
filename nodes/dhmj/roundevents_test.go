package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

// TestDrawEventsKeepFlowerOrderAndPrivateTiles 保护先发本人起手牌，再公布补花和首摸。
func TestDrawEventsKeepFlowerOrderAndPrivateTiles(t *testing.T) {
	wall := wallWith(t, map[int]byte{52: 0x41, 53: 0x42, 54: 0x37})
	g, err := dealRound(wall, 0, 0x31)
	if err != nil {
		t.Fatal(err)
	}
	g.startPlay(time.Now())
	if len(g.events) != 3 || g.revision != 3 {
		t.Fatalf("事件数错误：%+v", g.events)
	}
	deal, flower, draw := g.events[0], g.events[1], g.events[2]
	for seat := range seatCount {
		command, payload := deal.wire("table", seat)
		if command != accessagent.DhmjDealPushCommand || deal.revision != 1 || payload["seat"] != int64(seat) ||
			payload["dealer_seat"] != int64(0) || payload["remaining"] != int64(92) ||
			!reflect.DeepEqual(payload["hand"], wireTiles(wall[seat*openingHandSize:(seat+1)*openingHandSize])) {
			t.Fatalf("座位 %d 的起手牌错误：%v", seat, payload)
		}
	}
	flowerCommand, ownFlowers := flower.wire("table", 0)
	drawCommand, ownDraw := draw.wire("table", 0)
	if flowerCommand != accessagent.DhmjFlowerPushCommand || flower.revision != 2 ||
		!reflect.DeepEqual(ownFlowers["flowers"], []any{int64(0x41), int64(0x42)}) ||
		!reflect.DeepEqual(ownFlowers["replacements"], []any{int64(0x42), int64(0x37)}) ||
		ownFlowers["from_draw"] != true || ownFlowers["remaining"] != int64(89) {
		t.Fatalf("错误的连续补花事件：%v", ownFlowers)
	}
	if drawCommand != accessagent.DhmjDrawPushCommand || draw.revision != 3 || ownDraw["tile"] != int64(0x37) ||
		ownDraw["turn"] != g.turn || ownDraw["remaining"] != int64(89) {
		t.Fatalf("错误的最终摸牌事件：%v", ownDraw)
	}
	_, otherFlowers := flower.wire("table", 1)
	_, otherDraw := draw.wire("table", 1)
	if _, ok := otherFlowers["replacements"]; ok {
		t.Fatalf("向其他座位泄露替换牌：%v", otherFlowers)
	}
	if _, ok := otherDraw["tile"]; ok {
		t.Fatalf("向其他座位泄露摸牌：%v", otherDraw)
	}
}

// TestTurnNoticeKeepsOnlyRecipientOffers 保护抢杠窗口的公开信息和选项隔离，以及窗口结束后的出牌阶段。
func TestTurnNoticeKeepsOnlyRecipientOffers(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 1, turn: 4, hands: [4][]byte{nil, {5, 8}}}
	g.windowID = 7
	g.window = &discardWindow{source: 1, tile: 5, robKong: true}
	g.window.offers[0].add(claimHu)
	g.window.offers[2].add(claimHu)
	g.window.choices[2] = claimHu
	g.deadline = now.Add(responsePeriod)
	g.recordTurn(g.currentTurn(), now)
	event := g.events[0]
	for _, seat := range []int{0, 1, 2, 3} {
		cmd, payload := event.wire("table", seat)
		if cmd != accessagent.DhmjTurnPushCommand || payload["phase"] != "respond" ||
			payload["seat"] != int64(seat) || payload["window_id"] != uint64(7) ||
			payload["window_tile"] != int64(5) || payload["rob_kong"] != true ||
			payload["deadline_ms"] != g.deadline.UnixMilli() {
			t.Fatalf("错误的抢杠窗口：%v", payload)
		}
		offers := payload["offers"].([]any)
		if seat == 0 {
			if !reflect.DeepEqual(offers, []any{int64(claimHu)}) {
				t.Fatalf("可胡玩家未收到选项：%v", payload)
			}
		} else if len(offers) != 0 {
			t.Fatalf("选项泄露或已提交玩家再次收到选项：%v", payload)
		}
	}

	g.window = nil
	g.allowDiscard(now)
	g.recordTurn(g.currentTurn(), now)
	_, current := g.events[1].wire("table", 1)
	_, other := g.events[1].wire("table", 0)
	if current["phase"] != "discard" || current["window_id"] != uint64(0) ||
		current["can_discard"] != true || other["can_discard"] != nil {
		t.Fatalf("关闭窗口后出牌权限错误：%v %v", current, other)
	}
}

// TestTurnNoticeAfterResponseTimeout 保护超时窗口结束后推送实际开放的下家出牌阶段。
func TestTurnNoticeAfterResponseTimeout(t *testing.T) {
	now := time.Unix(100, 0)
	g := round{active: 0, turn: 1, hands: [4][]byte{nil, {8}}, wall: []byte{9}}
	g.drawn[1] = true
	window := discardWindow{source: 0, tile: 5}
	window.offers[2].add(pong)
	g.openResponse(window, now)
	before := g.currentTurn()
	if before.phase != "respond" || g.expireResponse(now) {
		t.Fatal("窗口未开放或提前结束")
	}
	if !g.expireResponse(before.deadline) {
		t.Fatal("已超时的响应窗口未结束")
	}
	g.recordTurn(g.currentTurn(), before.deadline)
	_, payload := g.events[len(g.events)-1].wire("table", 1)
	if payload["phase"] != "discard" || payload["window_id"] != uint64(0) ||
		payload["active_seat"] != int64(1) || payload["can_discard"] != true {
		t.Fatalf("超时后未推送实际出牌阶段：%v", payload)
	}
}

// TestRoundEndEventIncludesAllWinningSeats 保护一炮多响一次公布所有赢家并保留胡牌张。
func TestRoundEndEventIncludesAllWinningSeats(t *testing.T) {
	g := round{hands: [4][]byte{nil, {3, 4, 7, 7}, nil, {6, 7, 9, 9}}}
	g.finishDiscardHu(discardWindow{source: 0, tile: 5}, []response{{seat: 1, action: claimHu}, {seat: 3, action: claimHu}})
	g.recordRoundEnd()
	command, payload := g.events[0].wire("table", 0)
	result := payload["round_end"].(map[string]any)
	winners := result["winners"].([]any)
	if command != accessagent.DhmjRoundEndPushCommand || result["kind"] != "discard_hu" ||
		result["source_seat"] != int64(0) || result["tile"] != int64(5) || len(winners) != 2 ||
		winners[0].(map[string]any)["seat"] != int64(1) || winners[1].(map[string]any)["seat"] != int64(3) {
		t.Fatalf("一炮多响事件错误：%v", payload)
	}
}
