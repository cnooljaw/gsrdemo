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
			operation := operationForGame(*g)
			if !operation.valid() {
				t.Fatal("合法结果被拒绝")
			}
			var total int64
			for i, c := range operation.Changes {
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
			operation.Changes[0].Credit++
			if operation.valid() {
				t.Fatal("篡改金额被接受")
			}
		}
	}
}

// TestRoundPreparationKeepsOriginalRequest 保护结束前不生成账务，固定的局末请求不随游戏对象的后续修改而改变。
func TestRoundPreparationKeepsOriginalRequest(t *testing.T) {
	game := testLandlordGame(t)
	round := roundSettlement{}
	round.prepareOperation(*game)
	if round.pending() || round.snapshotFor(1) != nil {
		t.Fatal("进行中的牌局生成了局末账务")
	}
	game.Phase, game.Winner = "finished", game.Landlord
	round.prepareOperation(*game)
	want := round.operation.wire()
	game.Name, game.Multiplier, game.Players[0].UserID = "another-table", 8, 100
	round.prepareOperation(*game)
	if !round.pending() || !reflect.DeepEqual(want, round.operation.wire()) {
		t.Fatal("待提交的原 ID 或资金内容被后续游戏修改覆盖")
	}
}

// TestRoundReceiptAppliesOnce 保护 Call 回执只确认原牌局一次，错误内容不改变待请求，重复确认不覆盖终态。
func TestRoundReceiptAppliesOnce(t *testing.T) {
	game := testLandlordGame(t)
	game.Phase, game.Winner = "finished", game.Landlord
	round := roundSettlement{}
	round.prepareOperation(*game)
	id := round.operation.ID
	balances := []any{}
	for _, change := range round.operation.Changes {
		balances = append(balances, map[string]any{"user_id": change.UserID, "amount": int64(1000), "version": uint64(2)})
	}
	fields := map[string]any{"results": []any{map[string]any{"operation_id": id, "status": "committed", "reason": ""}}, "balances": balances}
	for _, malformed := range []any{
		nil,
		map[string]any{},
		map[string]any{"results": []any{map[string]any{"operation_id": "other-round", "status": "committed", "reason": ""}}, "balances": balances},
		map[string]any{"results": []any{map[string]any{"operation_id": id, "status": "pending", "reason": ""}}, "balances": balances},
		map[string]any{"results": fields["results"], "balances": balances[:2]},
		map[string]any{"results": fields["results"], "balances": []any{balances[0], balances[0], balances[2]}},
		map[string]any{"results": fields["results"], "balances": []any{map[string]any{"user_id": uint64(99), "amount": int64(1000), "version": uint64(2)}, balances[1], balances[2]}},
		map[string]any{"results": fields["results"], "balances": []any{map[string]any{"user_id": uint64(1), "amount": int64(-1), "version": uint64(2)}, balances[1], balances[2]}},
	} {
		if err := round.applyReceipt(malformed); err == nil || !round.pending() || round.cleanupPending || len(round.operation.Balances) != 0 {
			t.Fatalf("非法回执改变了账务: %v", malformed)
		}
	}
	if err := round.applyReceipt(fields); err != nil || round.pending() || !round.cleanupPending {
		t.Fatalf("有效回执未确认: %v", err)
	}
	for _, change := range round.operation.Changes {
		snapshot := round.snapshotFor(change.UserID)
		if snapshot["operation_id"] != id || snapshot["status"] != "committed" ||
			snapshot["debit"] != change.Debit || snapshot["credit"] != change.Credit ||
			snapshot["balance"] != int64(1000) || snapshot["version"] != uint64(2) {
			t.Fatalf("玩家结算快照不完整: %v", snapshot)
		}
		snapshot["status"], snapshot["balance"] = "pending", int64(0)
		if round.operation.Status != "committed" || round.operation.Balances[change.UserID].Amount != 1000 {
			t.Fatal("修改快照影响了本桌确认结果")
		}
	}
	if err := round.applyReceipt(map[string]any{}); err != nil || round.operation.Status != "committed" || round.fault != nil {
		t.Fatal("重复确认覆盖了终态")
	}
}
