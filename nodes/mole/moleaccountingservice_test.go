package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

func exampleHit(id string, userID uint64) hitEvent {
	return hitEvent{HitID: id, UserID: userID, ActionID: "action-" + id, AttackName: "attack-test",
		Sequence: 1, Debit: hammerCost, Rule: moleRuleVersion}
}

type submissionSink struct {
	gsr.ServiceContext
	reject  bool
	batches []moleBatch
}

func (sink *submissionSink) SendName(name cluster.ServiceName, command runtime.CommandID, args ...any) error {
	if name != moleSubmissionName || command != submissionBatchCommand {
		return errInvalidMoleInput
	}
	if sink.reject {
		return runtime.ErrMailboxFull
	}
	sink.batches = append(sink.batches, args[0].(moleBatch))
	return nil
}

// TestMoleHandoffRetainsAcceptedOperationsOnBusy 保护本地提交队列拒绝时不丢内存账，成功后只交接一个独立批次。
func TestMoleHandoffRetainsAcceptedOperationsOnBusy(t *testing.T) {
	service := fundedAccounting(t, 10000)
	for index := range 2 {
		event := exampleHit(fmt.Sprintf("hit-%d", index), 7)
		event.AttackName = fmt.Sprintf("attack-%d", index)
		if _, err := service.accept(event, false); err != nil {
			t.Fatal(err)
		}
	}
	sink := &submissionSink{reject: true}
	if err := service.flush(sink); !errors.Is(err, runtime.ErrMailboxFull) || len(service.buffer) != 2 || service.inFlightID != "" {
		t.Fatalf("failed admission lost accepted operations: %v", err)
	}
	sink.reject = false
	if err := service.flush(sink); err != nil {
		t.Fatal(err)
	}
	if err := service.flush(sink); err != nil || len(sink.batches) != 1 || len(service.buffer) != 0 {
		t.Fatalf("duplicate handoff: %v", err)
	}
	if len(sink.batches[0].Events) != 2 || sink.batches[0].Events[0].AttackName == sink.batches[0].Events[1].AttackName {
		t.Fatal("cross-group events were not merged")
	}
	sink.batches[0].Events[0].Credit = 198
	if service.pending["hit-0"].Result.Event.Credit != 0 {
		t.Fatal("submission shared accounting's mutable event slice")
	}
}

func fundedAccounting(t *testing.T, amount int64) *moleAccountingService {
	t.Helper()
	service := newMoleAccountingService()
	if err := service.initPool(nil, molePool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.setBalance(nil, 7, silverBalance{Amount: amount, Version: 1}.snapshot()); err != nil {
		t.Fatal(err)
	}
	return service
}

func accountAvailable(t *testing.T, service *moleAccountingService, userID uint64) int64 {
	t.Helper()
	_, available, err := service.balanceAmounts(userID)
	if err != nil {
		t.Fatal(err)
	}
	return available
}

// TestMemoryHitReturnsFullResultAndDoesNotSpendPendingCredit 保护一次内存记账返回完整扣返，显示奖励但不消费未确认返还。
func TestMemoryHitReturnsFullResultAndDoesNotSpendPendingCredit(t *testing.T) {
	service := fundedAccounting(t, 200)
	first := exampleHit("hit-1", 7)
	first.Credit = hammerReward
	result, err := service.accept(first, false)
	if err != nil || result.Balance != 298 || result.Available != 100 {
		t.Fatalf("memory result: %+v %v", result, err)
	}
	again, err := service.accept(first, true)
	if err != nil || again != result || len(service.pending) != 1 {
		t.Fatalf("retry changed result: %+v %v", again, err)
	}
	second := exampleHit("hit-2", 7)
	if _, err := service.accept(second, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.accept(exampleHit("hit-3", 7), false); !errors.Is(err, errInsufficientSilver) {
		t.Fatalf("pending reward was spent: %v", err)
	}
	if accountAvailable(t, service, 7) != 0 {
		t.Fatal("rejection released earlier debits")
	}
}

// TestMemoryHitRequiresInitializationAndNeverReappliesUnknownRetry 保护未知余额及已淘汰结果的重试不重新扣款。
func TestMemoryHitRequiresInitializationAndNeverReappliesUnknownRetry(t *testing.T) {
	service := newMoleAccountingService()
	event := exampleHit("hit-1", 7)
	if _, err := service.resumeBalance(nil, 7); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("uninitialized resume became a cache miss: %v", err)
	}
	if _, err := service.accept(event, false); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("uninitialized hit: %v", err)
	}
	service = fundedAccounting(t, 1000)
	if _, err := service.accept(event, true); !errors.Is(err, errMoleOperationUnknown) {
		t.Fatalf("unknown retry was executed: %v", err)
	}
	if len(service.pending) != 0 {
		t.Fatal("unknown retry modified balance")
	}
	if _, err := service.accept(event, false); err != nil {
		t.Fatal(err)
	}
	changed := event
	changed.Credit = hammerReward
	if _, err := service.accept(changed, true); !errors.Is(err, errMoleConflict) {
		t.Fatalf("conflicting retry: %v", err)
	}
}

// TestSettlementReconcilesProjectionWithoutDoubleSubtracting 保护最终余额加剩余内存操作重建显示，重复结果不重复记账。
func TestSettlementReconcilesProjectionWithoutDoubleSubtracting(t *testing.T) {
	service := fundedAccounting(t, 300)
	first, second := exampleHit("hit-1", 7), exampleHit("hit-2", 7)
	for _, event := range []hitEvent{first, second} {
		if _, err := service.accept(event, false); err != nil {
			t.Fatal(err)
		}
	}
	receipt := batchReceipt{BatchID: "batch-1", Results: []operationResult{{HitID: first.HitID, Status: "committed", OriginBatch: "batch-1"}},
		Balances: map[uint64]silverBalance{7: {Amount: 200, Version: 2}}}
	service.applyReceipt(receipt)
	service.applyReceipt(receipt)
	view, err := service.balanceView(7)
	if err != nil || view["balance"] != int64(100) || view["available"] != int64(100) || service.pool.Hits != 1 {
		t.Fatalf("double applied result: %v %+v %v", view, service.pool, err)
	}
	service.updateBalance(7, silverBalance{Amount: 999, Version: 1})
	if accountAvailable(t, service, 7) != 100 {
		t.Fatal("old version overwrote current balance")
	}
	if _, err := service.accept(first, true); err != nil {
		t.Fatal("completed memory result was not retained")
	}
}

// TestMoleWireContainsFactsWithoutConnections 保护发送给结算节点的载荷只有资金事实和业务来源。
func TestMoleWireContainsFactsWithoutConnections(t *testing.T) {
	event := exampleHit("hit-1", 7)
	operation := event.operationWire()
	for _, key := range []string{"login_id", "access_node", "agent_name"} {
		if _, exists := operation[key]; exists {
			t.Fatalf("connection field leaked: %s", key)
		}
	}
	if operation["operation_id"] != event.HitID || operation["business_ref"] != "attack-test/1/0" {
		t.Fatal("missing stable business identity")
	}
	items := operation["items"].([]any)
	cost, reward := items[0].(map[string]any), items[1].(map[string]any)
	if operation["user_id"] != uint64(7) || cost["reason"] != "hammer_cost" || cost["debit"] != int64(100) || reward["reason"] != "hammer_reward" || reward["credit"] != int64(0) {
		t.Fatal("invalid single-user money change")
	}
	if _, exists := operation["changes"]; exists {
		t.Fatal("single-user batch used group settlement fields")
	}
}

// TestMoleBatchLimitAndSnapshotOwnership 保护跨组批次有序且不超载荷上限，传递快照不暴露调用方切片。
func TestMoleBatchLimitAndSnapshotOwnership(t *testing.T) {
	batch := moleBatch{ID: "batch-1"}
	for index := range maxBatchOperations {
		event := exampleHit(fmt.Sprintf("hit-%d", index), 7)
		event.AttackName = fmt.Sprintf("attack-%d", index%2)
		batch.Events = append(batch.Events, event)
	}
	if batchSize(batch) > maxBatchBytes {
		t.Fatal("normal batch exceeds byte bound")
	}
	copied := batch.clone()
	copied.Events[0].Credit = 198
	if batch.Events[0].Credit != 0 {
		t.Fatal("batch snapshot shared mutable event slice")
	}
}

// TestMoleReceiptRequiresEveryOperationAndBalance 保护畸形或部分回复不会释放待扣。
func TestMoleReceiptRequiresEveryOperationAndBalance(t *testing.T) {
	batch := moleBatch{ID: "batch-1", Events: []hitEvent{exampleHit("hit-1", 7)}}
	valid := func() map[string]any {
		return map[string]any{"batch_id": batch.ID,
			"results":  []any{map[string]any{"operation_id": "hit-1", "status": "committed", "origin_batch": batch.ID}},
			"balances": []any{map[string]any{"user_id": uint64(7), "amount": int64(0), "version": uint64(2)}}}
	}
	if _, err := decodeReceipt(valid(), batch); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(value map[string]any) { value["results"] = []any{} },
		func(value map[string]any) { value["balances"] = []any{} },
		func(value map[string]any) { value["balances"].([]any)[0].(map[string]any)["amount"] = int64(-1) },
		func(value map[string]any) { value["results"].([]any)[0].(map[string]any)["status"] = "unknown" },
	} {
		value := valid()
		mutate(value)
		if _, err := decodeReceipt(value, batch); err == nil {
			t.Fatalf("accepted invalid receipt: %v", value)
		}
	}
}

// TestMoleAccountLeaveAndReenterKeepsPendingDebit 保护离开时保留待扣，重进取消延迟回收，结清后离开可回收余额。
func TestMoleAccountLeaveAndReenterKeepsPendingDebit(t *testing.T) {
	service := fundedAccounting(t, 1000)
	event := exampleHit("hit-1", 7)
	if _, err := service.accept(event, false); err != nil {
		t.Fatal(err)
	}
	if err := service.forgetBalance(nil, 7); err != nil {
		t.Fatal(err)
	}
	view, err := service.loadBalance(nil, 7)
	if err != nil || view["available"] != int64(900) {
		t.Fatalf("leaving lost pending debit: %v %v", view, err)
	}
	resumed, err := service.resumeBalance(nil, 7)
	if err != nil || resumed == nil || resumed.(map[string]any)["available"] != int64(900) {
		t.Fatalf("resume lost pending debit: %v %v", resumed, err)
	}
	service.applyReceipt(batchReceipt{BatchID: "batch-1",
		Results:  []operationResult{{HitID: event.HitID, Status: "committed", OriginBatch: "batch-1"}},
		Balances: map[uint64]silverBalance{7: {Amount: 900, Version: 2}}})
	service.evictIfIdle(7)
	view, err = service.loadBalance(nil, 7)
	if err != nil || view["available"] != int64(900) {
		t.Fatalf("reenter did not cancel delayed eviction: %v %v", view, err)
	}
	if err := service.forgetBalance(nil, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := service.loadBalance(nil, 7); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("idle departed account was retained: %v", err)
	}
	if resumed, err := service.resumeBalance(nil, 7); err != nil || resumed != nil {
		t.Fatalf("cache miss was not distinguished from failure: %v %v", resumed, err)
	}
}

// TestMoleBalanceQueryCannotRebasePendingHits 保护查询返回前新增命中时，普通余额快照不重复计入已处理但未回执的操作。
func TestMoleBalanceQueryCannotRebasePendingHits(t *testing.T) {
	for _, credit := range []int64{0, hammerReward} {
		t.Run(fmt.Sprintf("credit-%d", credit), func(t *testing.T) {
			service := fundedAccounting(t, 1000)
			first := exampleHit("hit-1", 7)
			first.Credit = credit
			if _, err := service.accept(first, false); err != nil {
				t.Fatal(err)
			}
			// 远端快照已经包含第一锤，但回执尚未到达。
			queried := silverBalance{Amount: 900 + credit, Version: 2}
			second, err := service.accept(exampleHit("hit-2", 7), false)
			if err != nil {
				t.Fatal(err)
			}
			view, err := service.setBalance(nil, 7, queried.snapshot())
			if err != nil || view["balance"] != second.Balance || view["confirmed_balance"] != int64(1000) ||
				view["version"] != uint64(1) || view["revision"] != second.Revision || view["available"] != int64(800) {
				t.Fatalf("query rebased pending hits: %v %v", view, err)
			}
			service.applyReceipt(batchReceipt{BatchID: "batch-1",
				Results:  []operationResult{{HitID: first.HitID, Status: "committed", OriginBatch: "batch-1"}},
				Balances: map[uint64]silverBalance{7: queried}})
			view, err = service.balanceView(7)
			if err != nil || view["balance"] != second.Balance || view["available"] != int64(800)+credit || len(service.pending) != 1 {
				t.Fatalf("receipt lost later hit: %v %v", view, err)
			}
			// 其他用户有待确认操作不应妨碍本用户初始化/刷新。
			view, err = service.setBalance(nil, 8, silverBalance{Amount: 500, Version: 1}.snapshot())
			if err != nil || view["confirmed_balance"] != int64(500) {
				t.Fatalf("other user's pending hit blocked balance load: %v %v", view, err)
			}
		})
	}
}
