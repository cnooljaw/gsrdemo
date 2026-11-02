package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// verifyAccessSettlementRejection 模拟其他业务先扣款，验证最终拒绝及客户端绝对余额校正，不直接修改数据库余额。
func verifyAccessSettlementRejection(t *testing.T, ctx context.Context, gateAddress string,
	player *accessDemoPlayer, db *sql.DB, operationIDs *[]string, settlementAddress, redisAddress string) {
	t.Helper()
	client, snapshot := accessLoginAndEnter(t, gateAddress, player)
	var amount int64
	var version uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
		Scan(&amount, &version); err != nil || amount < 100 {
		t.Fatalf("拒绝演练需要有初始余额: amount=%d err=%v", amount, err)
	}
	baseline := waitAccessMoleCheckpoint(t, ctx, redisAddress, player.id, func(state accessMoleCheckpoint) bool {
		return state.Pending == nil
	})
	suffix, err := connectionID()
	if err != nil {
		t.Fatal(err)
	}
	drainID := "access-drain-" + suffix
	*operationIDs = append(*operationIDs, drainID)
	result := accessSettlementWrite(t, ctx, settlementAddress, drainID, 6, map[string]any{
		"version": uint64(2), "operation_id": drainID, "business": "test", "business_ref": "other-business",
		"user_id": player.id, "detail": map[string]any{"case": "cached-balance"},
		"items": []any{map[string]any{"reason": "test_debit", "debit": amount, "credit": int64(0)}},
	})
	receipt, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("其他业务扣款回执: %v", result)
	}
	results, ok := receipt["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("其他业务扣款结果: %v", receipt)
	}
	drained, ok := results[0].(map[string]any)
	if !ok || drained["operation_id"] != drainID || drained["status"] != "committed" {
		t.Fatalf("其他业务扣款未提交: %v", receipt)
	}
	var drainedAmount int64
	var drainedVersion uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
		Scan(&drainedAmount, &drainedVersion); err != nil || drainedAmount != 0 || drainedVersion != version+1 {
		t.Fatalf("其他业务扣款账务差异: amount=%d version=%d err=%v", drainedAmount, drainedVersion, err)
	}

	// Mole 的缓存仍有余额，允许即时反馈；最终合法性由 Settlement 裁决。
	hit, pushes := accessHitTargetResponse(t, client, snapshot, "cached-balance-after-other-business")
	*operationIDs = append(*operationIDs, hit.Hit.HitId)
	if hit.Silver == nil || hit.Silver.Balance != amount-hit.Hit.Debit+hit.Hit.Credit {
		t.Fatalf("未验证到缓存即时反馈: %v", hit)
	}
	corrected := false
	observe := func(envelope *commonv1.Envelope) {
		if envelope.MsgId != uint32(demopb.MsgId_S2C_BALANCE_PUSH) {
			return
		}
		var push demopb.BalancePush
		if err := proto.Unmarshal(envelope.Payload, &push); err != nil {
			t.Fatal(err)
		}
		balance := push.Silver
		if balance == nil || balance.Confirmed == nil || balance.Balance != 0 || balance.Available != 0 ||
			balance.Confirmed.Amount != 0 || balance.Confirmed.Version != drainedVersion || balance.Revision <= hit.Silver.Revision {
			t.Fatalf("余额校正错误: hit=%v push=%v", hit, &push)
		}
		corrected = true
	}
	for _, push := range pushes {
		observe(push)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !corrected && time.Now().Before(deadline) {
		observe(readAccessEnvelope(t, client))
	}
	if !corrected {
		t.Fatal("最终拒绝后未收到当前连接的余额校正")
	}
	var operation demopb.OperationResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_OPERATION, &demopb.OperationRequest{HitId: hit.Hit.HitId}, &operation)
	if !operation.Found || operation.Status != "rejected" || operation.Reason != "insufficient_silver" {
		t.Fatalf("未取得最终拒绝: %v", &operation)
	}
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
		Scan(&amount, &version); err != nil || amount != 0 || version != drainedVersion {
		t.Fatalf("拒绝的命中修改了钱包: amount=%d version=%d err=%v", amount, version, err)
	}
	for _, id := range []string{drainID, hit.Hit.HitId} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("扣款或拒绝记录不唯一: count=%d err=%v", count, err)
		}
	}
	final := waitAccessMoleCheckpoint(t, ctx, redisAddress, player.id, func(state accessMoleCheckpoint) bool {
		return state.Pending == nil && state.Sequence == baseline.Sequence+1
	})
	if final.Pool != baseline.Pool {
		t.Fatal("被拒绝命中不应计入奖池统计")
	}
	var leave demopb.LeaveResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_LEAVE, &demopb.LeaveRequest{}, &leave)
	if !leave.Left {
		t.Fatal("余额校正后无法离开")
	}
	t.Log("其他业务通过 Settlement 扣至零后，Mole 仍即时接受缓存命中；最终拒绝为 insufficient_silver，客户端余额校正为零，钱包未再次变更、奖池未累计")
}
