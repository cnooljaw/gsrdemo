package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

// TestDrawEventsKeepFlowerOrderAndPrivateTiles 保护连续补花、最终摸牌的顺序及两类事件的暗牌范围。
func TestDrawEventsKeepFlowerOrderAndPrivateTiles(t *testing.T) {
	g, err := dealRound(wallWith(t, map[int]byte{52: 0x41, 53: 0x42, 54: 0x37}), 0, 0x31)
	if err != nil {
		t.Fatal(err)
	}
	g.startPlay(time.Now())
	if len(g.events) != 2 || g.revision != 2 {
		t.Fatalf("事件数错误：%+v", g.events)
	}
	flower, draw := g.events[0], g.events[1]
	flowerCommand, ownFlowers := flower.wire("table", 0)
	drawCommand, ownDraw := draw.wire("table", 0)
	if flowerCommand != accessagent.DhmjFlowerPushCommand || flower.revision != 1 ||
		!reflect.DeepEqual(ownFlowers["flowers"], []any{int64(0x41), int64(0x42)}) ||
		!reflect.DeepEqual(ownFlowers["replacements"], []any{int64(0x42), int64(0x37)}) ||
		ownFlowers["from_draw"] != true || ownFlowers["remaining"] != int64(89) {
		t.Fatalf("错误的连续补花事件：%v", ownFlowers)
	}
	if drawCommand != accessagent.DhmjDrawPushCommand || draw.revision != 2 || ownDraw["tile"] != int64(0x37) ||
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
