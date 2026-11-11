package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
)

type settlementMixedLimits struct{ writes, reads, connections int }
type settlementMixedReport struct {
	Scenario           string  `json:"scenario"`
	Round              int     `json:"round"`
	WriteWorkers       int     `json:"write_workers"`
	ReadWorkers        int     `json:"read_workers"`
	Connections        int     `json:"mysql_connections"`
	WriteRPC           int     `json:"write_rpc"`
	ReadRPC            int     `json:"read_rpc"`
	Operations         int     `json:"operations"`
	Seconds            float64 `json:"seconds"`
	WorkloadsPerSecond float64 `json:"workloads_per_second"`
	WriteOpsPerSecond  float64 `json:"write_operations_per_second"`
	ReadsPerSecond     float64 `json:"reads_per_second"`
	WriteP99           float64 `json:"write_p99_ms"`
	ReadP99            float64 `json:"read_p99_ms"`
	Retries            int     `json:"retries"`
	Errors             int     `json:"errors"`
	DBWait             int64   `json:"mysql_wait_count"`
	DBWaitMS           float64 `json:"mysql_wait_ms"`
	Verified           bool    `json:"verified"`
}

type settlementMixedScenario struct {
	name         string
	writePercent int
	audit        bool
}

func runSettlementMixedCase(t *testing.T, store settlementStore, sample *liveSettlementSample, scenario settlementMixedScenario, limits settlementMixedLimits, round int, seed string) settlementMixedReport {
	t.Helper()
	const sources, jobs, batchSize = 64, 1024, 64
	store.db.SetMaxOpenConns(limits.connections)
	store.db.SetMaxIdleConns(limits.connections)
	firstOperation, firstBatch := len(sample.Operations), len(sample.Batches)
	report := settlementMixedReport{Scenario: scenario.name, Round: round, WriteWorkers: limits.writes, ReadWorkers: limits.reads, Connections: limits.connections}
	// 每个写来源独占八个钱包；读来源跨来源查询，允许读到写入前或写入后的完整版本。
	snapshots := map[uint64]map[uint64]int64{}
	wallets := map[uint64]walletBalance{}
	for _, uid := range sample.Users[:sources*8] {
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		value, err := store.getBalance(ctx, uid)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		b := walletBalance{UserID: uid, Amount: value["amount"].(int64), Version: value["version"].(uint64)}
		wallets[uid] = b
		snapshots[uid] = map[uint64]int64{b.Version: b.Amount}
	}
	apply := func(uid uint64, delta int64) {
		b := wallets[uid]
		b.Amount += delta
		b.Version++
		wallets[uid] = b
		snapshots[uid][b.Version] = b.Amount
	}
	results := make(chan settlementLoadResult, jobs)
	callers := make([]*settlementLoadCaller, sources)
	for i := range sources {
		callers[i] = &settlementLoadCaller{results: results}
	}
	prefix := "mixed-" + rand.Text()
	for index := range jobs {
		stream := index % sources
		base := stream * 8
		id := fmt.Sprintf("%s-%d", prefix, index)
		job := settlementLoadJob{id: id}
		// 打散读写顺序，保证短周期内也维持同一请求比例。
		if (index*37)%100 < scenario.writePercent {
			report.WriteRPC++
			if report.WriteRPC%10 == 0 {
				job.command = settlementwrite.SettleEqualGroupCommand
				changes := []any{}
				for i, delta := range []int64{-1, -1, 2} {
					debit, credit := int64(0), int64(0)
					if delta < 0 {
						debit = -delta
					} else {
						credit = delta
					}
					uid := sample.Users[base+i]
					changes = append(changes, map[string]any{"user_id": uid, "reason": "game_win_loss", "debit": debit, "credit": credit})
					apply(uid, delta)
				}
				job.args = []any{map[string]any{"version": uint64(2), "operation_id": id, "business": "landlord", "business_ref": id, "changes": changes, "detail": map[string]any{}}}
				sample.Operations = append(sample.Operations, id)
				sample.Batches = append(sample.Batches, derivedBatchID("group-equal", id))
				report.Operations++
			} else {
				ops := make([]map[string]any, batchSize)
				for i := range batchSize {
					uid := sample.Users[base+i%8]
					op := fmt.Sprintf("%s-op-%d", id, i)
					ops[i] = testSingle(op, uid, 1, 0)
					ops[i]["business"] = "mole"
					apply(uid, -1)
					sample.Operations = append(sample.Operations, op)
				}
				job.request = testBatch(id, ops...)
				sample.Batches = append(sample.Batches, id)
				report.Operations += batchSize
			}
		} else {
			report.ReadRPC++
			job.command = settlementread.GetBalanceCommand
			// 与当前来源错开，读取正在被其他来源更新的钱包。
			job.args = []any{sample.Users[((stream+17)%sources)*8+index%8]}
			if scenario.audit {
				switch report.ReadRPC % 10 {
				case 0:
					job.command = settlementread.GetBatchCommand
					job.args = []any{seed}
				case 1:
					job.command = settlementread.GetOperationCommand
					job.args = []any{sample.Users[sources*8], seed + "-op"}
				}
			}
		}
		callers[stream].jobs = append(callers[stream].jobs, job)
	}
	stats := store.db.Stats()
	start, stop := settlementLoadNodes(t, store.db, limits.writes, limits.reads, 64, callers)
	var readLatency, writeLatency []time.Duration
	for range jobs {
		select {
		case result := <-results:
			report.Retries += result.retries
			if result.err != nil {
				report.Errors++
				t.Errorf("混合调用失败: %v", result.err)
				continue
			}
			cmd := result.job.command
			if cmd == 0 || cmd == settlementwrite.SettleEqualGroupCommand {
				writeLatency = append(writeLatency, result.elapsed)
				expectedBatch := result.job.id
				count := batchSize
				if cmd == settlementwrite.SettleEqualGroupCommand {
					expectedBatch = derivedBatchID("group-equal", result.job.id)
					count = 1
				}
				rows, ok := result.receipt["results"].([]any)
				if !ok || len(rows) != count || result.receipt["batch_id"] != expectedBatch {
					t.Fatal("写回执批次或数量错误")
				}
				for i, item := range rows {
					row := item.(map[string]any)
					op := result.job.id
					if cmd == 0 {
						op = fmt.Sprintf("%s-op-%d", op, i)
					}
					if row["operation_id"] != op || row["status"] != "committed" || row["origin_batch"] != expectedBatch {
						t.Fatalf("写回执身份或状态错误: %v", row)
					}
				}
			} else {
				readLatency = append(readLatency, result.elapsed)
				switch cmd {
				case settlementread.GetBalanceCommand:
					uid := result.job.args[0].(uint64)
					amount, found := snapshots[uid][result.receipt["version"].(uint64)]
					if !found || result.receipt["user_id"] != uid || result.receipt["amount"] != amount {
						t.Fatalf("查询读到不完整的钱包版本: %v", result.receipt)
					}
				case settlementread.GetBatchCommand:
					if result.receipt["batch_id"] != seed {
						t.Fatal("核查批次身份错误")
					}
					rows := result.receipt["results"].([]any)
					if len(rows) != 1 || rows[0].(map[string]any)["status"] != "committed" {
						t.Fatal("核查批次终态错误")
					}
				case settlementread.GetOperationCommand:
					if result.receipt["operation_id"] != seed+"-op" || result.receipt["status"] != "committed" || result.receipt["origin_batch"] != seed {
						t.Fatal("核查操作终态错误")
					}
				}
			}
		case <-time.After(30 * time.Second):
			t.Fatal("混合调用未完成")
		}
	}
	report.Seconds = time.Since(start).Seconds()
	after := store.db.Stats()
	report.DBWait, report.DBWaitMS = after.WaitCount-stats.WaitCount, float64(after.WaitDuration-stats.WaitDuration)/float64(time.Millisecond)
	stop()
	report.WorkloadsPerSecond = float64(jobs) / report.Seconds
	report.WriteOpsPerSecond = float64(report.Operations) / report.Seconds
	report.ReadsPerSecond = float64(report.ReadRPC) / report.Seconds
	report.WriteP99 = settlementLoadPercentile(writeLatency, 99)
	report.ReadP99 = settlementLoadPercentile(readLatency, 99)
	if len(writeLatency) != report.WriteRPC || len(readLatency) != report.ReadRPC {
		t.Fatal("混合负载有调用未完成")
	}
	for uid, want := range wallets {
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		value, err := store.getBalance(ctx, uid)
		cancel()
		if err != nil || value["amount"] != want.Amount || value["version"] != want.Version {
			t.Fatalf("混合负载钱包未对齐: %d %v %v", uid, value, err)
		}
	}
	if settlementCountRows(t, store.db, "settlement_operations", "operation_id", sample.Operations[firstOperation:]) != report.Operations || settlementCountRows(t, store.db, "settlement_batches", "batch_id", sample.Batches[firstBatch:]) != report.WriteRPC {
		t.Fatal("混合负载唯一账务数量错误")
	}
	sample.deleteRows(t, "settlement_operations", "operation_id", stringSQLArgs(sample.Operations[firstOperation:]))
	sample.deleteRows(t, "settlement_batches", "batch_id", stringSQLArgs(sample.Batches[firstBatch:]))
	if t.Failed() {
		t.Fatal("混合样本清理失败")
	}
	sample.Operations = sample.Operations[:firstOperation]
	sample.Batches = sample.Batches[:firstBatch]
	report.Verified = true
	t.Logf("%s round=%d 写/读=%d/%d db=%d: %.0f workload/s, %.0f op/s, %.0f read/s, P99写/读=%.1f/%.1fms, retries=%d wait=%d 对账通过", scenario.name, round, limits.writes, limits.reads, limits.connections, report.WorkloadsPerSecond, report.WriteOpsPerSecond, report.ReadsPerSecond, report.WriteP99, report.ReadP99, report.Retries, report.DBWait)
	return report
}

// TestSettlementMixedLoadLiveMySQL 比较相同业务负载下的读写池配比与连接数，逐笔验证读快照、终态和唯一账务。
func TestSettlementMixedLoadLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_MIXED_LOAD") != "1" {
		t.Skip("set GSRDEMO_SETTLEMENT_MIXED_LOAD=1 and GSRDEMO_SETTLEMENT_MYSQL=1")
	}
	store, _ := liveSettlementStore(t)
	sample := newLiveSettlementSampleAmount(t, store, 64*8+1, 1_000_000_000)
	seed := "mixed-seed-" + rand.Text()
	batch, err := decodeSettlementBatch(testBatch(seed, testSingle(seed+"-op", sample.Users[64*8], 0, 1)))
	if err != nil {
		t.Fatal(err)
	}
	sample.Operations = append(sample.Operations, seed+"-op")
	sample.Batches = append(sample.Batches, seed)
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	_, err = store.settle(ctx, batch)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	scenarios := []settlementMixedScenario{{"steady", 90, false}, {"entry_peak", 70, false}, {"audit_peak", 50, true}}
	options := []settlementMixedLimits{{4, 1, 10}, {8, 1, 10}, {8, 2, 10}, {8, 4, 10}, {12, 2, 10}, {12, 4, 10}, {16, 4, 10}}
	reports := []settlementMixedReport{}
	for _, scenario := range scenarios {
		var best settlementMixedLimits
		bestRate := float64(0)
		// 每个配比各三轮，按吞吐中位数选择连接数对比的候选。
		for _, limits := range options {
			rates := []float64{}
			for round := 1; round <= 3; round++ {
				r := runSettlementMixedCase(t, store, sample, scenario, limits, round, seed)
				reports = append(reports, r)
				rates = append(rates, r.WorkloadsPerSecond)
			}
			sort.Float64s(rates)
			if rates[1] > bestRate {
				best, bestRate = limits, rates[1]
			}
		}
		// 保持成员数不变，单独比较更多数据库连接，不混淆两种收益。
		best.connections = 16
		for round := 1; round <= 3; round++ {
			reports = append(reports, runSettlementMixedCase(t, store, sample, scenario, best, round, seed))
		}
	}
	encoded, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("../../.cache", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("../../.cache/settlement-mixed-load.json", append(encoded, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
