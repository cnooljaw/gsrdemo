package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/go-sql-driver/mysql"
)

func liveSettlementStore(t *testing.T) (settlementStore, settlementConfig) {
	t.Helper()
	if os.Getenv("GSRDEMO_SETTLEMENT_MYSQL") != "1" {
		t.Skip("set GSRDEMO_SETTLEMENT_MYSQL=1 after initializing gsrdemo_settlement")
	}
	config, err := loadSettlementConfig("../../configs/settlement-01.local.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := mysql.ParseDSN(config.MySQLDSN)
	if err != nil || dsn.DBName != "gsrdemo_settlement" || dsn.Net != "tcp" || dsn.Addr != "127.0.0.1:3306" {
		t.Fatal("live test requires the dedicated local gsrdemo_settlement database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := openSettlementDatabase(ctx, config.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return settlementStore{db: db}, config
}

type liveSettlementSample struct {
	store      settlementStore
	Business   string
	Users      []uint64
	Operations []string
	Batches    []string
}

func newLiveSettlementSample(t *testing.T, store settlementStore, count int) *liveSettlementSample {
	t.Helper()
	return newLiveSettlementSampleAmount(t, store, count, 1000)
}

func newLiveSettlementSampleAmount(t *testing.T, store settlementStore, count int, amount int64) *liveSettlementSample {
	t.Helper()
	sample := &liveSettlementSample{store: store}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		// 压测样本较多，按有界块删除自有身份，避免逐行清理拖长测试。
		for _, table := range []struct {
			name, key string
			ids       []any
		}{
			{"settlement_operations", "operation_id", stringSQLArgs(sample.Operations)},
			{"settlement_batches", "batch_id", stringSQLArgs(sample.Batches)},
			{"silver_balances", "user_id", userSQLArgs(sample.Users)},
		} {
			for start := 0; start < len(table.ids); start += 500 {
				end := min(start+500, len(table.ids))
				if _, err := tx.ExecContext(ctx, "DELETE FROM "+table.name+" WHERE "+table.key+" IN ("+sqlSlots(end-start)+")", table.ids[start:end]...); err != nil {
					t.Error(err)
					return
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	})
	for range count {
		var bytes [8]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			t.Fatal(err)
		}
		id := (binary.BigEndian.Uint64(bytes[:]) & ((1 << 62) - 1)) + 1
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		_, err := store.getBalance(ctx, id)
		cancel()
		if !errors.Is(err, errWalletNotFound) {
			t.Fatalf("test wallet ID not unused: %v", err)
		}
		sample.Users = append(sample.Users, id)
		op := fmt.Sprintf("wallet-init-%d", id)
		sample.Operations = append(sample.Operations, op)
		sample.Batches = append(sample.Batches, derivedBatchID("init", op))
		ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
		_, err = store.initialize(ctx, id, amount)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	return sample
}

func liveCall(t *testing.T, address string, command runtime.CommandID, args ...any) map[string]any {
	t.Helper()
	value, err := settlementRPC(t, address, command, args...)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("unexpected reply: %T", value)
	}
	return result
}

// TestSettlementLiveMySQL 保护真实事务上的三种入口、跨批次去重、多人原子性、重启及原始明细对账。
func TestSettlementLiveMySQL(t *testing.T) {
	store, _ := liveSettlementStore(t)
	sample := newLiveSettlementSample(t, store, 3)
	users := sample.Users
	prefix := "live-" + rand.Text()
	business := prefix
	address, stop := startSettlementTestNode(t, store.db)
	single := testSingle(prefix+"-single", users[0], 100, 198)
	single["items"] = []any{
		map[string]any{"reason": "service_fee", "debit": int64(5), "credit": int64(0)},
		map[string]any{"reason": "game_win_loss", "debit": int64(95), "credit": int64(198)},
	}
	single["business"] = business
	single["version"] = uint64(2)
	sample.Operations = append(sample.Operations, prefix+"-single")
	sample.Batches = append(sample.Batches, derivedBatchID("single", prefix+"-single"))
	result := liveCall(t, address, settleSingleCommand, single)
	if result["results"].([]any)[0].(map[string]any)["status"] != "committed" {
		t.Fatal("single failed")
	}
	// 同一显式初始化命令安全重试，不得重置已经使用过的钱包。
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	if _, err := store.initialize(ctx, users[0], 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.initialize(ctx, users[0], 999); !errors.Is(err, errSettlementConflict) {
		t.Fatalf("changed initial amount: %v", err)
	}
	cancel()
	batchOps := []map[string]any{testSingle(prefix+"-a", users[0], 1000, 0), testSingle(prefix+"-bad", users[1], 2000, 0), testSingle(prefix+"-b", users[2], 0, 50)}
	for _, op := range batchOps {
		op["business"] = business
		sample.Operations = append(sample.Operations, op["operation_id"].(string))
	}
	batchID := prefix + "-batch"
	sample.Batches = append(sample.Batches, batchID)
	result = liveCall(t, address, settleBatchCommand, testBatch(batchID, batchOps...))
	statuses := result["results"].([]any)
	if statuses[0].(map[string]any)["status"] != "committed" || statuses[1].(map[string]any)["status"] != "rejected" || statuses[2].(map[string]any)["status"] != "committed" {
		t.Fatal("independent batch outcomes lost")
	}
	group := map[string]any{"version": uint64(2), "operation_id": prefix + "-group-bad", "business": business, "business_ref": "round", "detail": map[string]any{"round": "test"}, "changes": []any{
		map[string]any{"user_id": users[0], "reason": "game_win_loss", "debit": int64(100), "credit": int64(0)}, map[string]any{"user_id": users[1], "reason": "game_win_loss", "debit": int64(0), "credit": int64(50)}, map[string]any{"user_id": users[2], "reason": "game_win_loss", "debit": int64(0), "credit": int64(50)}}}
	sample.Operations = append(sample.Operations, prefix+"-group-bad")
	sample.Batches = append(sample.Batches, derivedBatchID("group", prefix+"-group-bad"))
	result = liveCall(t, address, settleGroupCommand, group)
	if result["results"].([]any)[0].(map[string]any)["status"] != "rejected" {
		t.Fatal("insufficient group not rejected")
	}
	if liveCall(t, address, getBalanceCommand, users[1])["amount"] != int64(1000) {
		t.Fatal("rejected group paid a winner")
	}
	group["operation_id"] = prefix + "-group"
	group["changes"].([]any)[0].(map[string]any)["debit"] = int64(93)
	group["changes"] = append(group["changes"].([]any), map[string]any{"user_id": users[0], "reason": "service_fee", "debit": int64(5), "credit": int64(0)})
	group["changes"].([]any)[1].(map[string]any)["credit"] = int64(48)
	sample.Operations = append(sample.Operations, prefix+"-group")
	sample.Batches = append(sample.Batches, derivedBatchID("group", prefix+"-group"))
	first := liveCall(t, address, settleGroupCommand, group)
	encoded, _ := json.Marshal(first)
	again := liveCall(t, address, settleGroupCommand, group)
	retry, _ := json.Marshal(again)
	if string(encoded) != string(retry) {
		t.Fatal("retry changed historical receipt")
	}
	group["changes"].([]any)[1].(map[string]any)["credit"] = int64(49)
	if _, err := settlementRPC(t, address, settleGroupCommand, group); !errors.Is(err, errSettlementConflict) {
		t.Fatalf("group identity conflict: %v", err)
	}
	group["changes"].([]any)[1].(map[string]any)["credit"] = int64(48)
	group["changes"].([]any)[3].(map[string]any)["reason"] = "other_fee"
	if _, err := settlementRPC(t, address, settleGroupCommand, group); !errors.Is(err, errSettlementConflict) {
		t.Fatalf("changed reason: %v", err)
	}

	// 单人接口的同一操作可以作为新的批量请求重提，不能重复加减。
	repeated := testSingle(prefix+"-single", users[0], 100, 198)
	repeated["business"] = business
	repeated["items"] = single["items"]
	sample.Batches = append(sample.Batches, prefix+"-repeat")
	result = liveCall(t, address, settleBatchCommand, testBatch(prefix+"-repeat", repeated))
	if result["results"].([]any)[0].(map[string]any)["origin_batch"] != derivedBatchID("single", prefix+"-single") {
		t.Fatal("cross-interface duplicate lost origin")
	}
	summary, err := auditSettlementOperations(context.Background(), store, sample.Operations, business)
	if err != nil {
		t.Fatal(err)
	}
	if summary["operation_count"] != int64(4) || summary["debit"] != int64(1198) || summary["credit"] != int64(346) {
		t.Fatalf("summary duplicated money: %v", summary)
	}
	for i, want := range []int64{0, 1048, 1100} {
		balance := liveCall(t, address, getBalanceCommand, users[i])
		if balance["amount"] != want || balance["version"] != []uint64{4, 2, 3}[i] {
			t.Fatalf("wallet %d: %v want amount %d", i, balance, want)
		}
	}
	// 在写余额之后制造归档序列化错误，核对真实事务已回滚。
	broken := settlementBatch{ID: prefix + "-rollback", Kind: "single", Operations: []silverOperation{{ID: prefix + "-rollback", Business: business, Reference: "ref", Changes: []silverChange{{UserID: users[1], Debit: 10}}, Detail: json.RawMessage("invalid")}}}
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	if _, err := store.settle(ctx, broken); err == nil {
		t.Fatal("invalid archive succeeded")
	}
	cancel()
	if liveCall(t, address, getBalanceCommand, users[1])["amount"] != int64(1048) {
		t.Fatal("archive failure did not roll back wallet")
	}
	stop()
	address, _ = startSettlementTestNode(t, store.db)
	operation := liveCall(t, address, getOperationCommand, users[1], prefix+"-group")
	if operation["status"] != "committed" {
		t.Fatal("restart lost financial result")
	}
	// 原始明细、终局结果与余额仍可关联，检查业务记录数量（含两项业务拒绝）。
	ctx, cancel = context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE business = ?", business).Scan(&count); err != nil || count != 6 {
		t.Fatalf("detail/index count: %d %v", count, err)
	}
	var requestJSON []byte
	if err := store.db.QueryRowContext(ctx, "SELECT request_json FROM settlement_batches WHERE batch_id = ?", batchID).Scan(&requestJSON); err != nil {
		t.Fatal(err)
	}
	var archived settlementBatch
	if err := json.Unmarshal(requestJSON, &archived); err != nil || len(archived.Operations) != 3 || archived.Operations[0].Changes[0].Debit != 1000 {
		t.Fatal("original detail archive missing")
	}
	if err := store.db.QueryRowContext(ctx, "SELECT request_json FROM settlement_batches WHERE batch_id = ?", derivedBatchID("group", prefix+"-group")).Scan(&requestJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(requestJSON, &archived); err != nil {
		t.Fatal(err)
	}
	changes := archived.Operations[0].Changes
	feeFound := false
	for _, change := range changes {
		if change.UserID == users[0] && change.Reason == "service_fee" && change.Debit == 5 {
			feeFound = true
		}
	}
	if len(changes) != 4 || !feeFound {
		t.Fatal("reason components not preserved")
	}
}

func stringSQLArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func userSQLArgs(values []uint64) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

// auditSettlementOperations 只用于离线验收，按本次明确的操作 ID 分块读取，不提供线上汇总接口。
func auditSettlementOperations(ctx context.Context, store settlementStore, ids []string, business string) (map[string]int64, error) {
	totals := map[string]int64{"operation_count": 0, "debit": 0, "credit": 0}
	for start := 0; start < len(ids); start += 500 {
		part := ids[start:min(start+500, len(ids))]
		args := append(stringSQLArgs(part), business)
		var count, debit, credit int64
		err := store.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(debit), 0), COALESCE(SUM(credit), 0) FROM settlement_operations FORCE INDEX (PRIMARY) WHERE operation_id IN ("+sqlSlots(len(part))+") AND business = ? AND status = 'committed'", args...).Scan(&count, &debit, &credit)
		if err != nil {
			return nil, err
		}
		totals["operation_count"] += count
		totals["debit"] += debit
		totals["credit"] += credit
	}
	return totals, nil
}
