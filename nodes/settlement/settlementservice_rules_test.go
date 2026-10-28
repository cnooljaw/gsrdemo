package main

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func testSingle(id string, userID uint64, debit, credit int64) map[string]any {
	return map[string]any{"operation_id": id, "business": "demo", "business_ref": "ref", "user_id": userID, "debit": debit, "credit": credit, "detail": map[string]any{"reason": "test"}}
}

func testBatch(id string, operations ...map[string]any) map[string]any {
	values := make([]any, len(operations))
	for i, operation := range operations {
		values[i] = operation
	}
	return map[string]any{"version": uint64(1), "batch_id": id, "operations": values}
}

// TestSettlementSeparatesSingleBatchAndGroup 保护不同入口的参数边界，批量单人入口不偷偷接受多人操作。
func TestSettlementSeparatesSingleBatchAndGroup(t *testing.T) {
	single := testSingle("one", 1, 100, 198)
	batch, err := decodeSettlementBatch(testBatch("batch", single))
	if err != nil || len(batch.Operations) != 1 {
		t.Fatalf("single batch: %v", err)
	}
	single["version"] = uint64(1)
	if _, err := decodeSettlementSingle(single); err != nil {
		t.Fatal(err)
	}
	group := map[string]any{"version": uint64(1), "operation_id": "round", "business": "landlord", "business_ref": "round-1", "detail": map[string]any{}, "changes": []any{
		map[string]any{"user_id": uint64(1), "debit": int64(200), "credit": int64(0)},
		map[string]any{"user_id": uint64(2), "debit": int64(0), "credit": int64(100)},
		map[string]any{"user_id": uint64(3), "debit": int64(0), "credit": int64(100)}}}
	if _, err := decodeSettlementGroup(group); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSettlementBatch(testBatch("wrong", group)); !errors.Is(err, errInvalidSettlement) {
		t.Fatalf("group accepted by single batch: %v", err)
	}
	group["changes"].([]any)[1].(map[string]any)["user_id"] = uint64(1)
	if _, err := decodeSettlementGroup(group); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("duplicate participant accepted")
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
	second["credit"] = int64(198)
	changed, _ := decodeSettlementBatch(testBatch("batch", second))
	if batch.digest() == changed.digest() {
		t.Fatal("changed content kept identity")
	}
	second["debit"] = float64(100)
	if _, err := decodeSettlementBatch(testBatch("batch", second)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("noncanonical amount accepted")
	}
	second["debit"] = int64(100)
	second["detail"] = map[string]any{"large": strings.Repeat("x", 64*1024+1)}
	if _, err := decodeSettlementBatch(testBatch("batch", second)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("oversized detail accepted")
	}
	second["detail"] = map[string]any{"invalid": "\xff"}
	if _, err := decodeSettlementBatch(testBatch("batch", second)); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("invalid UTF-8 detail accepted")
	}

}
