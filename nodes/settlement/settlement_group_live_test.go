package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// TestSettlementGroupPoliciesLiveMySQL 保护两个真实 Call 接口原子入账、实际回执持久化、并发原 ID 重试和历史回执不随余额变化。
func TestSettlementGroupPoliciesLiveMySQL(t *testing.T) {
	store, _ := liveSettlementStore(t)
	store.db.SetMaxOpenConns(10)
	sample := newLiveSettlementSample(t, store, 4)
	amounts := []int64{21, 40, 0, 0}
	for i, uid := range sample.Users {
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		_, err := store.db.ExecContext(ctx, "UPDATE silver_balances SET amount=? WHERE user_id=?", amounts[i], uid)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	address, stop := startSettlementTestNode(t, store.db)
	call := func(cmd runtime.CommandID, request map[string]any) map[string]any {
		t.Helper()
		value, err := retryLiveSettlement(t, address, settlementWriteName, cmd, request)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	id := "capped-equal-" + rand.Text()
	sample.Operations = append(sample.Operations, id)
	sample.Batches = append(sample.Batches, derivedBatchID("group-equal", id))
	changes := []any{}
	for i, uid := range sample.Users {
		debit, credit := int64(0), int64(0)
		if i < 2 {
			debit = 40
		} else {
			credit = 40
		}
		changes = append(changes, map[string]any{"user_id": uid, "reason": "game_win_loss", "debit": debit, "credit": credit})
	}
	request := map[string]any{"version": uint64(2), "operation_id": id, "business": "cards", "business_ref": id, "detail": map[string]any{}, "changes": changes}
	type result struct {
		receipt map[string]any
		err     error
	}
	responses := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := retryLiveSettlement(t, address, settlementWriteName, settleEqualGroupCommand, request)
			responses <- result{receipt, err}
		}()
	}
	defer wg.Wait()
	var first map[string]any
	for range 8 {
		r := <-responses
		if r.err != nil {
			t.Fatal(r.err)
		}
		if first == nil {
			first = r.receipt
		} else if !reflect.DeepEqual(first, r.receipt) {
			t.Fatal("并发重试未返回相同回执")
		}
	}
	item := first["results"].([]any)[0].(map[string]any)
	if item["status"] != "committed" {
		t.Fatal(item)
	}
	actual := map[uint64]map[string]any{}
	for _, v := range item["actual_changes"].([]any) {
		r := v.(map[string]any)
		actual[r["user_id"].(uint64)] = r
	}
	higher, lower := sample.Users[2], sample.Users[3]
	if higher < lower {
		higher, lower = lower, higher
	}
	if actual[sample.Users[0]]["debit"] != int64(21) || actual[sample.Users[1]]["debit"] != int64(40) || actual[higher]["credit"] != int64(31) || actual[lower]["credit"] != int64(30) {
		t.Fatalf("平均分配错误: %v", actual)
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	var debit, credit int64
	err := store.db.QueryRowContext(ctx, "SELECT debit,credit FROM settlement_operations WHERE operation_id=?", id).Scan(&debit, &credit)
	cancel()
	if err != nil || debit != 61 || credit != 61 {
		t.Fatalf("账务索引不是实际扣返: %d %d %v", debit, credit, err)
	}
	// 再次修改余额并重启服务，原请求必须返回原来的实际扣返和历史余额。
	singleID := "capped-later-" + rand.Text()
	sample.Operations = append(sample.Operations, singleID)
	sample.Batches = append(sample.Batches, derivedBatchID("single", singleID))
	single := testSingle(singleID, sample.Users[0], 0, 100)
	single["version"] = uint64(2)
	call(settleSingleCommand, single)
	stop()
	address, _ = startSettlementTestNode(t, store.db)
	if again := call(settleEqualGroupCommand, request); !reflect.DeepEqual(first, again) {
		t.Fatal("重启重试改变了原回执")
	}
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	var stored []byte
	err = store.db.QueryRowContext(ctx, "SELECT receipt_json FROM settlement_batches WHERE batch_id=?", derivedBatchID("group-equal", id)).Scan(&stored)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	var broken map[string]any
	if err := json.Unmarshal(stored, &broken); err != nil {
		t.Fatal(err)
	}
	delete(broken["results"].([]any)[0].(map[string]any), "actual_changes")
	encoded, err := json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	_, err = store.db.ExecContext(ctx, "UPDATE settlement_batches SET receipt_json=? WHERE batch_id=?", encoded, derivedBatchID("group-equal", id))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", settlementWriteName, settleEqualGroupCommand, request); !errors.Is(err, errSettlementUnavailable) {
		t.Fatalf("损坏回执被当成成功: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	_, err = store.db.ExecContext(ctx, "UPDATE settlement_batches SET receipt_json=? WHERE batch_id=?", stored, derivedBatchID("group-equal", id))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	// 第二接口保留付款关系；该玩家刚获得 100，按比例封顶并按 UserID 分配余数。
	tid := "capped-transfer-" + rand.Text()
	sample.Operations = append(sample.Operations, tid)
	sample.Batches = append(sample.Batches, derivedBatchID("group-transfer", tid))
	transfer := map[string]any{"version": uint64(2), "operation_id": tid, "business": "mahjong", "business_ref": tid, "detail": map[string]any{}, "transfers": []any{
		map[string]any{"from_user_id": sample.Users[0], "to_user_id": sample.Users[2], "amount": int64(150)},
		map[string]any{"from_user_id": sample.Users[0], "to_user_id": sample.Users[3], "amount": int64(50)},
	}}
	receipt := call(settleTransferGroupCommand, transfer)
	row := receipt["results"].([]any)[0].(map[string]any)
	edges := row["actual_transfers"].([]any)
	got := map[uint64]int64{}
	for _, e := range edges {
		v := e.(map[string]any)
		got[v["to_user_id"].(uint64)] = v["amount"].(int64)
	}
	if row["status"] != "committed" || got[sample.Users[2]] != 75 || got[sample.Users[3]] != 25 {
		t.Fatalf("按明细分配错误: %v", row)
	}
	if again := call(settleTransferGroupCommand, transfer); !reflect.DeepEqual(receipt, again) {
		t.Fatal("明细重复提交改变回执")
	}
	// 已有全局 OperationID 不能切换到另一种结算策略。
	transfer["operation_id"], transfer["business_ref"] = id, id
	if _, err := businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", settlementWriteName, settleTransferGroupCommand, transfer); !errors.Is(err, errSettlementConflict) {
		t.Fatal("跨策略复用身份被接受")
	}

	// 全部输家为零时仍提交合法零结果，不应因钱包未变化导致 SQL 更新数错误。
	zero := newLiveSettlementSample(t, store, 2)
	for _, uid := range zero.Users {
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		_, err := store.db.ExecContext(ctx, "UPDATE silver_balances SET amount=0 WHERE user_id=?", uid)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	zeroID := "capped-zero-" + rand.Text()
	zero.Operations = append(zero.Operations, zeroID)
	zero.Batches = append(zero.Batches, derivedBatchID("group-equal", zeroID))
	zeroRequest := liveGroupRequest(zeroID, "cards", zero.Users[0], zero.Users[1], 10)
	zeroReceipt := call(settleEqualGroupCommand, zeroRequest)
	zeroResult := zeroReceipt["results"].([]any)[0].(map[string]any)
	if zeroResult["status"] != "committed" {
		t.Fatalf("全零结算未完成: %v", zeroResult)
	}
	for _, value := range zeroResult["actual_changes"].([]any) {
		row := value.(map[string]any)
		if row["debit"] != int64(0) || row["credit"] != int64(0) {
			t.Fatalf("全零结算发生扣返: %v", row)
		}
	}
	for _, value := range zeroReceipt["balances"].([]any) {
		if value.(map[string]any)["version"] != uint64(1) {
			t.Fatal("未变化的钱包版本发生变化")
		}
	}
	queried, err := businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", settlementReadName, getOperationCommand, zero.Users[0], zeroID)
	if err != nil || queried.(map[string]any)["actual_changes"] != nil {
		t.Fatalf("参与者查询泄漏了整组明细: %v %v", queried, err)
	}

	// 余额溢出时事务回滚，错误不会变成不可重试的 rejected 终态。
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	_, err = store.db.ExecContext(ctx, "UPDATE silver_balances SET amount=CASE user_id WHEN ? THEN 1 ELSE ? END WHERE user_id IN (?,?)", zero.Users[0], int64(math.MaxInt64), zero.Users[0], zero.Users[1])
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	overflowID := "capped-overflow-" + rand.Text()
	zero.Operations = append(zero.Operations, overflowID)
	zero.Batches = append(zero.Batches, derivedBatchID("group-equal", overflowID))
	overflowRequest := liveGroupRequest(overflowID, "cards", zero.Users[0], zero.Users[1], 1)
	if _, err := businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", settlementWriteName, settleEqualGroupCommand, overflowRequest); !errors.Is(err, errBalanceOverflow) {
		t.Fatalf("溢出未返回确定性错误: %v", err)
	}
	if previous, err := businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", settlementReadName, getBatchCommand, derivedBatchID("group-equal", overflowID)); err != nil || previous != nil {
		t.Fatalf("溢出写入了终态: %v %v", previous, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	_, err = store.db.ExecContext(ctx, "UPDATE silver_balances SET amount=0 WHERE user_id=?", zero.Users[1])
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if result := call(settleEqualGroupCommand, overflowRequest)["results"].([]any)[0].(map[string]any); result["status"] != "committed" {
		t.Fatalf("异常修复后原请求不能重试: %v", result)
	}
}
