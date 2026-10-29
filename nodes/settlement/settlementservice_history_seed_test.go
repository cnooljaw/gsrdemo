package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/mysqlclient"
	"github.com/cnooljaw/gsrdemo/common/model"
	"github.com/go-sql-driver/mysql"
)

type settlementHistoryConfig struct {
	MySQLConfig   string   `yaml:"mysql_config"`
	Dataset       string   `yaml:"dataset_id"`
	Wallets       uint64   `yaml:"wallets"`
	MoleBatchSize int      `yaml:"mole_batch_size"`
	Stages        []uint64 `yaml:"stages"`
	BatchStages   []uint64 `yaml:"batch_stages"`
	LoadSeconds   int      `yaml:"load_seconds"`
}

type historyProgress struct {
	Initialized uint64
	Operations  uint64
	Batches     uint64
}

type historyRecord struct {
	Batch   settlementBatch
	Receipt settlementReceipt
}

func historyStore(t *testing.T) (settlementStore, settlementConfig, settlementHistoryConfig) {
	t.Helper()
	// 同机工具互斥，避免造数与基线恢复并发；与业务钱包锁无关。
	if err := os.MkdirAll("../../.cache", 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile("../../.cache/settlement-history.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		t.Fatalf("another local history tool is active: %v", err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	path := "../../configs/settlement-history.yaml"
	var history settlementHistoryConfig
	if err := model.ReadYAMLFile(path, &history); err != nil {
		t.Fatal(err)
	}
	if history.Wallets < 20600 || history.Wallets%4 != 0 || history.Wallets > 10_000_000 || !validSettlementID(history.Dataset) || len(history.Dataset) > 40 || history.MoleBatchSize < 1 || history.MoleBatchSize > 300 || 3000%history.MoleBatchSize != 0 || history.LoadSeconds < 1 || history.LoadSeconds > 300 {
		t.Fatal("invalid bounded history config")
	}
	for i, stage := range history.Stages {
		if stage == 0 || stage%3200 != 0 || i > 0 && stage <= history.Stages[i-1] {
			t.Fatal("history stages must be increasing multiples of 3200 game operations")
		}
	}
	for i, target := range history.BatchStages {
		if target == 0 || target > 50000000 || i > 0 && target <= history.BatchStages[i-1] {
			t.Fatal("invalid bounded batch stages")
		}
	}

	config, err := loadSettlementConfig(filepath.Join(filepath.Dir(path), history.MySQLConfig))
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := mysql.ParseDSN(config.MySQLDSN)
	if err != nil || dsn.DBName != "gsrdemo_settlement_load" || dsn.Net != "tcp" || dsn.Addr != "127.0.0.1:3306" {
		t.Fatal("history tests require dedicated local gsrdemo_settlement_load")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 仅离线造数连接合并 prepare/execute/close 往返；业务子进程仍使用原 DSN。
	dsn.InterpolateParams = true
	// 离线 COUNT/扫描诊断由各自 Context 限时；不沿用业务连接默认的 2 秒读超时。
	dsn.ReadTimeout = 10 * time.Minute
	dsn.WriteTimeout = 30 * time.Second
	db, err := mysqlclient.Open(ctx, dsn.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return settlementStore{db: db}, config, history
}

func readHistoryProgress(ctx context.Context, db *sql.DB, config settlementHistoryConfig) (historyProgress, error) {
	var progress historyProgress
	var wallets uint64
	var batchSize int
	err := db.QueryRowContext(ctx, "SELECT wallet_count, initialized_users, game_operations, game_batches, mole_batch_size FROM load_seed_progress WHERE dataset_id = ?", config.Dataset).Scan(&wallets, &progress.Initialized, &progress.Operations, &progress.Batches, &batchSize)
	if err == nil {
		if wallets != config.Wallets || batchSize != config.MoleBatchSize {
			return progress, errors.New("seed config differs from stored dataset; do not rewrite history")
		}
		return progress, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return progress, err
	}
	for _, table := range []string{"silver_balances", "settlement_operations", "settlement_batches"} {
		var one int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&one)
		if !errors.Is(err, sql.ErrNoRows) {
			return progress, fmt.Errorf("unowned existing data in %s", table)
		}
	}
	_, err = db.ExecContext(ctx, "INSERT INTO load_seed_progress (dataset_id, wallet_count, initialized_users, game_operations, game_batches, mole_batch_size) VALUES (?, ?, 0, 0, 0, ?)", config.Dataset, config.Wallets, config.MoleBatchSize)
	return progress, err
}

func writeHistoryProgress(ctx context.Context, tx *sql.Tx, config settlementHistoryConfig, progress historyProgress) error {
	_, err := tx.ExecContext(ctx, "UPDATE load_seed_progress SET initialized_users = ?, game_operations = ?, game_batches = ? WHERE dataset_id = ?", progress.Initialized, progress.Operations, progress.Batches, config.Dataset)
	return err
}

// writeHistoryRecords 仅离线造数使用，保留真实摘要、完整请求、回执与操作索引；不关唯一键或持久化。
func writeHistoryRecords(ctx context.Context, tx *sql.Tx, records []historyRecord) error {
	batchSlots, operationSlots := []string{}, []string{}
	batchArgs, operationArgs := []any{}, []any{}
	for _, record := range records {
		batch := record.Batch
		hash := batch.digest()
		request, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		receipt, err := json.Marshal(record.Receipt)
		if err != nil {
			return err
		}
		batchSlots = append(batchSlots, "(?, ?, ?, ?)")
		batchArgs = append(batchArgs, batch.ID, hash[:], string(request), string(receipt))
		for i, operation := range batch.Operations {
			digest := operation.digest()
			var debit, credit int64
			for _, change := range operation.Changes {
				debit += change.Debit
				credit += change.Credit
			}
			operationSlots = append(operationSlots, "(?, ?, ?, ?, ?, ?, ?, ?)")
			operationArgs = append(operationArgs, operation.ID, digest[:], batch.ID, i, operation.Business, record.Receipt.Results[i].Status, debit, credit)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO settlement_batches (batch_id, request_hash, request_json, receipt_json) VALUES "+strings.Join(batchSlots, ","), batchArgs...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO settlement_operations (operation_id, request_hash, origin_batch, operation_position, business, status, debit, credit) VALUES "+strings.Join(operationSlots, ","), operationArgs...)
	return err
}

func historyInitialization(id uint64) historyRecord {
	operation := silverOperation{ID: fmt.Sprintf("wallet-init-%d", id), Business: "wallet_init", Reference: fmt.Sprint(id), Changes: []silverChange{{UserID: id, Reason: "initial_balance", Credit: 1_000_000_000}}, Detail: json.RawMessage(`{"reason":"demo_initial_balance"}`)}
	batch := settlementBatch{ID: derivedBatchID("init", operation.ID), Kind: "initialize", Operations: []silverOperation{operation}}
	receipt := settlementReceipt{BatchID: batch.ID, Results: []settlementResult{{OperationID: operation.ID, Status: "committed", OriginBatch: batch.ID}}, Balances: []walletBalance{{UserID: id, Amount: 1_000_000_000, Version: 1}}}
	return historyRecord{Batch: batch, Receipt: receipt}
}

func seedHistoryUsers(ctx context.Context, db *sql.DB, config settlementHistoryConfig, progress historyProgress) (historyProgress, error) {
	end := min(progress.Initialized+1000, config.Wallets)
	records := make([]historyRecord, 0, end-progress.Initialized)
	slots, args := []string{}, []any{}
	for id := progress.Initialized + 1; id <= end; id++ {
		records = append(records, historyInitialization(id))
		slots = append(slots, "(?, ?, 1)")
		args = append(args, id, int64(1_000_000_000))
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return progress, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO silver_balances (user_id, amount, version) VALUES "+strings.Join(slots, ","), args...); err != nil {
		return progress, err
	}
	if err := writeHistoryRecords(ctx, tx, records); err != nil {
		return progress, err
	}
	progress.Initialized = end
	if err := writeHistoryProgress(ctx, tx, config, progress); err != nil {
		return progress, err
	}
	return progress, tx.Commit()
}

func historyCycle(config settlementHistoryConfig, position uint64) []settlementBatch {
	batches := make([]settlementBatch, 0, 3000/config.MoleBatchSize+200)
	for offset := 0; offset < 3000; offset += config.MoleBatchSize {
		start := position + uint64(offset)
		batch := settlementBatch{ID: fmt.Sprintf("%s-b-%04d-%012d", config.Dataset, start%1024, start), Kind: "batch"}
		for i := 0; i < config.MoleBatchSize; i++ {
			number := start + uint64(i)
			user := number%config.Wallets + 1
			credit := int64(0)
			if number%2 == 1 {
				credit = 198
			}
			operation := silverOperation{ID: fmt.Sprintf("%s-h-%04d-%012d", config.Dataset, user%1024, number), Business: "mole", Reference: fmt.Sprintf("attack-%d/%d/0", user%600, number), Changes: []silverChange{{UserID: user, Reason: "hammer_cost", Debit: 100}, {UserID: user, Reason: "hammer_reward", Credit: credit}}, Detail: json.RawMessage(fmt.Sprintf(`{"action_id":"action-%d","rule":"demo-99pct-v1"}`, number))}
			batch.Operations = append(batch.Operations, operation)
		}
		batches = append(batches, batch)
	}
	for i := 0; i < 200; i++ {
		number := position + 3000 + uint64(i)
		operation := silverOperation{ID: fmt.Sprintf("%s-c-%04d-%012d", config.Dataset, number%1024, number), Business: "cards", Reference: fmt.Sprintf("table-%d/round-%d", number%5000, number), Detail: json.RawMessage(`{}`)}
		for seat := uint64(0); seat < 4; seat++ {
			user := (number*4+seat)%config.Wallets + 1
			debit, credit := int64(0), int64(50)
			if seat == 0 {
				debit, credit = 150, 0
			}
			// 与公开解码器同序：game_win_loss 在 service_fee 前。
			operation.Changes = append(operation.Changes, silverChange{UserID: user, Reason: "game_win_loss", Debit: debit, Credit: credit}, silverChange{UserID: user, Reason: "service_fee", Debit: 1})
		}
		sortHistoryChanges(operation.Changes)
		batches = append(batches, settlementBatch{ID: derivedBatchID("group", operation.ID), Kind: "group", Operations: []silverOperation{operation}})
	}
	return batches
}

func sortHistoryChanges(changes []silverChange) {
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].UserID != changes[j].UserID {
			return changes[i].UserID < changes[j].UserID
		}
		return changes[i].Reason < changes[j].Reason
	})
}

func seedHistoryCycle(ctx context.Context, db *sql.DB, config settlementHistoryConfig, progress historyProgress, wallets []walletBalance) (historyProgress, error) {
	batches := historyCycle(config, progress.Operations)
	work := make(map[uint64]walletBalance)
	dirty := make(map[uint64]bool)
	records := make([]historyRecord, 0, len(batches))
	for _, batch := range batches {
		receipt := settlementReceipt{BatchID: batch.ID}
		for _, operation := range batch.Operations {
			for _, change := range operation.Changes {
				if !dirty[change.UserID] {
					work[change.UserID] = wallets[change.UserID]
					dirty[change.UserID] = true
				}
			}
			if reason := applySilverOperation(work, operation); reason != "" {
				return progress, fmt.Errorf("seed operation %s: %s", operation.ID, reason)
			}
			receipt.Results = append(receipt.Results, settlementResult{OperationID: operation.ID, Status: "committed", OriginBatch: batch.ID})
		}
		for _, id := range batchUsers(batch) {
			receipt.Balances = append(receipt.Balances, work[id])
		}
		records = append(records, historyRecord{Batch: batch, Receipt: receipt})
	}
	ids := make([]uint64, 0, len(dirty))
	for id := range dirty {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return progress, err
	}
	defer tx.Rollback()
	if err := writeWallets(ctx, tx, ids, work, dirty); err != nil {
		return progress, err
	}
	if err := writeHistoryRecords(ctx, tx, records); err != nil {
		return progress, err
	}
	progress.Operations += 3200
	progress.Batches += uint64(len(batches))
	if err := writeHistoryProgress(ctx, tx, config, progress); err != nil {
		return progress, err
	}
	if err := tx.Commit(); err != nil {
		return progress, err
	}
	for id, balance := range work {
		wallets[id] = balance
	}
	return progress, nil
}

func loadHistoryWallets(ctx context.Context, db *sql.DB, count uint64) ([]walletBalance, error) {
	rows, err := db.QueryContext(ctx, "SELECT user_id, amount, version FROM silver_balances ORDER BY user_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	wallets := make([]walletBalance, count+1)
	var found uint64
	for rows.Next() {
		var balance walletBalance
		if err := rows.Scan(&balance.UserID, &balance.Amount, &balance.Version); err != nil {
			return nil, err
		}
		if balance.UserID == 0 || balance.UserID > count || balance.Amount < 0 {
			return nil, errors.New("unexpected seed wallet")
		}
		wallets[balance.UserID] = balance
		found++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found != count {
		return nil, errors.New("history wallet count mismatch")
	}
	return wallets, nil
}

func seedHistoryTo(t *testing.T, store settlementStore, config settlementHistoryConfig, target uint64) historyProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	progress, err := readHistoryProgress(ctx, store.db, config)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if progress.Operations > target {
		return progress
	}
	var orphan string
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	checkErr := store.db.QueryRowContext(checkCtx, "SELECT operation_id FROM settlement_operations WHERE business >= 'load-' AND business < 'load.' LIMIT 1").Scan(&orphan)
	checkCancel()
	if checkErr == nil {
		t.Fatal("unfinished load artifacts exist; preserve dataset and restore test baseline before seeding")
	}
	if !errors.Is(checkErr, sql.ErrNoRows) {
		t.Fatal(checkErr)
	}
	started, last := time.Now(), time.Now()
	for progress.Initialized < config.Wallets {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		progress, err = seedHistoryUsers(ctx, store.db, config, progress)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(last) > 10*time.Second {
			t.Logf("seed initialized wallets=%d/%d", progress.Initialized, config.Wallets)
			last = time.Now()
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	wallets, err := loadHistoryWallets(ctx, store.db, config.Wallets)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	for progress.Operations < target {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		progress, err = seedHistoryCycle(ctx, store.db, config, progress, wallets)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(last) > 10*time.Second {
			t.Logf("seed game_operations=%d/%d game_batches=%d elapsed=%s", progress.Operations, target, progress.Batches, time.Since(started).Round(time.Second))
			last = time.Now()
		}
	}
	return progress
}

func combinedHistoryProgress(ctx context.Context, store settlementStore, config settlementHistoryConfig, base historyProgress) (historyProgress, error) {
	var cursor, batches uint64
	err := store.db.QueryRowContext(ctx, "SELECT game_operations, game_batches FROM load_seed_progress WHERE dataset_id = ?", config.Dataset+"-small").Scan(&cursor, &batches)
	if errors.Is(err, sql.ErrNoRows) {
		return base, nil
	}
	if err != nil {
		return base, err
	}
	if cursor < base.Operations {
		return base, errors.New("small-batch cursor predates natural history")
	}
	return historyProgress{Initialized: base.Initialized, Operations: cursor, Batches: base.Batches + batches}, nil
}

func seedHistoryBatchTarget(t *testing.T, store settlementStore, config settlementHistoryConfig, target uint64) historyProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	base, err := readHistoryProgress(ctx, store.db, config)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Stages) == 0 || base.Operations != config.Stages[len(config.Stages)-1] {
		t.Fatal("finish natural history before high-batch stages")
	}
	small := config
	small.Dataset += "-small"
	small.MoleBatchSize = 1
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	var existing int
	err = store.db.QueryRowContext(ctx, "SELECT 1 FROM load_seed_progress WHERE dataset_id = ?", small.Dataset).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = store.db.ExecContext(ctx, "INSERT INTO load_seed_progress (dataset_id, wallet_count, initialized_users, game_operations, game_batches, mole_batch_size) VALUES (?, ?, ?, ?, 0, 1)", small.Dataset, config.Wallets, config.Wallets, base.Operations)
	}
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	progress, err := readHistoryProgress(ctx, store.db, small)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	current := base.Initialized + base.Batches + progress.Batches
	if current < target {
		extra := (target - current + 3199) / 3200 * 3200
		progress = seedHistoryTo(t, store, small, progress.Operations+extra)
	}
	return historyProgress{Initialized: base.Initialized, Operations: progress.Operations, Batches: base.Batches + progress.Batches}
}
