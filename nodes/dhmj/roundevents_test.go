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
