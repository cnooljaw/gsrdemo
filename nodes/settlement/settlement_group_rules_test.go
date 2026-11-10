package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

// TestEqualGroupCapsLosersAndSplitsDescending 保护多人输方各自封顶、赢家均分及按 UserID 降序分配余数。
func TestEqualGroupCapsLosersAndSplitsDescending(t *testing.T) {
	cases := []struct {
		name    string
		amounts map[uint64]int64
		changes []silverChange
		want    map[uint64]int64
	}{
		{"two_winners_two_losers", map[uint64]int64{1: 10, 2: 100, 20: 0, 30: 0}, []silverChange{{UserID: 1, Debit: 50}, {UserID: 2, Debit: 50}, {UserID: 20, Credit: 50}, {UserID: 30, Credit: 50}}, map[uint64]int64{1: 0, 2: 50, 20: 30, 30: 30}},
		{"one_winner_three_losers", map[uint64]int64{1: 20, 2: 7, 3: 20, 30: 0}, []silverChange{{UserID: 1, Debit: 20}, {UserID: 2, Debit: 20}, {UserID: 3, Debit: 20}, {UserID: 30, Credit: 60}}, map[uint64]int64{1: 0, 2: 0, 3: 0, 30: 47}},
		{"one_winner_two_losers", map[uint64]int64{1: 7, 2: 15, 30: 0}, []silverChange{{UserID: 1, Debit: 20}, {UserID: 2, Debit: 20}, {UserID: 30, Credit: 40}}, map[uint64]int64{1: 0, 2: 0, 30: 22}},
		{"three_winners_one_loser", map[uint64]int64{1: 8, 10: 0, 20: 0, 30: 0}, []silverChange{{UserID: 1, Debit: 30}, {UserID: 10, Credit: 10}, {UserID: 20, Credit: 10}, {UserID: 30, Credit: 10}}, map[uint64]int64{1: 0, 10: 2, 20: 3, 30: 3}},
		{"two_winners_one_loser", map[uint64]int64{1: 31, 20: 0, 30: 0}, []silverChange{{UserID: 1, Debit: 40}, {UserID: 20, Credit: 20}, {UserID: 30, Credit: 20}}, map[uint64]int64{1: 0, 20: 15, 30: 16}},
		{"empty_losers", map[uint64]int64{1: 0, 2: 0, 30: 10}, []silverChange{{UserID: 1, Debit: 20}, {UserID: 2, Debit: 20}, {UserID: 30, Credit: 40}}, map[uint64]int64{1: 0, 2: 0, 30: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wallets := map[uint64]walletBalance{}
			for id, amount := range tc.amounts {
				wallets[id] = walletBalance{UserID: id, Amount: amount, Version: 1}
			}
			actual, reason := settleEqualGroup(wallets, tc.changes)
			if reason != "" {
				t.Fatal(reason)
			}
			var debit, credit int64
			for _, v := range actual {
				debit += v.Debit
				credit += v.Credit
			}
			if debit != credit || len(actual) != len(tc.amounts) {
				t.Fatalf("收支或人数错误: %+v", actual)
			}
			for id, want := range tc.want {
				if wallets[id].Amount != want {
					t.Fatalf("user=%d want=%d got=%+v", id, want, wallets[id])
				}
			}
		})
	}
}

// TestTransferGroupKeepsPaymentRelationships 保护每个输家按原明细比例封顶、余额来自事务前快照且余数按收款 UserID 降序分配。
func TestTransferGroupKeepsPaymentRelationships(t *testing.T) {
	wallets := map[uint64]walletBalance{1: {UserID: 1, Amount: 21, Version: 1}, 2: {UserID: 2, Amount: 40, Version: 1}, 20: {UserID: 20, Amount: 0, Version: 1}, 30: {UserID: 30, Amount: 0, Version: 1}}
	transfers := []settlementTransfer{{FromUserID: 1, ToUserID: 30, Amount: 30}, {FromUserID: 1, ToUserID: 20, Amount: 10}, {FromUserID: 2, ToUserID: 20, Amount: 40}}
	actual, edges, reason := settleTransferGroup(wallets, transfers)
	if reason != "" || len(actual) != 4 || len(edges) != 3 {
		t.Fatalf("结算失败: %s %v %v", reason, actual, edges)
	}
	want := map[uint64]int64{1: 0, 2: 0, 20: 45, 30: 16}
	for id, amount := range want {
		if wallets[id].Amount != amount {
			t.Fatalf("user=%d %+v", id, wallets[id])
		}
	}
	for _, e := range edges {
		if e.FromUserID == 1 && ((e.ToUserID == 30 && e.Amount != 16) || (e.ToUserID == 20 && e.Amount != 5)) {
			t.Fatalf("明细分配错误: %+v", edges)
		}
	}
}

// TestTransferGroupDoesNotSpendIncomingOrPartiallyApply 保护同组收入不能提前支付，金额溢出时所有钱包保持不变。
func TestTransferGroupDoesNotSpendIncomingOrPartiallyApply(t *testing.T) {
	wallets := map[uint64]walletBalance{1: {UserID: 1, Amount: 0, Version: 1}, 2: {UserID: 2, Amount: 10, Version: 1}, 3: {UserID: 3, Amount: 0, Version: 1}}
	_, edges, reason := settleTransferGroup(wallets, []settlementTransfer{{FromUserID: 1, ToUserID: 3, Amount: 10}, {FromUserID: 2, ToUserID: 1, Amount: 10}})
	if reason != "" || wallets[1].Amount != 10 || wallets[3].Amount != 0 || len(edges) != 2 {
		t.Fatalf("收入被提前用于付款: %v %v %s", wallets, edges, reason)
	}
	wallets = map[uint64]walletBalance{1: {UserID: 1, Amount: 1, Version: 1}, 2: {UserID: 2, Amount: math.MaxInt64, Version: 1}}
	before := map[uint64]walletBalance{1: wallets[1], 2: wallets[2]}
	_, _, reason = settleTransferGroup(wallets, []settlementTransfer{{FromUserID: 1, ToUserID: 2, Amount: 1}})
	if reason != "balance_overflow" || !reflect.DeepEqual(before, wallets) {
		t.Fatalf("溢出产生部分付款: %s %v", reason, wallets)
	}
}

// TestTransferGroupProportionsDoNotOverflow 保护接近 int64 上限时按比例计算及余数分配仍然精确。
func TestTransferGroupProportionsDoNotOverflow(t *testing.T) {
	wallets := map[uint64]walletBalance{
		1: {UserID: 1, Amount: math.MaxInt64 - 1, Version: 1},
		2: {UserID: 2, Amount: 0, Version: 1},
		3: {UserID: 3, Amount: 0, Version: 1},
	}
	_, edges, reason := settleTransferGroup(wallets, []settlementTransfer{
		{FromUserID: 1, ToUserID: 2, Amount: math.MaxInt64 - 1},
		{FromUserID: 1, ToUserID: 3, Amount: 1},
	})
	if reason != "" || edges[0].Amount != math.MaxInt64-2 || edges[1].Amount != 1 || wallets[1].Amount != 0 {
		t.Fatalf("大金额比例计算错误: %s %+v %+v", reason, edges, wallets)
	}
}

// TestCappedGroupsRejectVersionOverflowAtomically 保护新接口的版本溢出不会修改任何参与者钱包。
func TestCappedGroupsRejectVersionOverflowAtomically(t *testing.T) {
	initial := map[uint64]walletBalance{
		1: {UserID: 1, Amount: 10, Version: math.MaxUint64},
		2: {UserID: 2, Amount: 0, Version: 1},
	}
	for _, mode := range []string{"equal", "transfer"} {
		t.Run(mode, func(t *testing.T) {
			wallets := map[uint64]walletBalance{1: initial[1], 2: initial[2]}
			var reason string
			if mode == "equal" {
				_, reason = settleEqualGroup(wallets, []silverChange{{UserID: 1, Debit: 10}, {UserID: 2, Credit: 10}})
			} else {
				_, _, reason = settleTransferGroup(wallets, []settlementTransfer{{FromUserID: 1, ToUserID: 2, Amount: 10}})
			}
			if reason != "version_overflow" || !reflect.DeepEqual(wallets, initial) {
				t.Fatalf("版本溢出产生部分修改: %s %+v", reason, wallets)
			}
		})
	}
}

// TestGroupPoliciesValidateAndPreserveIdentity 保护两种新接口的边界、明细乱序等价及原 Group 摘要不变。
func TestGroupPoliciesValidateAndPreserveIdentity(t *testing.T) {
	group := map[string]any{"version": uint64(2), "operation_id": "round", "business": "landlord", "business_ref": "round", "detail": map[string]any{}, "changes": []any{
		map[string]any{"user_id": uint64(1), "reason": "game_win_loss", "debit": int64(40), "credit": int64(0)},
		map[string]any{"user_id": uint64(2), "reason": "game_win_loss", "debit": int64(0), "credit": int64(20)},
		map[string]any{"user_id": uint64(3), "reason": "game_win_loss", "debit": int64(0), "credit": int64(20)},
	}}
	legacy, err := decodeSettlementGroup(group)
	if err != nil {
		t.Fatal(err)
	}
	// 按旧结构直接编码，验证新增可选字段不改变历史请求摘要。
	old := struct {
		ID        string          `json:"operation_id"`
		Business  string          `json:"business"`
		Reference string          `json:"business_ref"`
		Changes   []silverChange  `json:"changes"`
		Detail    json.RawMessage `json:"detail"`
	}{legacy.Operations[0].ID, legacy.Operations[0].Business, legacy.Operations[0].Reference, legacy.Operations[0].Changes, legacy.Operations[0].Detail}
	encoded, _ := json.Marshal(old)
	if legacy.Operations[0].digest() != sha256.Sum256(encoded) {
		t.Fatal("旧操作摘要发生变化")
	}
	equal, err := decodeEqualGroup(group)
	if err != nil || equal.Operations[0].digest() == legacy.Operations[0].digest() {
		t.Fatal("新旧策略未隔离")
	}
	changes := group["changes"].([]any)
	changes[2].(map[string]any)["credit"] = int64(19)
	if _, err := decodeEqualGroup(group); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("赢家不等额或收支不平被接纳")
	}
	changes[0].(map[string]any)["debit"] = int64(31)
	changes[1].(map[string]any)["credit"] = int64(15)
	changes[2].(map[string]any)["credit"] = int64(16)
	if _, err := decodeEqualGroup(group); err != nil {
		t.Fatalf("理论奖池余数按 UserID 降序分配应被接纳: %v", err)
	}
	changes[1].(map[string]any)["credit"] = int64(16)
	changes[2].(map[string]any)["credit"] = int64(15)
	if _, err := decodeEqualGroup(group); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("理论余数未按 UserID 降序分配")
	}
	changes[0].(map[string]any)["debit"] = int64(1)
	changes[1].(map[string]any)["credit"] = int64(0)
	changes[2].(map[string]any)["credit"] = int64(1)
	if _, err := decodeEqualGroup(group); err != nil {
		t.Fatalf("理论收益为零的赢家不应被排除: %v", err)
	}
	request := map[string]any{"version": uint64(2), "operation_id": "mahjong", "business": "mahjong", "business_ref": "round", "detail": map[string]any{}, "transfers": []any{
		map[string]any{"from_user_id": uint64(1), "to_user_id": uint64(3), "amount": int64(30)},
		map[string]any{"from_user_id": uint64(1), "to_user_id": uint64(2), "amount": int64(10)},
	}}
	first, err := decodeTransferGroup(request)
	if err != nil {
		t.Fatal(err)
	}
	values := request["transfers"].([]any)
	values[0], values[1] = values[1], values[0]
	second, err := decodeTransferGroup(request)
	if err != nil || first.digest() != second.digest() {
		t.Fatal("明细重排改变身份")
	}
	values[1] = values[0]
	if _, err := decodeTransferGroup(request); !errors.Is(err, errInvalidSettlement) {
		t.Fatal("重复付款关系被接受")
	}
}
