package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type settlementStore struct{ db *sql.DB }

type storedOperation struct {
	Hash     []byte
	BatchID  string
	Position int
	Receipt  settlementReceipt
}

func sqlSlots(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (store settlementStore) settle(ctx context.Context, batch settlementBatch) (settlementReceipt, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return settlementReceipt{}, err
	}
	defer tx.Rollback()
	receipt, found, err := readBatchReceipt(ctx, tx, batch)
	if err != nil || found {
		return receipt, err
	}
	receipt, err = applySettlementBatch(ctx, tx, batch)
	if err != nil {
		return settlementReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return settlementReceipt{}, err
	}
	return receipt, nil
}

func readBatchReceipt(ctx context.Context, tx *sql.Tx, batch settlementBatch) (settlementReceipt, bool, error) {
	hash := batch.digest()
	var previousHash, previousJSON []byte
	err := tx.QueryRowContext(ctx, "SELECT request_hash, receipt_json FROM settlement_batches WHERE batch_id = ?", batch.ID).Scan(&previousHash, &previousJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return settlementReceipt{}, false, nil
	}
	if err != nil {
		return settlementReceipt{}, false, err
	}
	if !bytes.Equal(previousHash, hash[:]) {
		return settlementReceipt{}, false, errSettlementConflict
	}
	receipt, err := decodeStoredReceipt(previousJSON)
	if err == nil && receipt.BatchID != batch.ID {
		err = errors.New("stored batch identity mismatch")
	}
	if err == nil && len(batch.Operations) == 1 && batch.Operations[0].Policy != "" && !validStoredGroup(batch.Operations[0], receipt) {
		err = errors.New("invalid stored group receipt")
	}
	return receipt, true, err
}

// validStoredGroup 核对原回执的参与者及分配规则，避免重试时确认损坏的实际扣返。
func validStoredGroup(operation silverOperation, receipt settlementReceipt) bool {
	if len(receipt.Results) != 1 || receipt.Results[0].OperationID != operation.ID || receipt.Results[0].Status != "committed" || receipt.Results[0].OriginBatch != receipt.BatchID {
		return false
	}
	result := receipt.Results[0]
	expected := make(map[uint64]silverChange, len(operation.Changes))
	for _, c := range operation.Changes {
		expected[c.UserID] = c
	}
	if len(result.ActualChanges) != len(expected) || len(receipt.Balances) != len(expected) {
		return false
	}
	// 使用每人的实扣重建最小付款余额，重新计算分配；不依赖当前已经变化的钱包。
	wallets := make(map[uint64]walletBalance, len(expected))
	for _, c := range result.ActualChanges {
		want, ok := expected[c.UserID]
		if !ok || wallets[c.UserID].UserID != 0 || c.Debit < 0 || c.Credit < 0 || c.Debit > want.Debit || c.Credit > want.Credit {
			return false
		}
		wallets[c.UserID] = walletBalance{UserID: c.UserID, Amount: c.Debit, Version: 1}
	}
	seen := map[uint64]bool{}
	for _, balance := range receipt.Balances {
		if _, ok := expected[balance.UserID]; !ok || seen[balance.UserID] {
			return false
		}
		seen[balance.UserID] = true
	}
	switch operation.Policy {
	case "equal":
		actual, reason := settleEqualGroup(wallets, operation.Changes)
		return reason == "" && len(result.ActualTransfers) == 0 && reflect.DeepEqual(actual, result.ActualChanges)
	case "transfer":
		actual, transfers, reason := settleTransferGroup(wallets, operation.Transfers)
		return reason == "" && reflect.DeepEqual(actual, result.ActualChanges) && reflect.DeepEqual(transfers, result.ActualTransfers)
	default:
		return false
	}
}

// applySettlementBatch 只操作调用方事务，钱包创建仅属于 initialize。
func applySettlementBatch(ctx context.Context, tx *sql.Tx, batch settlementBatch) (settlementReceipt, error) {
	hash := batch.digest()
	previous, err := loadOperations(ctx, tx, batch)
	if err != nil {
		return settlementReceipt{}, err
	}
	ids := batchUsers(batch)
	wallets, err := loadWallets(ctx, tx, ids)
	if err != nil {
		return settlementReceipt{}, err
	}
	receipt := settlementReceipt{BatchID: batch.ID}
	dirty := make(map[uint64]bool)
	fresh := make([]int, 0, len(batch.Operations))
	for position, operation := range batch.Operations {
		if old, exists := previous[operation.ID]; exists {
			digest := operation.digest()
			if !bytes.Equal(old.Hash, digest[:]) {
				return receipt, errSettlementConflict
			}
			receipt.Results = append(receipt.Results, old.Receipt.Results[old.Position])
			continue
		}
		result := settlementResult{OperationID: operation.ID, Status: "committed", OriginBatch: batch.ID}
		switch operation.Policy {
		case "equal":
			result.ActualChanges, result.Reason = settleEqualGroup(wallets, operation.Changes)
		case "transfer":
			result.ActualChanges, result.ActualTransfers, result.Reason = settleTransferGroup(wallets, operation.Transfers)
		case "":
			result.Reason = applySilverOperation(wallets, operation)
		default:
			return receipt, errInvalidSettlement
		}
		// 新规则中的异常不是牌局拒绝；回滚并返回确定性错误，保留原请求供修复后处理。
		if operation.Policy != "" && result.Reason != "" {
			switch result.Reason {
			case "balance_overflow":
				return receipt, errBalanceOverflow
			case "version_overflow":
				return receipt, errVersionOverflow
			case "wallet_not_found":
				return receipt, errWalletNotFound
			default:
				return receipt, errInvalidSettlement
			}
		}
		if result.Reason != "" {
			result.Status = "rejected"
			result.ActualChanges, result.ActualTransfers = nil, nil
		} else if operation.Policy == "" {
			for _, change := range operation.Changes {
				dirty[change.UserID] = true
			}
		} else {
			for _, change := range result.ActualChanges {
				if change.Debit != 0 || change.Credit != 0 {
					dirty[change.UserID] = true
				}
			}
		}
		fresh = append(fresh, position)
		receipt.Results = append(receipt.Results, result)
	}
	for _, id := range ids {
		receipt.Balances = append(receipt.Balances, wallets[id])
	}
	if err := writeWallets(ctx, tx, ids, wallets, dirty); err != nil {
		return receipt, err
	}
	requestJSON, err := json.Marshal(batch)
	if err != nil {
		return receipt, err
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO settlement_batches (batch_id, request_hash, request_json, receipt_json) VALUES (?, ?, ?, ?)", batch.ID, hash[:], requestJSON, receiptJSON); err != nil {
		return receipt, err
	}
	if err := writeOperations(ctx, tx, batch, receipt, fresh); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func batchUsers(batch settlementBatch) []uint64 {
	users := make(map[uint64]bool)
	for _, operation := range batch.Operations {
		for _, change := range operation.Changes {
			users[change.UserID] = true
		}
	}
	ids := make([]uint64, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func loadOperations(ctx context.Context, tx *sql.Tx, batch settlementBatch) (map[string]storedOperation, error) {
	args := make([]any, len(batch.Operations))
	for i, operation := range batch.Operations {
		args[i] = operation.ID
	}
	rows, err := tx.QueryContext(ctx, "SELECT o.operation_id, o.request_hash, o.origin_batch, o.operation_position, b.receipt_json FROM settlement_operations o JOIN settlement_batches b ON b.batch_id = o.origin_batch WHERE o.operation_id IN ("+sqlSlots(len(args))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make(map[string]storedOperation)
	for rows.Next() {
		var id string
		var old storedOperation
		var encoded []byte
		if err := rows.Scan(&id, &old.Hash, &old.BatchID, &old.Position, &encoded); err != nil {
			return nil, err
		}
		old.Receipt, err = decodeStoredReceipt(encoded)
		if err != nil || old.Receipt.BatchID != old.BatchID || old.Position < 0 || old.Position >= len(old.Receipt.Results) {
			return nil, errors.New("invalid stored operation receipt")
		}
		if old.Receipt.Results[old.Position].OperationID != id {
			return nil, errors.New("stored operation identity mismatch")
		}
		operations[id] = old
	}
	return operations, rows.Err()
}

func loadWallets(ctx context.Context, tx *sql.Tx, ids []uint64) (map[uint64]walletBalance, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, "SELECT user_id, amount, version FROM silver_balances WHERE user_id IN ("+sqlSlots(len(ids))+") ORDER BY user_id FOR UPDATE NOWAIT", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	wallets := make(map[uint64]walletBalance, len(ids))
	for rows.Next() {
		var balance walletBalance
		if err := rows.Scan(&balance.UserID, &balance.Amount, &balance.Version); err != nil {
			return nil, err
		}
		if balance.UserID == 0 || balance.Amount < 0 {
			return nil, errors.New("invalid stored wallet")
		}
		wallets[balance.UserID] = balance
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(wallets) != len(ids) {
		return nil, errWalletNotFound
	}
	return wallets, nil
}

func writeWallets(ctx context.Context, tx *sql.Tx, ids []uint64, wallets map[uint64]walletBalance, dirty map[uint64]bool) error {
	if len(dirty) == 0 {
		return nil
	}
	amount, version := strings.Builder{}, strings.Builder{}
	amount.WriteString("UPDATE silver_balances SET amount = CASE user_id")
	version.WriteString(" END, version = CASE user_id")
	args, versionArgs, idArgs := []any{}, []any{}, []any{}
	for _, id := range ids {
		if !dirty[id] {
			continue
		}
		amount.WriteString(" WHEN ? THEN ?")
		version.WriteString(" WHEN ? THEN ?")
		args = append(args, id, wallets[id].Amount)
		versionArgs = append(versionArgs, id, wallets[id].Version)
		idArgs = append(idArgs, id)
	}
	query := amount.String() + version.String() + " END WHERE user_id IN (" + sqlSlots(len(idArgs)) + ")"
	args = append(args, versionArgs...)
	args = append(args, idArgs...)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(dirty)) {
		return errors.New("wallet update count mismatch")
	}
	return nil
}

func writeOperations(ctx context.Context, tx *sql.Tx, batch settlementBatch, receipt settlementReceipt, fresh []int) error {
	if len(fresh) == 0 {
		return nil
	}
	values := make([]string, 0, len(fresh))
	args := make([]any, 0, len(fresh)*8)
	for _, position := range fresh {
		operation := batch.Operations[position]
		result := receipt.Results[position]
		hash := operation.digest()
		var debit, credit int64
		if operation.Policy == "" {
			for _, change := range operation.Changes {
				debit += change.Debit
				credit += change.Credit
			}
		} else {
			for _, change := range result.ActualChanges {
				debit += change.Debit
				credit += change.Credit
			}
		}
		values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, operation.ID, hash[:], batch.ID, position, operation.Business, result.Status, debit, credit)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO settlement_operations (operation_id, request_hash, origin_batch, operation_position, business, status, debit, credit) VALUES "+strings.Join(values, ","), args...)
	return err
}

func decodeStoredReceipt(encoded []byte) (settlementReceipt, error) {
	var receipt settlementReceipt
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return receipt, err
	}
	if !validSettlementID(receipt.BatchID) || len(receipt.Results) == 0 || len(receipt.Balances) == 0 {
		return receipt, errors.New("invalid stored settlement receipt")
	}
	for _, balance := range receipt.Balances {
		if balance.UserID == 0 || balance.Amount < 0 {
			return receipt, errors.New("invalid stored receipt balance")
		}
	}
	for _, result := range receipt.Results {
		if !validSettlementID(result.OperationID) || !validSettlementID(result.OriginBatch) || result.Status != "committed" && result.Status != "rejected" {
			return receipt, errors.New("invalid stored settlement result")
		}
	}
	return receipt, nil
}

func (store settlementStore) getBatch(ctx context.Context, id string) (any, error) {
	var encoded []byte
	err := store.db.QueryRowContext(ctx, "SELECT receipt_json FROM settlement_batches WHERE batch_id = ?", id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	receipt, err := decodeStoredReceipt(encoded)
	if err != nil {
		return nil, err
	}
	if receipt.BatchID != id {
		return nil, errors.New("stored batch identity mismatch")
	}
	return receipt.wire(), nil
}

func (store settlementStore) getOperation(ctx context.Context, userID uint64, id string) (any, error) {
	var requestJSON, receiptJSON []byte
	var position int
	err := store.db.QueryRowContext(ctx, "SELECT b.request_json, b.receipt_json, o.operation_position FROM settlement_operations o JOIN settlement_batches b ON b.batch_id = o.origin_batch WHERE o.operation_id = ?", id).Scan(&requestJSON, &receiptJSON, &position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var batch settlementBatch
	if err := json.Unmarshal(requestJSON, &batch); err != nil {
		return nil, err
	}
	receipt, err := decodeStoredReceipt(receiptJSON)
	if err != nil || position < 0 || position >= len(batch.Operations) || position >= len(receipt.Results) {
		return nil, errors.New("invalid stored operation position")
	}
	operation := batch.Operations[position]
	if operation.ID != id || receipt.Results[position].OperationID != id {
		return nil, errors.New("stored operation identity mismatch")
	}
	for _, change := range operation.Changes {
		if change.UserID == userID {
			result := receipt.Results[position]
			return map[string]any{"operation_id": result.OperationID, "status": result.Status, "reason": result.Reason, "origin_batch": result.OriginBatch}, nil
		}
	}
	return nil, nil
}

func (store settlementStore) getBalance(ctx context.Context, id uint64) (map[string]any, error) {
	balance := walletBalance{UserID: id}
	err := store.db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", id).Scan(&balance.Amount, &balance.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errWalletNotFound
	}
	if err != nil {
		return nil, err
	}
	if balance.Amount < 0 {
		return nil, errors.New("invalid stored wallet balance")
	}
	return balance.wire(), nil
}

func (store settlementStore) initialize(ctx context.Context, id uint64, amount int64) (settlementReceipt, error) {
	if id == 0 || amount < 0 {
		return settlementReceipt{}, errInvalidSettlement
	}
	operation := silverOperation{ID: fmt.Sprintf("wallet-init-%d", id), Business: "wallet_init", Reference: fmt.Sprint(id), Changes: []silverChange{{UserID: id, Reason: "initial_balance", Credit: amount}}, Detail: json.RawMessage(`{"reason":"demo_initial_balance"}`)}
	batch := settlementBatch{ID: derivedBatchID("init", operation.ID), Kind: "initialize", Operations: []silverOperation{operation}}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return settlementReceipt{}, err
	}
	defer tx.Rollback()
	receipt, found, err := readBatchReceipt(ctx, tx, batch)
	if err != nil || found {
		return receipt, err
	}
	var existing uint64
	err = tx.QueryRowContext(ctx, "SELECT user_id FROM silver_balances WHERE user_id = ?", id).Scan(&existing)
	if err == nil {
		return settlementReceipt{}, errSettlementConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return settlementReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO silver_balances (user_id, amount, version) VALUES (?, 0, 0)", id); err != nil {
		return settlementReceipt{}, err
	}
	receipt, err = applySettlementBatch(ctx, tx, batch)
	if err != nil {
		return settlementReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return settlementReceipt{}, err
	}
	return receipt, nil
}
