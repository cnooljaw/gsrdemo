package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type settlementLoadJob struct {
	id      string
	request map[string]any
}

type settlementLoadResult struct {
	job     settlementLoadJob
	elapsed time.Duration
	retries int
	err     error
	receipt map[string]any
}

// settlementLoadCaller 模拟独立业务来源；每个来源同步 Call，只有一个在途批次。
type settlementLoadCaller struct {
	jobs    []settlementLoadJob
	results chan<- settlementLoadResult
	current int
	started time.Time
	retries int
}

var _ gsr.Service = (*settlementLoadCaller)(nil)

func (*settlementLoadCaller) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*settlementLoadCaller) OnStop(context.Context) error                      { return nil }

func (caller *settlementLoadCaller) Handle(self gsr.CommandContext, command runtime.CommandID, _ ...any) (any, error) {
	if command != 1 {
		return nil, runtime.ErrUnknownCommand
	}
	if caller.current >= len(caller.jobs) {
		return nil, nil
	}
	job := caller.jobs[caller.current]
	if caller.started.IsZero() {
		caller.started = time.Now()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	value, err := self.Cluster().Call(ctx, "settlement", settlementWriteName, settleBatchCommand, job.request)
	cancel()
	if err != nil && retryableSettlementLoadError(err) && time.Since(caller.started) < 15*time.Second {
		caller.retries++
		_, err = self.After(20*time.Millisecond, 1)
		return nil, err
	}
	receipt, ok := value.(map[string]any)
	if err == nil && !ok {
		err = fmt.Errorf("invalid settlement receipt: %T", value)
	}
	caller.results <- settlementLoadResult{job, time.Since(caller.started), caller.retries, err, receipt}
	caller.current++
	caller.started, caller.retries = time.Time{}, 0
	if caller.current == len(caller.jobs) {
		self.Exit()
		return nil, nil
	}
	_, err = self.After(time.Nanosecond, 1)
	return nil, err
}

func retryableSettlementLoadError(err error) bool {
	return errors.Is(err, errSettlementUnavailable) || errors.Is(err, runtime.ErrMailboxFull) ||
		errors.Is(err, runtime.ErrCallTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// settlementLoadMain 只创建独立调用者；统一发起后记录开始时间，不开放回调监听。
type settlementLoadMain struct {
	callers []*settlementLoadCaller
	started chan time.Time
}

var _ gsr.Service = (*settlementLoadMain)(nil)

func (main *settlementLoadMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	ids := make([]runtime.ServiceID, 0, len(main.callers))
	for _, caller := range main.callers {
		id, err := self.CreateService(ctx, caller)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	started := time.Now()
	for _, id := range ids {
		if err := self.Send(id, 1); err != nil {
			return err
		}
	}
	main.started <- started
	self.Exit()
	return nil
}
func (*settlementLoadMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (*settlementLoadMain) OnStop(context.Context) error { return nil }

func settlementLoadPercentile(samples []time.Duration, percent int) float64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return float64(samples[(len(samples)-1)*percent/100]) / float64(time.Millisecond)
}

type settlementLoadLimits struct {
	workers, connections, mailbox int
}

type settlementLoadReport struct {
	Round               int     `json:"round"`
	Sources             int     `json:"sources"`
	Workers             int     `json:"workers"`
	Connections         int     `json:"mysql_connections"`
	Mailbox             int     `json:"mailbox_capacity"`
	BatchSize           int     `json:"batch_size"`
	RPC                 int     `json:"completed_rpc"`
	Operations          int     `json:"operations"`
	Seconds             float64 `json:"seconds"`
	OperationsPerSecond float64 `json:"operations_per_second"`
	P95MS               float64 `json:"p95_ms"`
	P99MS               float64 `json:"p99_ms"`
	Retries             int     `json:"retries"`
	Errors              int     `json:"errors"`
	DBWaitCount         int64   `json:"mysql_pool_wait_count"`
	DBWaitMS            float64 `json:"mysql_pool_wait_ms"`
	Verified            bool    `json:"verified"`
}

func settlementLoadNodes(t *testing.T, db *sql.DB, workers, mailbox int, callers []*settlementLoadCaller) (time.Time, func()) {
	t.Helper()
	address, stopTarget := startSettlementNode(t, db, workers, mailbox)
	main := &settlementLoadMain{callers: callers, started: make(chan time.Time, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	config := gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": address}}}
	go func() { done <- gsr.Run(ctx, config, main) }()
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
				t.Error("Call 压测 Runtime 未停止")
			}
			stopTarget()
		})
	}
	t.Cleanup(stop)
	select {
	case started := <-main.started:
		return started, stop
	case err := <-done:
		done <- err
		t.Fatalf("Call 压测调用者启动失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Call 压测调用者未启动")
	}
	return time.Time{}, stop
}

func runPoolLoadCase(t *testing.T, store settlementStore, sample *liveSettlementSample, limits settlementLoadLimits, batchSize, jobs, streams int) settlementLoadReport {
	workers, connections, mailbox := limits.workers, limits.connections, limits.mailbox
	firstOperation, firstBatch := len(sample.Operations), len(sample.Batches)
	t.Helper()
	store.db.SetMaxOpenConns(connections)
	store.db.SetMaxIdleConns(connections)
	prefix := "pool-load-" + rand.Text()
	results := make(chan settlementLoadResult, jobs)
	callers := make([]*settlementLoadCaller, streams)
	expected := map[uint64]int64{}
	for index := range streams {
		callers[index] = &settlementLoadCaller{results: results}
	}
	for index := range jobs {
		stream := index % streams
		id := fmt.Sprintf("%s-%d", prefix, index)
		operations := make([]map[string]any, batchSize)
		for operation := range batchSize {
			uid := sample.Users[stream*8+operation%8]
			opID := fmt.Sprintf("%s-op-%d", id, operation)
			operations[operation] = testSingle(opID, uid, 1, 0)
			operations[operation]["business"] = prefix
			sample.Operations = append(sample.Operations, opID)
			expected[uid]++
		}
		request := testBatch(id, operations...)
		sample.Batches = append(sample.Batches, id)
		callers[stream].jobs = append(callers[stream].jobs, settlementLoadJob{id, request})
	}
	before := map[uint64]walletBalance{}
	for uid := range expected {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		fields, err := store.getBalance(ctx, uid)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		before[uid] = walletBalance{UserID: uid, Amount: fields["amount"].(int64), Version: fields["version"].(uint64)}
	}
	stats := store.db.Stats()
	start, stop := settlementLoadNodes(t, store.db, workers, mailbox, callers)
	defer stop()
	report := settlementLoadReport{Sources: streams, Workers: workers, Connections: connections, Mailbox: mailbox, BatchSize: batchSize, RPC: jobs, Operations: jobs * batchSize}
	samples := make([]time.Duration, 0, jobs)
	for range jobs {
		select {
		case result := <-results:
			samples = append(samples, result.elapsed)
			report.Retries += result.retries
			if result.err != nil {
				report.Errors++
				t.Errorf("Call 批次失败: %v", result.err)
				continue
			}
			rows, ok := result.receipt["results"].([]any)
			if !ok || len(rows) != batchSize || result.receipt["batch_id"] != result.job.id {
				t.Fatal("批次回执身份或数量错误")
			}
			seen := map[string]bool{}
			for _, value := range rows {
				row := value.(map[string]any)
				id := row["operation_id"].(string)
				if seen[id] || row["status"] != "committed" || row["origin_batch"] != result.job.id {
					t.Fatal("批次重复、拒绝或来源错误")
				}
				seen[id] = true
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Call 压测没有完成")
		}
	}
	report.Seconds = time.Since(start).Seconds()
	report.OperationsPerSecond = float64(report.Operations) / report.Seconds
	report.P95MS, report.P99MS = settlementLoadPercentile(samples, 95), settlementLoadPercentile(samples, 99)
	after := store.db.Stats()
	report.DBWaitCount = after.WaitCount - stats.WaitCount
	report.DBWaitMS = float64(after.WaitDuration-stats.WaitDuration) / float64(time.Millisecond)
	for uid, count := range expected {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		fields, err := store.getBalance(ctx, uid)
		cancel()
		if err != nil || fields["amount"] != before[uid].Amount-count || fields["version"] != before[uid].Version+uint64(count) {
			t.Fatalf("钱包余额/版本未对齐: user=%d count=%d result=%v err=%v", uid, count, fields, err)
		}
	}
	operations := settlementCountRows(t, store.db, "settlement_operations", "operation_id", sample.Operations[firstOperation:])
	batches := settlementCountRows(t, store.db, "settlement_batches", "batch_id", sample.Batches[firstBatch:])
	if operations != report.Operations || batches != jobs {
		t.Fatalf("账务数量不一致: operations=%d batches=%d", operations, batches)
	}
	report.Verified = true
	t.Logf("sources=%d workers=%d connections=%d mailbox=%d batch=%d: %.0f op/s, P99 %.1fms, retries=%d, pool_wait=%d, 对账通过", streams, workers, connections, mailbox, batchSize, report.OperationsPerSecond, report.P99MS, report.Retries, report.DBWaitCount)
	return report
}

func settlementCountRows(t *testing.T, db *sql.DB, table, key string, ids []string) int {
	t.Helper()
	total := 0
	for first := 0; first < len(ids); first += 500 {
		last := min(first+500, len(ids))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var count int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+key+" IN ("+sqlSlots(last-first)+")", stringSQLArgs(ids[first:last])...).Scan(&count)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		total += count
	}
	return total
}

// TestSettlementPoolLoadLiveMySQL 比较真实 Cluster Call 池的并行度、连接数、邮箱与批次；排除启动时间，每档核对余额、版本和唯一账务。
func TestSettlementPoolLoadLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_POOL_LOAD") != "1" {
		t.Skip("set GSRDEMO_SETTLEMENT_POOL_LOAD=1 and GSRDEMO_SETTLEMENT_MYSQL=1")
	}
	store, _ := liveSettlementStore(t)
	sources, jobs, rounds := 16, 256, 1
	if os.Getenv("GSRDEMO_SETTLEMENT_POOL_CONFIRM") == "1" {
		sources, jobs, rounds = 64, 512, 3
	}
	sample := newLiveSettlementSampleAmount(t, store, sources*8, 1_000_000_000)
	reports := []settlementLoadReport{}
	// 相同业务来源和批次下逐档比较；多轮时按吞吐中位数选择下一阶段基线。
	selectLimits := func(options []settlementLoadLimits, repetitions int) settlementLoadLimits {
		seen := map[settlementLoadLimits]bool{}
		var best settlementLoadLimits
		var bestRate float64
		for _, limits := range options {
			if seen[limits] {
				continue
			}
			seen[limits] = true
			rates := make([]float64, 0, repetitions)
			for round := range repetitions {
				report := runPoolLoadCase(t, store, sample, limits, 64, jobs, sources)
				report.Round = round + 1
				reports = append(reports, report)
				if report.Errors != 0 || !report.Verified {
					t.Fatal("无效压测样本不能参与参数选择")
				}
				rates = append(rates, report.OperationsPerSecond)
			}
			sort.Float64s(rates)
			median := rates[len(rates)/2]
			if median > bestRate {
				best, bestRate = limits, median
			}
		}
		return best
	}
	options := []settlementLoadLimits{}
	for _, workers := range []int{1, 2, 4, 8, 16} {
		options = append(options, settlementLoadLimits{workers, workers + 2, 32})
	}
	best := selectLimits(options, rounds)
	options = nil
	for _, connections := range []int{best.connections, max(2, best.workers/2), best.workers, best.workers * 2} {
		options = append(options, settlementLoadLimits{best.workers, connections, best.mailbox})
	}
	best = selectLimits(options, 1)
	options = nil
	for _, mailbox := range []int{4, 16, 32, 64} {
		options = append(options, settlementLoadLimits{best.workers, best.connections, mailbox})
	}
	best = selectLimits(options, 1)
	for _, size := range []int{1, 32, 128, 500} {
		batchJobs := max(sources, 8192/size)
		report := runPoolLoadCase(t, store, sample, best, size, batchJobs, sources)
		report.Round = 1
		reports = append(reports, report)
	}
	path := "../../.cache/settlement-pool-load.json"
	if os.Getenv("GSRDEMO_SETTLEMENT_POOL_CONFIRM") == "1" {
		path = "../../.cache/settlement-pool-confirm.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
