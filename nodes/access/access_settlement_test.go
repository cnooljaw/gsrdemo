package main

import (
	"context"
	"database/sql"
	"os"
	"testing"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// verifyAccessSettlementRecovery 只重启本用例的结算进程，验证不假报完成、源端补齐原操作及客户端不重复命中。
func verifyAccessSettlementRecovery(t *testing.T, ctx context.Context, gateAddress string,
	player *accessDemoPlayer, db *sql.DB, operationIDs *[]string, process *os.Process,
	binary, config, address string) {
	t.Helper()
	client, snapshot := accessLoginAndEnter(t, gateAddress, player)
	var before int64
	var previousVersion uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
		Scan(&before, &previousVersion); err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitAccessProcessListenerClosed(t, address)
	hit := accessHitTarget(t, client, snapshot, "while-settlement-unavailable")
	*operationIDs = append(*operationIDs, hit.HitId)
	response := accessRequest(t, client, demopb.MsgId_C2S_OPERATION, &demopb.OperationRequest{HitId: hit.HitId})
	if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatalf("结算不可用时不能把查询当成入账结果: %v", response)
	}
	var failure commonv1.ErrorResponse
	if err := proto.Unmarshal(response.Payload, &failure); err != nil || failure.Code != commonv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("结算不可用应明确返回失败: %v %v", &failure, err)
	}
	var amount int64
	var version uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
		Scan(&amount, &version); err != nil || amount != before || version != previousVersion {
		t.Fatalf("结算停止时钱包被改动: amount=%d version=%d err=%v", amount, version, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", hit.HitId).Scan(&count); err != nil || count != 0 {
		t.Fatalf("结算停止时出现账务记录: count=%d err=%v", count, err)
	}

	// 客户端不再提交 Hit，只让 Mole 按已接纳的原 ID 恢复提交。
	startAccessProcess(t, binary, []string{"-config", config}, address)
	accessWaitCommitted(t, client, hit.HitId)
	accessWaitCommitted(t, client, hit.HitId)
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
		Scan(&amount, &version); err != nil {
		t.Fatal(err)
	}
	if amount != before-hit.Debit+hit.Credit || version != previousVersion+1 {
		t.Fatalf("恢复后未按原操作补齐: amount=%d version=%d hit=%v", amount, version, hit)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", hit.HitId).Scan(&count); err != nil || count != 1 {
		t.Fatalf("恢复后丢账或重复入账: count=%d err=%v", count, err)
	}
	var leave demopb.LeaveResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_LEAVE, &demopb.LeaveRequest{}, &leave)
	if !leave.Left {
		t.Fatal("结算恢复后无法离开")
	}
	t.Logf("Settlement 停止期间 hit 仅返回内存结果、查询明确失败且钱包未变；重启后原 HitID 补齐一次，余额 %d，版本 %d → %d", amount, previousVersion, version)
}
