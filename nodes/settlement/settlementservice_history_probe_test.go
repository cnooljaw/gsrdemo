package main

import (
	"context"
	random "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

type historyQueryReport struct {
	Name   string  `json:"name"`
	Count  int     `json:"count"`
	Errors int     `json:"errors"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	MaxMS  float64 `json:"max_ms"`
	Error  string  `json:"error,omitempty"`
}

type historyStageReport struct {
	TableSizesFresh bool                   `json:"table_sizes_fresh"`
	ExactRows       map[string]uint64      `json:"exact_rows,omitempty"`
	CountSeconds    float64                `json:"exact_count_seconds,omitempty"`
	Games           uint64                 `json:"game_operations"`
	Wallets         uint64                 `json:"wallets"`
	Operations      uint64                 `json:"total_operations"`
	Batches         uint64                 `json:"total_batches"`
	Tables          []historyTableSize     `json:"tables"`
	Loads           []settlementLoadReport `json:"loads"`
	Queries         []historyQueryReport   `json:"queries"`
}

type historyTableSize struct {
	DataFileBytes uint64 `json:"data_file_logical_bytes,omitempty"`
	Name          string `json:"name"`
	EstimatedRows uint64 `json:"estimated_rows"`
	DataBytes     uint64 `json:"data_bytes"`
	IndexBytes    uint64 `json:"index_bytes"`
}

// newHistoryLoadSample 借用历史钱包；本次新增账务删除后，将余额/版本恢复原值，保留大库。
func newHistoryLoadSample(t *testing.T, store settlementStore, config settlementHistoryConfig, spread bool) (*liveSettlementSample, map[uint64]walletBalance) {
	t.Helper()
	sample := &liveSettlementSample{store: store}
	count := 20600
	if spread {
		count = min(int(config.Wallets), 20600*((config.LoadSeconds+24)/25))
	}
	for i := 0; i < count; i++ {
		id := uint64(i + 1)
		if spread {
			id = (uint64(i)*7919)%config.Wallets + 1
		}
		sample.Users = append(sample.Users, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := readHistorySubset(ctx, tx, sample.Users)
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	sample.Business = "load-history-" + config.Dataset + "-" + random.Text()
	backup, err := json.Marshal(map[string]any{"dataset": config.Dataset, "business": sample.Business, "balances": baseline})
	if err != nil {
		t.Fatal(err)
	}
	backupPath := "../../.cache/history-wallet-backup-" + random.Text() + ".json"
	if err := os.WriteFile(backupPath, backup, 0600); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		for _, table := range []struct {
			name, key string
			ids       []any
		}{{"settlement_operations", "operation_id", stringSQLArgs(sample.Operations)}, {"settlement_batches", "batch_id", stringSQLArgs(sample.Batches)}} {
			for start := 0; start < len(table.ids); start += 500 {
				end := min(start+500, len(table.ids))
				if _, err := tx.ExecContext(ctx, "DELETE FROM "+table.name+" WHERE "+table.key+" IN ("+sqlSlots(end-start)+")", table.ids[start:end]...); err != nil {
					t.Error(err)
					return
				}
			}
		}
		// 未触及的钱包不写，兼容短程验证和 MySQL changed-rows 语义。
		current, err := readHistorySubset(ctx, tx, sample.Users)
		if err != nil {
			t.Error(err)
			return
		}
		var ids []uint64
		for id, balance := range current {
			if balance != baseline[id] {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for start := 0; start < len(ids); start += 500 {
			part := ids[start:min(start+500, len(ids))]
			if err := writeWallets(ctx, tx, part, baseline, partDirty(part)); err != nil {
				t.Error(err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(backupPath); err != nil {
			t.Error(err)
		}
	})
	return sample, baseline
}

func partDirty(ids []uint64) map[uint64]bool {
	dirty := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		dirty[id] = true
	}
	return dirty
}

func historyQuery(t *testing.T, jobs chan<- settlementLoadJob, results <-chan settlementLoadResult, report *historyQueryReport, job settlementLoadJob) settlementLoadResult {
	t.Helper()
	job.scheduled = time.Now()
	jobs <- job
	select {
	case result := <-results:
		report.Count++
		if result.err != nil {
			report.Errors++
			report.Error = result.err.Error()
		}
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("history probe caller did not return")
		return settlementLoadResult{}
	}
}

func runHistoryQueries(t *testing.T, store settlementStore, config settlementConfig, history settlementHistoryConfig, progress historyProgress) []historyQueryReport {
	t.Helper()
	address, _ := startSettlementLoadProcess(t, config)
	jobs, results := make(chan settlementLoadJob, 16), make(chan settlementLoadResult, 32)
	stop := startSettlementLoadCaller(t, address, jobs, results)
	defer stop()
	reports := []historyQueryReport{}
	positions := []uint64{0}
	cycles := progress.Operations / 3200
	for i := uint64(1); i < 32; i++ {
		positions = append(positions, ((i*7919)%cycles)*3200)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	primary, err := readHistoryProgress(ctx, store.db, history)
	cancel()
	if err != nil {
		t.Fatal(err)
	}

	for _, pass := range []string{"history_first_pass", "history_repeat_pass"} {
		report := historyQueryReport{Name: pass}
		var samples []time.Duration
		for _, position := range positions {
			source := history
			if position >= primary.Operations {
				source.Dataset += "-small"
				source.MoleBatchSize = 1
			}
			batches := historyCycle(source, position)
			for _, batch := range []settlementBatch{batches[0], batches[len(batches)-1]} {
				result := historyQuery(t, jobs, results, &report, settlementLoadJob{command: getBatchCommand, args: []any{batch.ID}})
				samples = append(samples, result.elapsed)
				if result.err == nil && result.receipt["batch_id"] != batch.ID {
					t.Fatal("history batch query returned wrong identity")
				}
				operation := batch.Operations[0]
				result = historyQuery(t, jobs, results, &report, settlementLoadJob{command: getOperationCommand, args: []any{operation.Changes[0].UserID, operation.ID}})
				samples = append(samples, result.elapsed)
				if result.err == nil && result.receipt["operation_id"] != operation.ID {
					t.Fatal("history operation query returned wrong identity")
				}
			}
		}
		report.P50MS = loadPercentile(samples, 50)
		report.P95MS = loadPercentile(samples, 95)
		report.MaxMS = loadPercentile(samples, 100)
		reports = append(reports, report)
	}
	// 同批重试、跨批重提与分类冲突分别测，不把超时当成未执行。
	original := historyCycle(history, 0)[0]
	for _, kind := range []string{"same_batch_retry", "cross_batch_300_operations", "reason_conflict"} {
		report := historyQueryReport{Name: kind}
		batch := original
		command := settleBatchCommand
		if kind != "same_batch_retry" {
			batch.ID = "history-probe-" + random.Text()
		}
		if kind == "reason_conflict" {
			// Changes 是共享切片，先重新构建独立值再改变分类。
			batch = historyCycle(history, 0)[0]
			batch.ID = "history-probe-" + random.Text()
			batch.Operations[0].Changes[0].Reason = "other_cost"
		}
		request := historyWire(batch)
		result := historyQuery(t, jobs, results, &report, settlementLoadJob{command: command, request: request})
		report.P50MS = float64(result.elapsed) / float64(time.Millisecond)
		report.P95MS = report.P50MS
		report.MaxMS = report.P50MS
		if kind == "reason_conflict" {
			if !errors.Is(result.err, errSettlementConflict) {
				t.Errorf("history conflict: %v", result.err)
			} else {
				report.Errors = 0
				report.Error = "expected identity_conflict"
			}
		} else if result.err == nil && result.receipt["batch_id"] != batch.ID {
			t.Error("retry batch identity mismatch")
		}
		if kind != "same_batch_retry" {
			// 只删除本次重提归档；操作索引和钱包都应保持原账。
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := store.db.ExecContext(ctx, "DELETE FROM settlement_batches WHERE batch_id = ?", batch.ID); err != nil {
				t.Error(err)
			}
			cancel()
		}
		reports = append(reports, report)
	}
	return reports
}

func historySizes(t *testing.T, db *sql.DB) []historyTableSize {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 仅采集连接绕过 24 小时元数据缓存，不修改数据库全局参数。
	if _, err := conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0"); err != nil {
		t.Fatal(err)
	}
	var directory string
	if err := conn.QueryRowContext(ctx, "SELECT @@datadir").Scan(&directory); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT table_name, table_rows, data_length, index_length FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sizes []historyTableSize
	for rows.Next() {
		var size historyTableSize
		if err := rows.Scan(&size.Name, &size.EstimatedRows, &size.DataBytes, &size.IndexBytes); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(filepath.Join(directory, "gsrdemo_settlement_load", size.Name+".ibd")); err == nil && info.Size() > 0 {
			size.DataFileBytes = uint64(info.Size())
		}
		sizes = append(sizes, size)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return sizes
}

func runHistoryStage(t *testing.T, store settlementStore, config settlementConfig, history settlementHistoryConfig, progress historyProgress) {
	t.Helper()
	verifyHistoryWalletTotals(t, store, history, progress)
	report := historyStageReport{TableSizesFresh: true, Games: progress.Operations, Wallets: history.Wallets, Operations: progress.Operations + progress.Initialized, Batches: progress.Batches + progress.Initialized, Tables: historySizes(t, store.db)}
	save := func() {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("../../.cache/settlement-history-%d.json", progress.Operations)
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	modes := []string{"hot_wallets", "spread_wallets"}
	if progress.Operations >= 100000000 {
		modes = append(modes, "spread_wallets_and_keys")
	}
	for _, name := range modes {
		spread := name != "hot_wallets"
		t.Run(name, func(t *testing.T) {
			sample, baseline := newHistoryLoadSample(t, store, history, spread)
			profile := settlementLoadProfile{Tables: 5000, Seats: 4, CardsPerTick: 2, External: true, Rotation: spread}
			if name == "spread_wallets_and_keys" {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				primary, err := readHistoryProgress(ctx, store.db, history)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				profile.HistoryDataset = history.Dataset
				profile.HistoryGames = primary.Operations
				profile.HistoryWallets = history.Wallets
			}
			load := executeSettlementLoad(t, "C", time.Duration(history.LoadSeconds)*time.Second, profile, store, config, sample, baseline)
			load.Case = name
			report.Loads = append(report.Loads, load)
		})
		save()
	}
	report.Queries = runHistoryQueries(t, store, config, history, progress)
	begin := time.Now()
	report.ExactRows = historyExactCounts(t, store, progress)
	report.CountSeconds = time.Since(begin).Seconds()
	save()
	for _, query := range report.Queries {
		t.Logf("history query=%s count=%d errors=%d p95_ms=%.1f error=%s", query.Name, query.Count, query.Errors, query.P95MS, query.Error)
		if query.Errors > 0 {
			t.Errorf("history path failed its existing contract: %s", query.Name)
		}
	}
}

func readHistorySubset(ctx context.Context, tx *sql.Tx, ids []uint64) (map[uint64]walletBalance, error) {
	balances := make(map[uint64]walletBalance, len(ids))
	for start := 0; start < len(ids); start += 5000 {
		part := ids[start:min(start+5000, len(ids))]
		rows, err := tx.QueryContext(ctx, "SELECT user_id, amount, version FROM silver_balances WHERE user_id IN ("+sqlSlots(len(part))+")", userSQLArgs(part)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var balance walletBalance
			if err := rows.Scan(&balance.UserID, &balance.Amount, &balance.Version); err != nil {
				rows.Close()
				return nil, err
			}
			balances[balance.UserID] = balance
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(balances) != len(ids) {
		return nil, fmt.Errorf("history balance subset: got %d want %d", len(balances), len(ids))
	}
	return balances, nil
}

func readLoadDatabaseCounters(t *testing.T, store settlementStore) map[string]uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := store.db.QueryContext(ctx, "SHOW GLOBAL STATUS WHERE Variable_name IN ('Innodb_buffer_pool_reads', 'Innodb_buffer_pool_read_requests', 'Innodb_data_read', 'Innodb_data_written')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counters := make(map[string]uint64)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		counters[key] = number
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return counters
}

func verifyHistoryWalletTotals(t *testing.T, store settlementStore, config settlementHistoryConfig, progress historyProgress) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var count, amount, versions uint64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(amount), 0), COALESCE(SUM(version), 0) FROM silver_balances").Scan(&count, &amount, &versions); err != nil {
		t.Fatal(err)
	}
	movements := progress.Operations / 3200 * 3800
	if count != config.Wallets || amount != config.Wallets*1_000_000_000-movements || versions != config.Wallets+movements {
		t.Fatalf("seed wallet totals mismatch: count=%d amount=%d versions=%d", count, amount, versions)
	}
	// 逐钱包抽样按序号独立推导，避免只校验总净额。
	for i := uint64(0); i < 64; i++ {
		user := (i*7919)%config.Wallets + 1
		expected := walletBalance{UserID: user, Amount: 1_000_000_000, Version: 1}
		for number := user - 1; number < progress.Operations; number += config.Wallets {
			if number%3200 >= 3000 {
				continue
			}
			if number%2 == 0 {
				expected.Amount -= 100
			} else {
				expected.Amount += 98
			}
			expected.Version++
		}
		for number := (user - 1) / 4; number < progress.Operations; number += config.Wallets / 4 {
			if number%3200 < 3000 {
				continue
			}
			if (user-1)%4 == 0 {
				expected.Amount -= 151
			} else {
				expected.Amount += 49
			}
			expected.Version++
		}
		var amount int64
		var version uint64
		if err := store.db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", user).Scan(&amount, &version); err != nil {
			t.Fatal(err)
		}
		if amount != expected.Amount || version != expected.Version {
			t.Fatalf("historical wallet %d: got %d/%d want %d/%d", user, amount, version, expected.Amount, expected.Version)
		}
	}
}

func historyExactCounts(t *testing.T, store settlementStore, progress historyProgress) map[string]uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	expected := map[string]uint64{"silver_balances": progress.Initialized, "settlement_operations": progress.Initialized + progress.Operations, "settlement_batches": progress.Initialized + progress.Batches}
	counts := make(map[string]uint64)
	for _, table := range []string{"silver_balances", "settlement_operations", "settlement_batches"} {
		var count uint64
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
		if count != expected[table] {
			t.Fatalf("exact count %s=%d want %d", table, count, expected[table])
		}
	}
	return counts
}
