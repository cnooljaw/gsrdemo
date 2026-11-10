package main

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func testSingle(id string, userID uint64, debit, credit int64) map[string]any {
	return map[string]any{"operation_id": id, "business": "demo", "business_ref": "ref", "user_id": userID, "items": []any{map[string]any{"reason": "game_win_loss", "debit": debit, "credit": credit}}, "detail": map[string]any{"reason": "test"}}
}

func testBatch(id string, operations ...map[string]any) map[string]any {
	values := make([]any, len(operations))
	for i, operation := range operations {
		values[i] = operation
	}
	return map[string]any{"version": uint64(2), "batch_id": id, "operations": values}
}

// TestSettlementSeparatesSingleBatchAndGroup 保护不同入口的参数边界，批量单人入口不偷偷接受多人操作。
func TestSettlementSeparatesSingleBatchAndGroup(t *testing.T) {
	single := testSingle("one", 1, 100, 198)
	batch, err := decodeSettlementBatch(testBatch("batch", single))
	if err != nil || len(batch.Operations) != 1 {
		t.Fatalf("single batch: %v", err)
	}
	single["version"] = uint64(2)
	if _, err := decodeSettlementSingle(single); err != nil {
		t.Fatal(err)
	}
	group := map[string]any{"version": uint64(2), "operation_id": "round", "business": "landlord", "business_ref": "round-1", "detail": map[string]any{}, "changes": []any{
		map[string]any{"user_id": uint64(1), "reason": "game_win_loss", "debit": int64(200), "credit": int64(0)},
		map[string]any{"user_id": uint64(2), "reason": "game_win_loss", "debit": int64(0), "credit": int64(100)},
		map[string]any{"user_id": uint64(3), "reason": "game_win_loss", "debit": int64(0), "credit": int64(100)}}}
	if _, err := decodeSettlementGroup(group); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSettlementBatch(testBatch("wrong", group)); !errors.Is(err, errInvalidSettlement) {
		t.Fatalf("group accepted by single batch: %v", err)
	}
	group["changes"].([]any)[1].(map[string]any)["user_id"] = uint64(1)
	if _, err := decodeSettlementGroup(group); err != nil {
		t.Fatal("repeated participant rejected", err)
	}
}

// TestSettlementGroupIsAtomicAndChecksFullDebit 保护多人中一人不足时无人改变，单人返还不能支付本笔扣款。
func TestSettlementGroupIsAtomicAndChecksFullDebit(t *testing.T) {
	wallets := map[uint64]walletBalance{1: {UserID: 1, Amount: 100, Version: 1}, 2: {UserID: 2, Amount: 100, Version: 1}}
	operation := silverOperation{Changes: []silverChange{{UserID: 1, Debit: 200}, {UserID: 2, Credit: 200}}}
	if reason := applySilverOperation(wallets, operation); reason != "insufficient_silver" || wallets[2].Amount != 100 || wallets[1].Amount != 100 {
		t.Fatal("partial group settlement")
	}
	operation.Changes = []silverChange{{UserID: 1, Debit: 200, Credit: 398}}
	if reason := applySilverOperation(wallets, operation); reason != "insufficient_silver" {
		t.Fatal("credit paid its own debit")
	}
	operation.Changes = []silverChange{{UserID: 1, Debit: 100}, {UserID: 2, Credit: 100}}
	if reason := applySilverOperation(wallets, operation); reason != "" || wallets[1].Amount != 0 || wallets[2].Amount != 200 {
		t.Fatal("valid group failed")
	}
	wallets[2] = walletBalance{UserID: 2, Amount: math.MaxInt64, Version: 2}
	operation.Changes = []silverChange{{UserID: 1, Credit: 1}, {UserID: 2, Credit: 1}}
	if reason := applySilverOperation(wallets, operation); reason != "balance_overflow" || wallets[1].Amount != 0 {
		t.Fatal("overflow changed a participant")
	}
}

// TestSettlementDigestIsCanonicalAndBounded 保护明细键顺序不改变身份，内容变更改变摘要，拒绝浮点金额和超限明细。
func TestSettlementDigestIsCanonicalAndBounded(t *testing.T) {
	first := testSingle("one", 1, 100, 0)
	first["detail"] = map[string]any{"b": "two", "a": int64(1)}
	batch, err := decodeSettlementBatch(testBatch("batch", first))
	if err != nil {
		t.Fatal(err)
	}
	second := testSingle("one", 1, 100, 0)
	second["detail"] = map[string]any{"a": uint64(1), "b": "two"}
	same, err := decodeSettlementBatch(testBatch("batch", second))
	if err != nil {
		t.Fatal(err)
	}
	if batch.digest() != same.digest() {
		t.Fatal("equivalent map order changed identity")
	}
	second["items"].([]any)[0].(map[string]any)["credit"] = int64(198)
	changed, _ := decodeSettlementBatch(testBatch("batch", second))
	if batch.digest() == changed.digest() {
		t.Fatal("changed content kept identity")
	}
	second["items"].([]any)[0].(map[string]any)["debit"] = float64(100)
	if _, err := decodeSettlementBatch(testBatch("batch", second)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("noncanonical amount accepted")
	}
	second["items"].([]any)[0].(map[string]any)["debit"] = int64(100)
	second["detail"] = map[string]any{"large": strings.Repeat("x", 64*1024+1)}
	if _, err := decodeSettlementBatch(testBatch("batch", second)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("oversized detail accepted")
	}
	second["detail"] = map[string]any{"invalid": "\xff"}
	if _, err := decodeSettlementBatch(testBatch("batch", second)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("invalid UTF-8 detail accepted")
	}

}

// TestSettlementRepeatedUserChecksAggregate 保护同用户多条明细先汇总扣款，失败时不改变任何钱包。
func TestSettlementRepeatedUserChecksAggregate(t *testing.T) {
	wallets := map[uint64]walletBalance{1: {UserID: 1, Amount: 100, Version: 1}}
	operation := silverOperation{Changes: []silverChange{{UserID: 1, Debit: 5}, {UserID: 1, Debit: 100, Credit: 200}}}
	if reason := applySilverOperation(wallets, operation); reason != "insufficient_silver" || wallets[1].Amount != 100 || wallets[1].Version != 1 {
		t.Fatalf("aggregate debit changed wallet: %s %+v", reason, wallets)
	}
	operation.Changes[1].Debit = 95
	if reason := applySilverOperation(wallets, operation); reason != "" || wallets[1].Amount != 200 || wallets[1].Version != 2 {
		t.Fatalf("successful operation must advance version once: %s %+v", reason, wallets)
	}
}

// TestSettlementReasonCanonicalIdentity 保护分类与拆分参与幂等身份，重排不改变身份，重复明细不会被去重。
func TestSettlementReasonCanonicalIdentity(t *testing.T) {
	request := testSingle("components", 1, 0, 0)
	items := []any{
		map[string]any{"reason": "service_fee", "debit": int64(5), "credit": int64(0)},
		map[string]any{"reason": "game_win_loss", "debit": int64(100), "credit": int64(0)},
		map[string]any{"reason": "service_fee", "debit": int64(5), "credit": int64(0)},
	}
	request["items"] = items
	first, err := decodeSettlementBatch(testBatch("components", request))
	if err != nil {
		t.Fatal(err)
	}
	items[0], items[1] = items[1], items[0]
	second, err := decodeSettlementBatch(testBatch("components", request))
	if err != nil || first.digest() != second.digest() || len(second.Operations[0].Changes) != 3 {
		t.Fatalf("reorder/dedup: %v", err)
	}
	items[0].(map[string]any)["reason"] = "other"
	changed, err := decodeSettlementBatch(testBatch("components", request))
	if err != nil || first.digest() == changed.digest() {
		t.Fatal("reason missing from identity")
	}
	for _, reason := range []string{"", "UPPER", "bad reason", strings.Repeat("a", 65)} {
		items[0].(map[string]any)["reason"] = reason
		if _, err := decodeSettlementBatch(testBatch("components", request)); !errors.Is(err, errInvalidSettlement) {
			t.Fatalf("invalid reason %q accepted", reason)
		}
	}
	request["items"] = make([]any, maxSettlementChanges+1)
	if _, err := decodeSettlementBatch(testBatch("components", request)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("too many changes accepted")
	}
	request = testSingle("legacy", 1, 0, 0)
	request["version"] = uint64(1)
	if _, err := decodeSettlementSingle(request); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("old write version accepted")
	}
}

// TestSettlementAggregateOverflowIsAtomic 保护多条明细加总溢出及余额版本溢出均不修改钱包。
func TestSettlementAggregateOverflowIsAtomic(t *testing.T) {
	wallets := map[uint64]walletBalance{1: {UserID: 1, Amount: math.MaxInt64, Version: 1}}
	operation := silverOperation{Changes: []silverChange{{UserID: 1, Debit: math.MaxInt64}, {UserID: 1, Debit: 1}}}
	if reason := applySilverOperation(wallets, operation); reason != "amount_overflow" || wallets[1].Amount != math.MaxInt64 {
		t.Fatalf("aggregate overflow: %s %+v", reason, wallets)
	}
	wallets[1] = walletBalance{UserID: 1, Amount: 100, Version: math.MaxUint64}
	operation.Changes = []silverChange{{UserID: 1, Debit: 1}, {UserID: 1, Credit: 1}}
	if reason := applySilverOperation(wallets, operation); reason != "version_overflow" || wallets[1].Amount != 100 {
		t.Fatalf("version overflow: %s %+v", reason, wallets)
	}
}
