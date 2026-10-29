package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"os"
	"strings"
	"testing"
	"time"
)

func historyWire(batch settlementBatch) map[string]any {
	operations := make([]any, len(batch.Operations))
	for i, operation := range batch.Operations {
		detail := make(map[string]any)
		_ = json.Unmarshal(operation.Detail, &detail)
		fields := map[string]any{"operation_id": operation.ID, "business": operation.Business, "business_ref": operation.Reference, "detail": detail}
		if batch.Kind == "group" {
			changes := make([]any, len(operation.Changes))
			for j, change := range operation.Changes {
				changes[j] = map[string]any{"user_id": change.UserID, "reason": change.Reason, "debit": change.Debit, "credit": change.Credit}
			}
			fields["changes"] = changes
			fields["version"] = uint64(2)
			return fields
		}
		items := make([]any, len(operation.Changes))
		for j, change := range operation.Changes {
			items[j] = map[string]any{"reason": change.Reason, "debit": change.Debit, "credit": change.Credit}
		}
		fields["user_id"] = operation.Changes[0].UserID
		fields["items"] = items
		operations[i] = fields
	}
	return map[string]any{"version": uint64(2), "batch_id": batch.ID, "operations": operations}
}

// TestHistorySeedKeepsCanonicalIdentity 保护离线造数可由真实公开解码器重建同一摘要，包含跨钱包边界的四人操作。
func TestHistorySeedKeepsCanonicalIdentity(t *testing.T) {
	for _, size := range []int{300, 25, 1} {
		config := settlementHistoryConfig{Dataset: "history-v1", Wallets: 20600, MoleBatchSize: size}
		for _, position := range []uint64{0, 16000, 99200, 100000000} {
			count := 0
			batches := historyCycle(config, position)
			if len(batches) != 3000/size+200 {
				t.Fatal("wrong batch ratio")
			}
			for _, batch := range batches {
				var decoded settlementBatch
				var err error
				if batch.Kind == "group" {
					decoded, err = decodeSettlementGroup(historyWire(batch))
				} else {
					decoded, err = decodeSettlementBatch(historyWire(batch))
				}
				if err != nil || batch.digest() != decoded.digest() {
					t.Fatalf("noncanonical seed batch %s: %v", batch.ID, err)
				}
				count += len(batch.Operations)
			}
			if count != 3200 {
				t.Fatal("wrong operations per cycle")
			}
		}
	}
}

// TestSettlementHistory 保护大库各档的造数可恢复，并输出实际存储和账务查询证据；仅显式开启。
func TestSettlementHistory(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_HISTORY") != "1" {
		t.Skip("explicit dedicated history database required")
	}
	if err := os.MkdirAll("../../.cache", 0700); err != nil {
		t.Fatal(err)
	}
	store, nodeConfig, config := historyStore(t)
	for _, stage := range config.Stages {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			base, err := readHistoryProgress(ctx, store.db, config)
			if err == nil {
				base, err = combinedHistoryProgress(ctx, store, config, base)
			}
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if base.Operations > stage {
				t.Skipf("dataset already at %d; cannot report it as %d", base.Operations, stage)
			}
			progress := seedHistoryTo(t, store, config, stage)
			if progress.Operations > stage {
				t.Skipf("stored dataset already at %d; cannot report it as %d", progress.Operations, stage)
			}
			runHistoryStage(t, store, nodeConfig, config, progress)
		})
	}
	for _, batches := range config.BatchStages {
		t.Run(fmt.Sprintf("batches-%d", batches), func(t *testing.T) {
			progress := seedHistoryBatchTarget(t, store, config, batches)
			if progress.Initialized+progress.Batches >= batches+3200 {
				t.Skipf("batch dataset already beyond %d", batches)
			}
			runHistoryStage(t, store, nodeConfig, config, progress)
		})
	}

}

// TestHistorySeedFailureKeepsWalletBaseline 保护归档失败时余额与检查点一起回滚，内存状态不提前推进。
func TestHistorySeedFailureKeepsWalletBaseline(t *testing.T) {
	store, mock := mockStore(t)
	config := settlementHistoryConfig{Dataset: "history-v1", Wallets: 20600, MoleBatchSize: 300}
	wallets := make([]walletBalance, config.Wallets+1)
	for i := uint64(1); i <= config.Wallets; i++ {
		wallets[i] = walletBalance{UserID: i, Amount: 1_000_000_000, Version: 1}
	}
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE silver_balances").WillReturnResult(sqlmock.NewResult(0, 3800))
	failure := errors.New("injected archive failure")
	mock.ExpectExec("INSERT INTO settlement_batches").WillReturnError(failure)
	mock.ExpectRollback()
	progress := historyProgress{Initialized: config.Wallets}
	next, err := seedHistoryCycle(context.Background(), store.db, config, progress, wallets)
	if !errors.Is(err, failure) || next != progress || wallets[1].Amount != 1_000_000_000 || wallets[1].Version != 1 {
		t.Fatalf("seed advanced after failure: %+v %v wallet=%+v", next, err, wallets[1])
	}
}

// TestHistoryBaseline 保护持久历史总余额、版本与抽样钱包同造数规则一致，不写入数据。
func TestHistoryBaseline(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_HISTORY") != "1" {
		t.Skip("dedicated history database required")
	}
	store, _, config := historyStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	progress, err := readHistoryProgress(ctx, store.db, config)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	progress, err = combinedHistoryProgress(ctx, store, config, progress)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	verifyHistoryWalletTotals(t, store, config, progress)
	countBegin := time.Now()
	counts := historyExactCounts(t, store, progress)
	sizes := historySizes(t, store.db)
	path := fmt.Sprintf("../../.cache/settlement-history-%d.json", progress.Operations)
	if encoded, err := os.ReadFile(path); err == nil {
		var report historyStageReport
		if err := json.Unmarshal(encoded, &report); err != nil {
			t.Fatal(err)
		}
		if report.Games != progress.Operations {
			t.Fatal("audit report cursor mismatch")
		}
		report.ExactRows = counts
		report.CountSeconds = time.Since(countBegin).Seconds()
		report.Tables = sizes
		report.TableSizesFresh = true
		diagnostic := historySummaryDiagnostic(t, store, progress)
		replaced := false
		for i := range report.Queries {
			if report.Queries[i].Name == diagnostic.Name {
				report.Queries[i] = diagnostic
				replaced = true
			}
		}
		if !replaced {
			report.Queries = append(report.Queries, diagnostic)
		}
		if err := os.WriteFile(path+".preaudit", encoded, 0600); err != nil {
			t.Fatal(err)
		}
		updated, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, updated, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("persistent baseline verified: %+v exact_rows=%v", progress, counts)
}

// TestHistoryScatteredInsertIdentities 保护分散写入的新身份不覆盖历史身份，仍符合单用户结算协议且一批内唯一。
func TestHistoryScatteredInsertIdentities(t *testing.T) {
	users := make([]uint64, 600)
	for i := range users {
		users[i] = uint64(i + 1)
	}
	profile := settlementLoadProfile{Tables: 5000, Seats: 4, CardsPerTick: 2, HistoryDataset: "history-v1", HistoryGames: 100000000, HistoryWallets: 1000000}
	job := loadJob(t, "mole", "load-test", 10, time.Now(), users, true, false, profile)
	if len(job.batch.Operations) != 300 {
		t.Fatal("wrong batch size")
	}
	seen := make(map[string]bool)
	for _, operation := range job.batch.Operations {
		if !strings.Contains(operation.ID, "~") || !validSettlementID(operation.ID) || seen[operation.ID] {
			t.Fatalf("invalid new identity %q", operation.ID)
		}
		seen[operation.ID] = true
	}
	if first, second := historyInsertKey(profile, 42), historyInsertKey(profile, 42); first != second {
		t.Fatal("key mapping not reproducible")
	}
}
