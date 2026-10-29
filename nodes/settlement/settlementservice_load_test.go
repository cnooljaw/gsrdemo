package main

import (
	"context"
	random "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	gsrruntime "github.com/cnooljaw/gsr/runtime"
)

// 压测器的 channel/goroutine 仅归测试宿主管理；不进入节点业务实现。
type settlementLoadJob struct {
	command   gsrruntime.CommandID
	request   map[string]any
	args      []any
	batch     settlementBatch
	scheduled time.Time
}

type settlementLoadResult struct {
	job     settlementLoadJob
	elapsed time.Duration
	rpc     time.Duration
	err     error
	receipt map[string]any
}

type settlementLoadWorker struct {
	jobs    <-chan settlementLoadJob
	results chan<- settlementLoadResult
}

var _ gsr.Service = (*settlementLoadWorker)(nil)

func (worker *settlementLoadWorker) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(time.Millisecond, 1)
	return err
}

func (worker *settlementLoadWorker) Handle(self gsr.CommandContext, _ gsrruntime.CommandID, _ ...any) (any, error) {
	select {
	case job := <-worker.jobs:
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		args := job.args
		if args == nil {
			args = []any{job.request}
		}
		value, err := self.Cluster().Call(ctx, "settlement", settlementServiceName, job.command, args...)
		cancel()
		receipt, _ := value.(map[string]any)
		worker.results <- settlementLoadResult{job: job, elapsed: time.Since(job.scheduled), rpc: time.Since(started), err: err, receipt: receipt}
	default:
	}
	_, err := self.After(time.Millisecond, 1)
	return nil, err
}

func (worker *settlementLoadWorker) OnStop(_ context.Context) error {
	return nil
}

type settlementLoadMain struct {
	jobs    <-chan settlementLoadJob
	results chan<- settlementLoadResult
	ready   chan<- struct{}
}

var _ gsr.Service = (*settlementLoadMain)(nil)

func (main *settlementLoadMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for range 16 {
		if _, err := self.CreateService(ctx, &settlementLoadWorker{jobs: main.jobs, results: main.results}); err != nil {
			return err
		}
	}
	main.ready <- struct{}{}
	self.Exit()
	return nil
}

func (main *settlementLoadMain) Handle(_ gsr.CommandContext, _ gsrruntime.CommandID, _ ...any) (any, error) {
	return nil, gsrruntime.ErrUnknownCommand
}

func (main *settlementLoadMain) OnStop(_ context.Context) error {
	return nil
}

func startSettlementLoadCaller(t *testing.T, address string, jobs <-chan settlementLoadJob, results chan<- settlementLoadResult) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}, 1), make(chan error, 1)
	config := gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": address}}}
	go func() { done <- gsr.Run(ctx, config, &settlementLoadMain{jobs: jobs, results: results, ready: ready}) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("load caller shutdown timed out")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case <-ready:
	case err := <-done:
		done <- err
		t.Fatalf("load caller: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("load caller startup timed out")
	}
	return stop
}

type settlementLoadReport struct {
	Case                string             `json:"case"`
	Tables              int                `json:"tables"`
	Seats               int                `json:"seats"`
	External            bool               `json:"external_settlement_process"`
	Processes           []loadProcessUsage `json:"processes,omitempty"`
	DatabaseCounters    map[string]uint64  `json:"mysql_global_counter_deltas,omitempty"`
	Seconds             float64            `json:"seconds"`
	Wallets             int                `json:"wallets"`
	PlannedRPC          int                `json:"planned_rpc"`
	DroppedRPC          int                `json:"dropped_rpc"`
	CompletedRPC        int                `json:"completed_rpc"`
	Errors              int                `json:"errors"`
	Rejected            int                `json:"rejected"`
	Operations          int                `json:"operations"`
	MoleOperations      int                `json:"mole_operations"`
	CardOperations      int                `json:"card_operations"`
	MoleP99MS           float64            `json:"mole_p99_ms"`
	CardP99MS           float64            `json:"card_p99_ms"`
	OperationsPerSecond float64            `json:"operations_per_second"`
	P50MS               float64            `json:"p50_ms"`
	P95MS               float64            `json:"p95_ms"`
	P99MS               float64            `json:"p99_ms"`
	MaxMS               float64            `json:"max_ms"`
	RPCP99MS            float64            `json:"rpc_p99_ms"`
	DrainMS             float64            `json:"drain_ms"`
	CPUSeconds          float64            `json:"test_process_cpu_seconds"`
	HeapMiB             float64            `json:"test_process_heap_mib"`
	GCCycles            uint32             `json:"test_process_gc_cycles"`
	Reconciled          bool               `json:"reconciled"`
	VerificationError   string             `json:"verification_error,omitempty"`
}

func loadCPUSeconds() float64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
}

func loadPercentile(samples []time.Duration, percent int) float64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return float64(samples[(len(samples)-1)*percent/100]) / float64(time.Millisecond)
}

type settlementLoadProfile struct {
	HistoryDataset string
	HistoryGames   uint64
	HistoryWallets uint64
	Tables         int
	Seats          int
	CardsPerTick   int
	External       bool
	Rotation       bool
}

func currentLoadProfile(t *testing.T) settlementLoadProfile {
	t.Helper()
	switch os.Getenv("GSRDEMO_SETTLEMENT_LOAD_PROFILE") {
	case "":
		return settlementLoadProfile{Tables: 2500, Seats: 3, CardsPerTick: 1}
	case "four":
		return settlementLoadProfile{Tables: 5000, Seats: 4, CardsPerTick: 2, External: true}
	default:
		t.Fatal("unknown load profile")
		return settlementLoadProfile{}
	}
}

// loadJob 保留真实业务金额结构；只省略尚未实现的棋牌局内行为。
func loadJob(t *testing.T, name, prefix string, tick int, scheduled time.Time, users []uint64, mole bool, hot bool, profile settlementLoadProfile) settlementLoadJob {
	t.Helper()
	id := fmt.Sprintf("%s-%s-%d", prefix, name, tick)
	if mole {
		operations := make([]map[string]any, 300)
		for i := range operations {
			slot := ((tick/10)*300 + i) % 600
			if profile.Rotation {
				slot += ((tick - 1) / 2500) * 20600
			}
			user := users[slot%len(users)]
			if hot && i%5 == 0 {
				user = users[0]
			}
			credit := int64(0)
			if rand.IntN(2) == 1 {
				credit = 198
			}
			op := testSingle(fmt.Sprintf("%s-%d", id, i), user, 100, credit)
			if profile.HistoryGames > 0 {
				// 新身份插入历史键附近，避免仅测一片全新前缀的热索引。
				key := historyInsertKey(profile, uint64(tick)*300+uint64(i))
				suffix := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", id, i)))
				op["operation_id"] = key + "~" + hex.EncodeToString(suffix[:8])
			}
			op["business"] = prefix
			op["items"] = []any{map[string]any{"reason": "hammer_cost", "debit": int64(100), "credit": int64(0)}, map[string]any{"reason": "hammer_reward", "debit": int64(0), "credit": credit}}
			operations[i] = op
		}
		request := testBatch(id, operations...)
		batch, err := decodeSettlementBatch(request)
		if err != nil {
			t.Fatal(err)
		}
		return settlementLoadJob{command: settleBatchCommand, request: request, batch: batch, scheduled: scheduled}
	}
	offset := 0
	if len(users) > profile.Tables*profile.Seats {
		offset = 600
	}
	table := (tick - 1) % profile.Tables
	if profile.Rotation {
		offset += ((tick - 1) / 5000) * 20600
	}
	first := users[(offset+table*profile.Seats)%len(users)]
	if hot && tick%5 == 0 {
		first = users[0]
	}
	ids := make([]uint64, profile.Seats)
	for i := range ids {
		ids[i] = users[(offset+table*profile.Seats+i)%len(users)]
	}
	ids[0] = first
	changes := make([]any, 0, profile.Seats*2)
	for i, user := range ids {
		debit, credit := int64(0), int64(50)
		if i == 0 {
			debit, credit = int64(50*(profile.Seats-1)), 0
		}
		changes = append(changes, map[string]any{"user_id": user, "reason": "service_fee", "debit": int64(1), "credit": int64(0)}, map[string]any{"user_id": user, "reason": "game_win_loss", "debit": debit, "credit": credit})
	}
	request := map[string]any{"version": uint64(2), "operation_id": id, "business": prefix, "business_ref": fmt.Sprintf("table-%d", table), "detail": map[string]any{}, "changes": changes}
	batch, err := decodeSettlementGroup(request)
	if err != nil {
		t.Fatal(err)
	}
	return settlementLoadJob{command: settleGroupCommand, request: request, batch: batch, scheduled: scheduled}
}

// TestSettlementLoad 保护真实 TCP/MySQL 持续负载下金额、版本和操作总额可对账；仅显式开启。
func TestSettlementLoad(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_LOAD") != "1" {
		t.Skip("set GSRDEMO_SETTLEMENT_LOAD=1 and GSRDEMO_SETTLEMENT_MYSQL=1")
	}
	duration := 30 * time.Second
	if value := os.Getenv("GSRDEMO_SETTLEMENT_LOAD_DURATION"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < time.Second || parsed > 5*time.Minute {
			t.Fatal("load duration must be 1s..5m")
		}
		duration = parsed
	}
	var reports []settlementLoadReport
	for _, name := range []string{"A", "B", "C", "D"} {
		t.Run(name, func(t *testing.T) {
			report := runSettlementLoad(t, name, duration, currentLoadProfile(t))
			reports = append(reports, report)
			encoded, err := json.MarshalIndent(reports, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := "../../.cache/settlement-load.json"
			if currentLoadProfile(t).External {
				path = "../../.cache/settlement-load-four.json"
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s", encoded)
		})
	}
}

func runSettlementLoad(t *testing.T, name string, duration time.Duration, profile settlementLoadProfile) settlementLoadReport {
	t.Helper()
	store, config := liveSettlementStore(t)
	wallets := 600 + profile.Tables*profile.Seats
	if name == "A" {
		wallets = 600
	}
	if name == "B" {
		wallets = profile.Tables * profile.Seats
	}
	sample := newLiveSettlementSampleAmount(t, store, wallets, 1_000_000_000)
	return executeSettlementLoad(t, name, duration, profile, store, config, sample, nil)
}

func executeSettlementLoad(t *testing.T, name string, duration time.Duration, profile settlementLoadProfile, store settlementStore, config settlementConfig, sample *liveSettlementSample, baseline map[uint64]walletBalance) settlementLoadReport {
	t.Helper()
	wallets := len(sample.Users)
	prefix := "load-" + name + "-" + random.Text()
	if sample.Business != "" {
		prefix = sample.Business
	}
	address := ""
	serverPID := 0
	if profile.External {
		address, serverPID = startSettlementLoadProcess(t, config)
	} else {
		address, _ = startSettlementTestNode(t, store.db)
	}
	jobs, results := make(chan settlementLoadJob, 256), make(chan settlementLoadResult, 512)
	stop := startSettlementLoadCaller(t, address, jobs, results)
	report := settlementLoadReport{Case: name, Seconds: duration.Seconds(), Wallets: wallets, Tables: profile.Tables, Seats: profile.Seats, External: profile.External}
	var stopSampler func() []loadProcessUsage
	if profile.External {
		stopSampler = startLoadProcessSampler(t, serverPID)
	}
	expected := make(map[uint64]walletBalance, len(sample.Users))
	for _, user := range sample.Users {
		if baseline == nil {
			expected[user] = walletBalance{UserID: user, Amount: 1_000_000_000, Version: 1}
		} else {
			expected[user] = baseline[user]
		}
	}
	var latencies, rpcTimes, moleLatencies, cardLatencies []time.Duration
	var totalDebit, totalCredit int64
	var databaseBefore map[string]uint64
	if baseline != nil {
		databaseBefore = readLoadDatabaseCounters(t, store)
	}
	started := time.Now()
	cpu := loadCPUSeconds()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	ticks := int(duration / (10 * time.Millisecond))
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	sent, received := 0, 0
	consume := func(result settlementLoadResult) {
		received++
		report.CompletedRPC++
		latencies = append(latencies, result.elapsed)
		if result.job.command == settleBatchCommand {
			moleLatencies = append(moleLatencies, result.elapsed)
		} else {
			cardLatencies = append(cardLatencies, result.elapsed)
		}
		rpcTimes = append(rpcTimes, result.rpc)
		if result.err != nil {
			report.Errors++
			t.Logf("RPC error: %v", result.err)
			return
		}
		outcomes, ok := result.receipt["results"].([]any)
		if !ok || len(outcomes) != len(result.job.batch.Operations) {
			report.Errors++
			return
		}
		for i, operation := range result.job.batch.Operations {
			if outcomes[i].(map[string]any)["status"] != "committed" {
				report.Rejected++
				continue
			}
			report.Operations++
			if result.job.command == settleBatchCommand {
				report.MoleOperations++
			} else {
				report.CardOperations++
			}
			users := make(map[uint64]bool)
			for _, change := range operation.Changes {
				wallet := expected[change.UserID]
				wallet.Amount += change.Credit - change.Debit
				expected[change.UserID] = wallet
				users[change.UserID] = true
				totalDebit += change.Debit
				totalCredit += change.Credit
			}
			for user := range users {
				wallet := expected[user]
				wallet.Version++
				expected[user] = wallet
			}
		}
	}
	enqueue := func(job settlementLoadJob) {
		report.PlannedRPC++
		// 所有生成身份都归测试样本；超时也可能已入账，清理时仍按这些身份收回。
		sample.Batches = append(sample.Batches, job.batch.ID)
		for _, operation := range job.batch.Operations {
			sample.Operations = append(sample.Operations, operation.ID)
		}
		select {
		case jobs <- job:
			sent++
		default:
			report.DroppedRPC++
		}
	}
	for tick := 1; tick <= ticks; tick++ {
		due := started.Add(time.Duration(tick) * 10 * time.Millisecond)
		for time.Now().Before(due) {
			select {
			case result := <-results:
				consume(result)
			case <-ticker.C:
			}
		}
		if name != "B" && tick%10 == 0 {
			enqueue(loadJob(t, "mole", prefix, tick, due, sample.Users, true, name == "D", profile))
		}
		if name != "A" {
			for slot := 0; slot < profile.CardsPerTick; slot++ {
				enqueue(loadJob(t, "cards", prefix, (tick-1)*profile.CardsPerTick+slot+1, due, sample.Users, false, name == "D", profile))
			}
		}
	}
	drain := time.Now()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for received < sent {
		select {
		case result := <-results:
			consume(result)
		case <-deadline.C:
			t.Fatalf("load drain timeout: sent=%d received=%d", sent, received)
		}
	}
	report.DrainMS = float64(time.Since(drain)) / float64(time.Millisecond)
	if stopSampler != nil {
		report.Processes = stopSampler()
	}
	elapsed := time.Since(started)
	report.OperationsPerSecond = float64(report.Operations) / elapsed.Seconds()
	report.MoleP99MS = loadPercentile(moleLatencies, 99)
	report.CardP99MS = loadPercentile(cardLatencies, 99)
	report.P50MS = loadPercentile(latencies, 50)
	report.P95MS = loadPercentile(latencies, 95)
	report.P99MS = loadPercentile(latencies, 99)
	report.MaxMS = loadPercentile(latencies, 100)
	report.RPCP99MS = loadPercentile(rpcTimes, 99)
	if databaseBefore != nil {
		after := readLoadDatabaseCounters(t, store)
		report.DatabaseCounters = make(map[string]uint64)
		for key, value := range after {
			report.DatabaseCounters[key] = value - databaseBefore[key]
		}
	}
	report.CPUSeconds = loadCPUSeconds() - cpu
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	report.HeapMiB = float64(after.HeapAlloc) / (1024 * 1024)
	report.GCCycles = after.NumGC - before.NumGC
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ids := append([]uint64(nil), sample.Users...)
	// 分块回查，避免大钱包池超过 MySQL 单条语句参数上限。
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		report.VerificationError = "begin wallet verification: " + err.Error()
		t.Error(report.VerificationError)
		return report
	}
	actuals, err := readHistorySubset(ctx, tx, ids)
	_ = tx.Rollback()
	if err != nil {
		report.VerificationError = "read wallet verification: " + err.Error()
		t.Error(report.VerificationError)
		return report
	}
	matches := 0
	for id, actual := range actuals {
		if actual != expected[id] {
			t.Errorf("wallet mismatch: actual=%+v expected=%+v", actual, expected[id])
		} else {
			matches++
		}
	}

	summary, err := store.summary(ctx, prefix)
	if err != nil {
		report.VerificationError = "load business summary: " + err.Error()
		t.Error(report.VerificationError)
		return report
	}
	report.Reconciled = matches == len(expected) && summary["operation_count"] == int64(report.Operations) && summary["debit"] == totalDebit && summary["credit"] == totalCredit
	if !report.Reconciled || report.Errors != 0 || report.DroppedRPC != 0 || report.Rejected != 0 {
		t.Errorf("load failed: %+v summary=%v", report, summary)
	}
	return report
}

func startSettlementLoadProcess(t *testing.T, config settlementConfig) (string, int) {
	t.Helper()
	directory := t.TempDir()
	address := freeBusinessAddress(t)
	nodes := fmt.Sprintf("nodes:\n  settlement: %s\n", address)
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte(nodes), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("instance: settlement-load\nlisten: settlement\nnodes_file: nodes.yaml\nlog_directory: %s\nmysql_dsn: %s\n", strconv.Quote(filepath.Join(directory, "logs")), strconv.Quote(config.MySQLDSN))
	path := filepath.Join(directory, "settlement.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	pid, _ := startBusinessProcessPID(t, "settlement", path, address)
	return address, pid
}

func historyInsertKey(profile settlementLoadProfile, index uint64) string {
	// SplitMix64 只用来可复现地分散压测键，不用于玩法随机或安全用途。
	number := index + 0x9e3779b97f4a7c15
	number = (number ^ (number >> 30)) * 0xbf58476d1ce4e5b9
	number = (number ^ (number >> 27)) * 0x94d049bb133111eb
	number = (number ^ (number >> 31)) % profile.HistoryGames
	if number%3200 >= 3000 {
		number -= number%3200 - 2999
	}
	user := number%profile.HistoryWallets + 1
	return fmt.Sprintf("%s-h-%04d-%012d", profile.HistoryDataset, user%1024, number)
}
