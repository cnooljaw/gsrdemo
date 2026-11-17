package main

import (
	"reflect"
	"testing"
	"time"
)

// TestPlayEventReportsCommittedAction 保护出牌事件描述已提交动作，不包含其他玩家手牌。
func TestPlayEventReportsCommittedAction(t *testing.T) {
	game := newAttackGame("table")
	game.Phase, game.Seat, game.Turn, game.LastSeat = "playing", 0, 4, -1
	game.Players[0] = landlordPlayer{UserID: 1, Hand: []int64{3, 4}}
	game.Players[1] = landlordPlayer{UserID: 2, Hand: []int64{5}}
	game.Players[2] = landlordPlayer{UserID: 3, Hand: []int64{6}}
	if err := game.play(1, 4, []int64{3}, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	event := landlordPlayEvent(game, 1, []int64{3})
	if event["user_id"] != uint64(1) || !reflect.DeepEqual(event["cards"], []any{int64(3)}) || event["remaining"] != int64(1) || event["turn"] != uint64(5) || event["turn_seat"] != int64(1) {
		t.Fatalf("错误出牌事件: %v", event)
	}
	if _, exists := event["hand"]; exists {
		t.Fatal("出牌广播包含私有手牌")
	}
}

// TestDealEventOnlyContainsOwnHand 保护发牌通知只包含接收者的手牌。
func TestDealEventOnlyContainsOwnHand(t *testing.T) {
	game := newAttackGame("table")
	game.Players[0] = landlordPlayer{UserID: 1, Hand: []int64{3, 4}}
	game.Players[1] = landlordPlayer{UserID: 2, Hand: []int64{5, 6}}
	game.Players[2] = landlordPlayer{UserID: 3, Hand: []int64{7, 8}}
	event := landlordDealEvent(game, 2)
	if !reflect.DeepEqual(event["hand"], []any{int64(5), int64(6)}) {
		t.Fatalf("发牌通知没有本人手牌: %v", event)
	}
	if _, exists := event["players"]; exists {
		t.Fatal("发牌通知包含其他玩家手牌")
	}
}
