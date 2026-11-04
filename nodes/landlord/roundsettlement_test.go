package main

import (
	"reflect"
	"testing"
)

// TestRoundChanges 保护不同地主与胜方按当前倍率结算，三人变化守恒，异常金额和倍率不能持久化。
func TestRoundChanges(t *testing.T) {
	for landlord := 0; landlord < 3; landlord++ {
		for winner := 0; winner < 3; winner++ {
			g := testLandlordGame(t)
			g.Landlord, g.Winner, g.Multiplier = landlord, winner, 8
			result := resultForGame(*g)
			if !result.valid() {
				t.Fatal("合法结果被拒绝")
			}
			var total int64
			for i, c := range result.Changes {
				total += c.Credit - c.Debit
				amount := int64(80)
				if i == landlord {
					amount = 160
				}
				if c.Credit+c.Debit != amount {
					t.Fatal("倍率或地主结算错误")
				}
			}
			if total != 0 {
				t.Fatal("银子不守恒")
			}
			result.Changes[0].Credit++
			if result.valid() {
				t.Fatal("篡改金额被接受")
			}
		}
	}
}

// TestRoundPreparationKeepsOriginalRequest 保护结束前不生成账务，固定的局末请求不随游戏对象的后续修改而改变。
func TestRoundPreparationKeepsOriginalRequest(t *testing.T) {
	game := testLandlordGame(t)
	round := roundSettlement{}
	round.prepare(*game)
	if round.pending() || round.view(1) != nil {
		t.Fatal("进行中的牌局生成了局末账务")
	}
	game.Phase, game.Winner = "finished", game.Landlord
	round.prepare(*game)
	want := round.result.wire()
	game.Name, game.Multiplier, game.Players[0].UserID = "another-table", 8, 100
	round.prepare(*game)
	if !round.pending() || !reflect.DeepEqual(want, round.result.wire()) {
		t.Fatal("待提交的原 ID 或资金内容被后续游戏修改覆盖")
	}
}

// TestRoundReceiptAppliesOnce 保护异步回执只确认原牌局一次，重复终态、旧错误和其他牌局结果不能覆盖已确认账务。
func TestRoundReceiptAppliesOnce(t *testing.T) {
	game := testLandlordGame(t)
	game.Phase, game.Winner = "finished", game.Landlord
	round := roundSettlement{}
	round.prepare(*game)
	id := round.result.ID
	balances := []any{}
	for _, change := range round.result.Changes {
		balances = append(balances, map[string]any{"user_id": change.UserID, "amount": int64(1000), "version": uint64(2)})
	}
	fields := map[string]any{"results": []any{map[string]any{"operation_id": id, "status": "committed", "reason": ""}}, "balances": balances}
	if accepted, err := round.accept("other-round", fields, ""); accepted || err != nil || !round.pending() {
		t.Fatal("其他牌局结果改变了当前账务")
	}
	if accepted, err := round.accept(id, fields, "settlement.busy"); accepted || err == nil || round.fault != nil || !round.pending() {
		t.Fatal("临时错误被当成终态")
	}
	if accepted, err := round.accept(id, map[string]any{}, ""); accepted || err == nil || !round.pending() {
		t.Fatal("非法回执改变了账务")
	}
	if accepted, err := round.accept(id, fields, ""); !accepted || err != nil || round.pending() || !round.cleanupPending {
		t.Fatalf("有效回执未确认: %v %v", accepted, err)
	}
	for _, code := range []string{"", "settlement.store_unavailable", "settlement.identity_conflict"} {
		if accepted, err := round.accept(id, fields, code); accepted || err != nil || round.result.Status != "committed" || round.fault != nil {
			t.Fatal("重复回执或迟到错误覆盖了终态")
		}
	}
}
