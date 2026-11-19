package main

import (
	"maps"
	"math"
	"reflect"
	"testing"
)

// TestTransferWithFeesCapsBeforePayments 保护先收全员手续费、再分配出款，并只增加一次版本。
func TestTransferWithFeesCapsBeforePayments(t *testing.T) {
	wallets := map[uint64]walletBalance{
		1: {UserID: 1, Amount: 71, Version: 1},
		2: {UserID: 2, Amount: 20, Version: 1},
		3: {UserID: 3, Amount: 60, Version: 1},
		4: {UserID: 4, Amount: 10, Version: 1},
	}
	fees := []settlementFee{{1, 50}, {2, 50}, {3, 50}, {4, 50}}
	transfers := []settlementTransfer{{1, 2, 100}, {1, 3, 100}, {2, 3, 1}}
	actual, edges, charged, reason := settleTransferWithFees(wallets, transfers, fees)
	if reason != "" || !reflect.DeepEqual(edges, []settlementTransfer{{1, 2, 10}, {1, 3, 11}, {2, 3, 0}}) ||
		!reflect.DeepEqual(charged, []settlementFee{{1, 50}, {2, 20}, {3, 50}, {4, 10}}) ||
		!reflect.DeepEqual(actual, []settlementActual{{1, 71, 0}, {2, 20, 10}, {3, 50, 11}, {4, 10, 0}}) {
		t.Fatalf("错误扣费或赔付：%v %v %v %s", charged, edges, actual, reason)
	}
	for id, amount := range map[uint64]int64{1: 0, 2: 10, 3: 21, 4: 0} {
		if wallet := wallets[id]; wallet.Amount != amount || wallet.Version != 2 {
			t.Fatalf("钱包 %d 未一次应用手续费和输赢：%+v", id, wallet)
		}
	}
}

// TestTransferWithFeesLeavesWalletsUntouchedOnError 保护后续付款溢出或缺失钱包时不提前扣费。
func TestTransferWithFeesLeavesWalletsUntouchedOnError(t *testing.T) {
	wallets := map[uint64]walletBalance{1: {UserID: 1, Amount: 71}, 2: {UserID: 2, Amount: math.MaxInt64}}
	for _, tc := range []struct {
		fees []settlementFee
		want string
	}{
		{[]settlementFee{{1, 50}, {2, 0}}, "balance_overflow"},
		{[]settlementFee{{1, 50}, {2, 0}, {3, 50}}, "wallet_not_found"},
	} {
		before := maps.Clone(wallets)
		_, _, _, reason := settleTransferWithFees(wallets, []settlementTransfer{{1, 2, 100}}, tc.fees)
		if reason != tc.want || !maps.Equal(wallets, before) {
			t.Fatalf("错误 %q 修改了钱包：%v", reason, wallets)
		}
	}
}

func feeTransferRequest(id string) map[string]any {
	return map[string]any{
		"version": uint64(2), "operation_id": id, "business": "dhmj", "business_ref": id, "detail": map[string]any{},
		"fees": []any{
			map[string]any{"user_id": uint64(2), "amount": int64(50)},
			map[string]any{"user_id": uint64(1), "amount": int64(50)},
		},
		"transfers": []any{map[string]any{"from_user_id": uint64(1), "to_user_id": uint64(2), "amount": int64(100)}},
	}
}

// TestFeeTransferContract 保护费用纳入幂等身份、列表顺序归一，以及边参与者必须明确列费。
func TestFeeTransferContract(t *testing.T) {
	req := feeTransferRequest("fee-round")
	first, err := decodeTransferGroupWithFees(req)
	if err != nil {
		t.Fatal(err)
	}
	list := req["fees"].([]any)
	list[0], list[1] = list[1], list[0]
	reordered, err := decodeTransferGroupWithFees(req)
	if err != nil || first.digest() != reordered.digest() {
		t.Fatal("费用顺序影响幂等身份", err)
	}
	list[0].(map[string]any)["amount"] = int64(51)
	changed, err := decodeTransferGroupWithFees(req)
	if err != nil || first.digest() == changed.digest() {
		t.Fatal("费用变化未改变请求摘要", err)
	}
	list[1].(map[string]any)["user_id"] = uint64(3)
	if _, err := decodeTransferGroupWithFees(req); err == nil {
		t.Fatal("接受了未列在费用参与者中的收款人")
	}
}
