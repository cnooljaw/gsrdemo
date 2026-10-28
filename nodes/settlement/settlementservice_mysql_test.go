package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func mockStore(t *testing.T) (settlementStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return settlementStore{db: db}, mock
}

func emptyOperations() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"operation_id", "request_hash", "origin_batch", "operation_position", "receipt_json"})
}

func expectNewBatch(mock sqlmock.Sqlmock, batch settlementBatch, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT request_hash, receipt_json").WithArgs(batch.ID).WillReturnRows(sqlmock.NewRows([]string{"request_hash", "receipt_json"}))
	args := make([]driver.Value, len(batch.Operations))
	for i, operation := range batch.Operations {
		args[i] = operation.ID
	}
	mock.ExpectQuery("SELECT o.operation_id").WithArgs(args...).WillReturnRows(rows)
}

// TestSettlementBatchPersistsIndependentResults 保护批次内不足操作不影响另一笔，余额、结果及明细在同一事务提交。
func TestSettlementBatchPersistsIndependentResults(t *testing.T) {
	store, mock := mockStore(t)
	batch, err := decodeSettlementBatch(testBatch("batch", testSingle("bad", 1, 100, 0), testSingle("good", 2, 100, 198)))
	if err != nil {
		t.Fatal(err)
	}
	expectNewBatch(mock, batch, emptyOperations())
	mock.ExpectQuery("SELECT user_id, amount, version").WithArgs(uint64(1), uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"user_id", "amount", "version"}).AddRow(1, 50, 1).AddRow(2, 1000, 1))
	mock.ExpectExec("UPDATE silver_balances SET amount = CASE").WithArgs(uint64(2), int64(1098), uint64(2), uint64(2), uint64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO settlement_batches").WithArgs("batch", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO settlement_operations").WithArgs("bad", sqlmock.AnyArg(), "batch", 0, "demo", "rejected", int64(100), int64(0), "good", sqlmock.AnyArg(), "batch", 1, "demo", "committed", int64(100), int64(198)).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	receipt, err := store.settle(context.Background(), batch, 0)
	if err != nil || receipt.Results[0].Status != "rejected" || receipt.Results[1].Status != "committed" || receipt.Balances[0].Amount != 50 || receipt.Balances[1].Amount != 1098 {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
}

// TestSettlementFailureDoesNotReportBusinessCompletion 保护写入失败回滚、提交结果未知时不假报成功或余额不足。
func TestSettlementFailureDoesNotReportBusinessCompletion(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "commit"}[commitFailure], func(t *testing.T) {
			store, mock := mockStore(t)
			batch, _ := decodeSettlementBatch(testBatch("batch", testSingle("op", 1, 100, 0)))
			expectNewBatch(mock, batch, emptyOperations())
			mock.ExpectQuery("SELECT user_id, amount, version").WillReturnRows(sqlmock.NewRows([]string{"user_id", "amount", "version"}).AddRow(1, 1000, 1))
			failure := errors.New("injected database failure")
			if !commitFailure {
				mock.ExpectExec("UPDATE silver_balances").WillReturnError(failure)
				mock.ExpectRollback()
			} else {
				mock.ExpectExec("UPDATE silver_balances").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO settlement_batches").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec("INSERT INTO settlement_operations").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit().WillReturnError(failure)
			}
			if _, err := store.settle(context.Background(), batch, 0); !errors.Is(err, failure) {
				t.Fatalf("SQL failure became final success: %v", err)
			}
		})
	}
}

// TestSettlementBatchIdentitySurvivesLostReply 保护已提交批次重试只返回原结果，内容冲突不写余额。
func TestSettlementBatchIdentitySurvivesLostReply(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "conflict"}[conflict], func(t *testing.T) {
			store, mock := mockStore(t)
			batch, _ := decodeSettlementBatch(testBatch("batch", testSingle("op", 1, 100, 0)))
			hash := batch.digest()
			previous := settlementReceipt{BatchID: "batch", Results: []settlementResult{{OperationID: "op", Status: "committed", OriginBatch: "batch"}}, Balances: []walletBalance{{UserID: 1, Amount: 900, Version: 2}}}
			encoded, _ := json.Marshal(previous)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT request_hash, receipt_json").WithArgs("batch").WillReturnRows(sqlmock.NewRows([]string{"request_hash", "receipt_json"}).AddRow(hash[:], encoded))
			mock.ExpectRollback()
			if conflict {
				batch.Operations[0].Changes[0].Credit = 198
			}
			receipt, err := store.settle(context.Background(), batch, 0)
			if conflict {
				if !errors.Is(err, errSettlementConflict) {
					t.Fatal(err)
				}
			} else if err != nil || receipt.Balances[0].Amount != 900 {
				t.Fatalf("retry: %+v %v", receipt, err)
			}
		})
	}
}

// TestSettlementOperationDeduplicatesAcrossBatches 保护相同业务操作换批次不再次记账，内容不同则拒绝整个请求。
func TestSettlementOperationDeduplicatesAcrossBatches(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "conflict"}[conflict], func(t *testing.T) {
			store, mock := mockStore(t)
			batch, _ := decodeSettlementBatch(testBatch("new", testSingle("op", 1, 100, 0)))
			hash := batch.Operations[0].digest()
			previous := settlementReceipt{BatchID: "old", Results: []settlementResult{{OperationID: "op", Status: "committed", OriginBatch: "old"}}, Balances: []walletBalance{{UserID: 1, Amount: 900, Version: 2}}}
			encoded, _ := json.Marshal(previous)
			expectNewBatch(mock, batch, emptyOperations().AddRow("op", hash[:], "old", 0, encoded))
			mock.ExpectQuery("SELECT user_id, amount, version").WillReturnRows(sqlmock.NewRows([]string{"user_id", "amount", "version"}).AddRow(1, 900, 2))
			if conflict {
				batch.Operations[0].Changes[0].Credit = 198
				mock.ExpectRollback()
			} else {
				mock.ExpectExec("INSERT INTO settlement_batches").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			}
			receipt, err := store.settle(context.Background(), batch, 0)
			if conflict {
				if !errors.Is(err, errSettlementConflict) {
					t.Fatal(err)
				}
			} else if err != nil || receipt.Balances[0].Amount != 900 || receipt.Results[0].OriginBatch != "old" {
				t.Fatalf("cross-batch retry: %+v %v", receipt, err)
			}
		})
	}
}

// TestSettlementGroupWritesAllWalletsTogether 保护多人成功时所有参与余额与一项操作凭据同事务写入。
func TestSettlementGroupWritesAllWalletsTogether(t *testing.T) {
	store, mock := mockStore(t)
	request := map[string]any{"version": uint64(1), "operation_id": "round", "business": "cards", "business_ref": "round-1", "detail": map[string]any{}, "changes": []any{
		map[string]any{"user_id": uint64(1), "debit": int64(100), "credit": int64(0)}, map[string]any{"user_id": uint64(2), "debit": int64(0), "credit": int64(100)}}}
	batch, err := decodeSettlementGroup(request)
	if err != nil {
		t.Fatal(err)
	}
	expectNewBatch(mock, batch, emptyOperations())
	mock.ExpectQuery("SELECT user_id, amount, version").WithArgs(uint64(1), uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"user_id", "amount", "version"}).AddRow(1, 1000, 1).AddRow(2, 1000, 1))
	mock.ExpectExec("UPDATE silver_balances SET amount = CASE").WithArgs(uint64(1), int64(900), uint64(2), int64(1100), uint64(1), uint64(2), uint64(2), uint64(2), uint64(1), uint64(2)).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO settlement_batches").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO settlement_operations").WithArgs("round", sqlmock.AnyArg(), batch.ID, 0, "cards", "committed", int64(100), int64(100)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	receipt, err := store.settle(context.Background(), batch, 0)
	if err != nil || len(receipt.Results) != 1 || receipt.Results[0].Status != "committed" || receipt.Balances[0].Amount != 900 || receipt.Balances[1].Amount != 1100 {
		t.Fatalf("group receipt: %+v %v", receipt, err)
	}
}
