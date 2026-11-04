package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
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

type asyncLoadJob struct {
	id      string
	request map[string]any
}

type asyncLoadResult struct {
	job     asyncLoadJob
	elapsed time.Duration
	retries int
	err     error
	receipt map[string]any
}

// asyncLoadCaller 模拟独立业务来源：每个来源一个在途批次，重试保持同一身份。
type asyncLoadCaller struct {
	name    cluster.ServiceName
	jobs    []asyncLoadJob
	results chan<- asyncLoadResult
	current int
	started time.Time
	timer   runtime.TimerID
	retries int
}

var _ gsr.Service = (*asyncLoadCaller)(nil)

func (caller *asyncLoadCaller) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (caller *asyncLoadCaller) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if caller.current >= len(caller.jobs) {
		return nil, nil
	}
	job := caller.jobs[caller.current]
	switch command {
	case 1:
		if caller.started.IsZero() {
			caller.started = time.Now()
		}
		if time.Since(caller.started) > 15*time.Second {
			caller.finish(self, nil, fmt.Errorf("original batch timed out"))
			return nil, nil
		}
		if err := self.Cluster().Send("settlement", settlementSubmitName, settleBatchCommand, job.request, "caller", string(caller.name)); err != nil {
			caller.retries++
			caller.timer, _ = self.After(20*time.Millisecond, 1)
			return nil, nil
		}
		caller.timer, _ = self.After(5*time.Second, 2)
	case 2:
		caller.retries++
		caller.timer, _ = self.After(20*time.Millisecond, 1)
	case settlementResultCommand:
		if args[0] != job.id {
			return nil, nil
		}
		self.CancelTimer(caller.timer)
		code := args[2].(string)
		if code == "settlement.busy" || code == "settlement.store_unavailable" {
			caller.retries++
			caller.timer, _ = self.After(20*time.Millisecond, 1)
			return nil, nil
		}
		if code != "" {
			caller.finish(self, nil, fmt.Errorf("%s", code))
			return nil, nil
		}
		caller.finish(self, args[1].(map[string]any), nil)
	default:
		return nil, runtime.ErrUnknownCommand
	}
	return nil, nil
}

func (caller *asyncLoadCaller) finish(self gsr.ServiceContext, receipt map[string]any, err error) {
	caller.results <- asyncLoadResult{caller.jobs[caller.current], time.Since(caller.started), caller.retries, err, receipt}
	caller.current++
	caller.started, caller.retries = time.Time{}, 0
	if caller.current < len(caller.jobs) {
		caller.timer, _ = self.After(time.Nanosecond, 1)
	}
}

func (caller *asyncLoadCaller) OnStop(context.Context) error {
	return nil
}

func asyncLoadPercentile(samples []time.Duration, percent int) float64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return float64(samples[(len(samples)-1)*percent/100]) / float64(time.Millisecond)
}

type asyncLoadReport struct {
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
	Verified            bool    `json:"verified"`
}

func asyncLoadNodes(t *testing.T, db *sql.DB, workers, mailbox int, callers []*asyncLoadCaller) func() {
	t.Helper()
	routes := map[cluster.NodeID]string{}
	listeners := []net.Listener{}
	for _, node := range []cluster.NodeID{"settlement", "caller"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		routes[node] = listener.Addr().String()
		listeners = append(listeners, listener)
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	children := map[string]gsr.Service{}
	for _, caller := range callers {
		children[string(caller.name)] = caller
	}
	stops := []func(){}
	for _, main := range []*asyncSettlementMain{
		{node: "settlement", children: map[string]gsr.Service{"": newSettlementSubmitService(db, workers)}, ready: make(chan struct{})},
		{node: "caller", children: children, ready: make(chan struct{}), startCommand: 1},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		capacity := 128
		if main.node == "settlement" {
			capacity = mailbox
		}
		config := gsr.Config{Runtime: gsr.RuntimeConfig{MailboxCapacity: capacity}, Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}
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
					t.Error("压测 Runtime 未停止")
				}
			})
		}
		stops = append(stops, stop)
		t.Cleanup(stop)
		select {
		case <-main.ready:
		case err := <-done:
			done <- err
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("压测节点未启动")
		}
	}
	return func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
}

func runAsyncLoadCase(t *testing.T, store settlementStore, sample *liveSettlementSample, workers, connections, mailbox, batchSize, jobs, streams int) asyncLoadReport {
	firstOperation, firstBatch := len(sample.Operations), len(sample.Batches)
	t.Helper()
	store.db.SetMaxOpenConns(connections)
	store.db.SetMaxIdleConns(connections)
	prefix := "async-load-" + rand.Text()
	results := make(chan asyncLoadResult, jobs)
	callers := make([]*asyncLoadCaller, streams)
	expected := map[uint64]int64{}
	for index := range streams {
		callers[index] = &asyncLoadCaller{name: cluster.ServiceName(fmt.Sprintf("load-%d", index)), results: results}
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
		callers[stream].jobs = append(callers[stream].jobs, asyncLoadJob{id, request})
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
	start := time.Now()
	stop := asyncLoadNodes(t, store.db, workers, mailbox, callers)
	defer stop()
	report := asyncLoadReport{Sources: streams, Workers: workers, Connections: connections, Mailbox: mailbox, BatchSize: batchSize, RPC: jobs, Operations: jobs * batchSize}
	samples := make([]time.Duration, 0, jobs)
	for range jobs {
		select {
		case result := <-results:
			samples = append(samples, result.elapsed)
			report.Retries += result.retries
			if result.err != nil {
				report.Errors++
				t.Errorf("异步批次失败: %v", result.err)
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
			t.Fatal("异步压测没有完成，保留失败数据诊断")
		}
	}
	report.Seconds = time.Since(start).Seconds()
	report.OperationsPerSecond = float64(report.Operations) / report.Seconds
	report.P95MS, report.P99MS = asyncLoadPercentile(samples, 95), asyncLoadPercentile(samples, 99)
	report.DBWaitCount = store.db.Stats().WaitCount - stats.WaitCount
	for uid, count := range expected {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		fields, err := store.getBalance(ctx, uid)
		cancel()
		if err != nil || fields["amount"] != before[uid].Amount-count || fields["version"] != before[uid].Version+uint64(count) {
			t.Fatalf("钱包余额/版本未对齐: user=%d count=%d result=%v err=%v", uid, count, fields, err)
		}
	}
	operations := asyncCountRows(t, store.db, "settlement_operations", "operation_id", sample.Operations[firstOperation:])
	batches := asyncCountRows(t, store.db, "settlement_batches", "batch_id", sample.Batches[firstBatch:])
	if operations != report.Operations || batches != jobs {
		t.Fatalf("账务数量不一致: operations=%d batches=%d", operations, batches)
	}
	report.Verified = true
	t.Logf("sources=%d workers=%d connections=%d mailbox=%d batch=%d: %.0f op/s, P99 %.1fms, retries=%d, pool_wait=%d, 对账通过", streams, workers, connections, mailbox, batchSize, report.OperationsPerSecond, report.P99MS, report.Retries, report.DBWaitCount)
	return report
}

func asyncCountRows(t *testing.T, db *sql.DB, table, key string, ids []string) int {
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

// TestSettlementAsyncLoadLiveMySQL 比较实际双向 TCP 的执行并行度、连接池、邮箱与批次大小，每档均核对余额、版本及唯一账务数量。
func TestSettlementAsyncLoadLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_ASYNC_LOAD") != "1" {
		t.Skip("set GSRDEMO_SETTLEMENT_ASYNC_LOAD=1 and GSRDEMO_SETTLEMENT_MYSQL=1")
	}
	store, _ := liveSettlementStore(t)
	wallets := 128
	if os.Getenv("GSRDEMO_SETTLEMENT_ASYNC_CONFIRM") == "1" {
		wallets = 512
	}
	sample := newLiveSettlementSampleAmount(t, store, wallets, 1_000_000_000)
	reports := []asyncLoadReport{}
	if os.Getenv("GSRDEMO_SETTLEMENT_ASYNC_CONFIRM") == "1" {
		for range 3 {
			for _, workers := range []int{2, 4, 8, 16} {
				reports = append(reports, runAsyncLoadCase(t, store, sample, workers, workers+2, 64, 64, 512, 64))
			}
		}
		for _, mailbox := range []int{16, 32, 128} {
			reports = append(reports, runAsyncLoadCase(t, store, sample, 4, 6, mailbox, 64, 512, 64))
		}
		for _, size := range []int{32, 128, 500} {
			reports = append(reports, runAsyncLoadCase(t, store, sample, 4, 6, 64, size, 128, 64))
		}
	} else {
		for _, workers := range []int{1, 2, 4, 8, 16} {
			reports = append(reports, runAsyncLoadCase(t, store, sample, workers, workers+2, 32, 64, 256, 16))
		}
		best := reports[0]
		for _, report := range reports {
			if report.Errors == 0 && report.OperationsPerSecond > best.OperationsPerSecond {
				best = report
			}
		}
		for _, connections := range []int{max(2, best.Workers/2), best.Workers, best.Workers * 2} {
			if connections == best.Connections {
				continue
			}
			reports = append(reports, runAsyncLoadCase(t, store, sample, best.Workers, connections, 32, 64, 256, 16))
		}
		for _, mailbox := range []int{4, 16, 64} {
			reports = append(reports, runAsyncLoadCase(t, store, sample, best.Workers, best.Connections, mailbox, 64, 256, 16))
		}
		for _, size := range []int{1, 32, 128, 500} {
			jobs := max(64, 8192/size)
			reports = append(reports, runAsyncLoadCase(t, store, sample, best.Workers, best.Connections, 32, size, jobs, 16))
		}
	}
	path := "../../.cache/settlement-async-load.json"
	if os.Getenv("GSRDEMO_SETTLEMENT_ASYNC_CONFIRM") == "1" {
		path = "../../.cache/settlement-async-confirm.json"
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
