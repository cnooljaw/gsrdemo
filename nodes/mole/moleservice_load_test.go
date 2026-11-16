package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	gsrruntime "github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

const (
	moleLoadStartCommand gsrruntime.CommandID = 980
	moleLoadStopCommand  gsrruntime.CommandID = 981
	moleLoadSinkCommand  gsrruntime.CommandID = 982
	moleLoadDuration                          = 5 * time.Second
	moleLoadCallers                           = 4
	moleLoadPushNodes                         = 2
	moleLoadPairPeriod                        = 20 * time.Millisecond
)

type moleLoadLatency struct {
	Samples int     `json:"samples"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
}

type moleLoadReport struct {
	GoVersion     string          `json:"go_version"`
	GOMAXPROCS    int             `json:"gomaxprocs"`
	Members       int             `json:"members"`
	Groups        int             `json:"groups"`
	Refresh       string          `json:"refresh"`
	Seconds       float64         `json:"seconds"`
	RPCPerSecond  float64         `json:"rpc_per_second"`
	GeneratorLag  float64         `json:"generator_max_lag_ms"`
	Tick          moleLoadLatency `json:"tick"`
	Hit           moleLoadLatency `json:"hit"`
	Snapshot      moleLoadLatency `json:"snapshot"`
	ScheduledHit  moleLoadLatency `json:"scheduled_hit"`
	ScheduledView moleLoadLatency `json:"scheduled_snapshot"`
	RefreshWork   moleLoadLatency `json:"refresh_work"`
	Refreshes     uint64          `json:"target_refreshes"`
	RefreshMin    uint64          `json:"refreshes_per_group_min"`
	RefreshMax    uint64          `json:"refreshes_per_group_max"`
	RefreshLag    float64         `json:"oldest_refresh_lag_ms"`
	AcceptedHits  int             `json:"accepted_hits"`
	Unavailable   int             `json:"target_unavailable"`
	RequestErrors int             `json:"request_errors"`
	Errors        []string        `json:"errors,omitempty"`
	PushAttempted uint64          `json:"push_attempted"`
	PushAccepted  uint64          `json:"push_accepted"`
	PushReceived  uint64          `json:"push_received"`
	HeapBeforeMB  float64         `json:"heap_before_mib"`
	HeapAfterMB   float64         `json:"heap_after_mib"`
	AllocatedMB   float64         `json:"allocated_mib"`
	GCCycles      uint32          `json:"gc_cycles"`
	Verified      bool            `json:"verified"`
}

// moleLoadCodec 保留发布版 ValueCodec，只统计符合本用例推送形状的编码尝试。
type moleLoadCodec struct {
	cluster.PayloadCodec
	pushes atomic.Uint64
}

var _ cluster.PayloadCodec = (*moleLoadCodec)(nil)

func (codec *moleLoadCodec) Encode(value any) ([]byte, error) {
	if args, ok := value.([]any); ok && len(args) == 2 && args[0] == "load" {
		codec.pushes.Add(1)
	}
	return codec.PayloadCodec.Encode(value)
}

// 业务错误单独计数，日志队列丢弃也计入拒绝数；不依赖可能丢失的日志文本推断接纳率。
type moleLoadLog struct{ lines atomic.Uint64 }

func (writer *moleLoadLog) Write(data []byte) (int, error) {
	writer.lines.Add(uint64(bytes.Count(data, []byte{'\n'})))
	return len(data), nil
}

type moleLoadContext struct {
	gsr.CommandContext
	logger *logging.ServiceLogger
}

var _ gsr.CommandContext = (*moleLoadContext)(nil)

func (self *moleLoadContext) Logger() *logging.ServiceLogger { return self.logger }

// moleLoadOwner 只增加测量窗口与观测，不改真实 Tick/Hit/Snapshot 的实现和调用方式。
type moleLoadOwner struct {
	*moleService
	logger                            *logging.Logger
	refresh                           string
	begin, end                        time.Time
	ticks                             []time.Duration
	refreshWork                       []time.Duration
	acceptedHits                      int
	refreshes, refreshMin, refreshMax uint64
	refreshLag                        time.Duration
}

var _ gsr.Service = (*moleLoadOwner)(nil)

func (owner *moleLoadOwner) Handle(self gsr.CommandContext, command gsrruntime.CommandID, args ...any) (any, error) {
	switch command {
	case moleLoadStartCommand:
		owner.begin = time.Now().Add(500 * time.Millisecond)
		owner.end = owner.begin.Add(moleLoadDuration)
		index := 0
		ordered := make([]*moleGroup, 0, len(owner.groups))
		for _, group := range owner.groups {
			group.game.spawn(owner.begin)
			offset := spawnPeriod
			if owner.refresh == "staggered" {
				offset = time.Duration(index%10+1) * accountingPeriod
			}
			group.nextSpawnAt = owner.begin.Add(offset)
			ordered = append(ordered, group)
			index++
		}
		// 只有夹具会批量改截止时间，按新时间重建真实刷新队列；不改变生产调度规则。
		slices.SortFunc(ordered, func(a, b *moleGroup) int { return a.nextSpawnAt.Compare(b.nextSpawnAt) })
		owner.spawnOrder.Init()
		for _, group := range ordered {
			group.spawnElement = owner.spawnOrder.PushBack(group)
		}
		return owner.begin.UnixNano(), nil
	case moleLoadStopCommand:
		// 调用方已停止发请求，之后的 Tick 不再产生新广播；先排空之前已接纳的出站消息。
		owner.end = time.Time{}
		owner.refreshMin = ^uint64(0)
		for _, group := range owner.groups {
			count := group.game.sequence - 2 // 建组和预装各生成一次，均不属于计时刷新。
			owner.refreshes += count
			owner.refreshMin, owner.refreshMax = min(owner.refreshMin, count), max(owner.refreshMax, count)
			owner.refreshLag = max(owner.refreshLag, time.Since(group.nextSpawnAt))
		}
		var received uint64
		for node := range moleLoadPushNodes {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for {
				value, err := self.Cluster().Call(ctx, moleLoadNode(node), "load-agent", moleLoadSinkCommand)
				if errors.Is(err, cluster.ErrOutboundQueueFull) && ctx.Err() == nil {
					time.Sleep(time.Millisecond)
					continue
				}
				cancel()
				if err != nil {
					return nil, err
				}
				fields := value.(map[string]any)
				if fields["invalid"] != uint64(0) {
					return nil, fmt.Errorf("invalid push payload: %v", fields)
				}
				received += fields["received"].(uint64)
				break
			}
		}
		if len(owner.users) != len(owner.accounting.accounts) || len(owner.accounting.pending) != owner.acceptedHits {
			return nil, errors.New("load changed membership or lost accepted accounting")
		}
		for uid, name := range owner.users {
			if owner.groups[name] == nil || owner.groups[name].members[uid].UserID != uid {
				return nil, errors.New("load left an invalid member index")
			}
			for id := range owner.accounting.accounts[uid].Pending {
				if owner.accounting.pending[id].Result.Event.UserID != uid {
					return nil, errors.New("load left an invalid pending index")
				}
			}
		}
		if owner.spawnOrder.Len() != len(owner.groups) {
			return nil, errors.New("load lost a group in the refresh order")
		}
		var previous time.Time
		for element := owner.spawnOrder.Front(); element != nil; element = element.Next() {
			group := element.Value.(*moleGroup)
			if group.spawnElement != element || group.nextSpawnAt.Before(previous) {
				return nil, errors.New("load broke the refresh deadline order")
			}
			previous = group.nextSpawnAt
		}
		return received, nil
	}
	if (command == moleTickCommand || command == moleRefreshCommand) && (time.Now().Before(owner.begin) || !time.Now().Before(owner.end)) {
		return nil, nil
	}
	before := time.Now()
	value, err := owner.moleService.Handle(&moleLoadContext{CommandContext: self, logger: owner.logger.WithService(uint64(self.Self()))}, command, args...)
	if command == moleTickCommand {
		owner.ticks = append(owner.ticks, time.Since(before))
	}
	if command == moleTickCommand || command == moleRefreshCommand {
		owner.refreshWork = append(owner.refreshWork, time.Since(before))
	}
	if command == moleHitCommand && err == nil {
		owner.acceptedHits++
	}
	return value, err
}

type moleLoadSink struct{ received, invalid uint64 }

var _ gsr.Service = (*moleLoadSink)(nil)

func (*moleLoadSink) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*moleLoadSink) OnStop(context.Context) error                      { return nil }
func (sink *moleLoadSink) Handle(_ gsr.CommandContext, command gsrruntime.CommandID, args ...any) (any, error) {
	if command == moleLoadSinkCommand {
		return map[string]any{"received": sink.received, "invalid": sink.invalid}, nil
	}
	if command != accessagent.MoleSnapshotPushCommand || len(args) != 2 {
		sink.invalid++
		return nil, gsrruntime.ErrUnknownCommand
	}
	payload, ok := args[1].(map[string]any)
	name, _ := payload["attack_name"].(string)
	if !ok || args[0] != "load" || name == "" {
		sink.invalid++
		return nil, errInvalidMoleInput
	}
	sink.received++
	return nil, nil
}

type moleLoadCallerResult struct {
	hit, snapshot, scheduledHit, scheduledView []time.Duration
	accepted                                   int
	unavailable                                int
	errors                                     int
	err                                        string
	lag                                        time.Duration
	elapsed                                    time.Duration
}

// TestMoleOwnerLoad 测量单 owner 在刷新和业务请求混合时的等待、推送与内存，默认跳过，不访问 Redis/MySQL。
func TestMoleOwnerLoad(t *testing.T) {
	if os.Getenv("GSRDEMO_MOLE_LOAD") != "1" {
		t.Skip("run ./scripts/dev.sh load mole for the six short capacity samples")
	}
	var reports []moleLoadReport
	for _, members := range []int{1200, 12000, 100000} {
		for _, refresh := range []string{"staggered", "burst"} {
			t.Run(fmt.Sprintf("members=%d/%s", members, refresh), func(t *testing.T) {
				reports = append(reports, measureMoleOwner(t, members, refresh))
			})
		}
	}
	if err := os.MkdirAll("../../.cache", 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(reports, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("../../.cache/mole-owner-load.json", append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}

func moleLoadNode(index int) cluster.NodeID {
	return cluster.NodeID(fmt.Sprintf("load-access-%d", index))
}

func measureMoleOwner(t *testing.T, members int, refresh string) moleLoadReport {
	t.Helper()
	routes := make(map[cluster.NodeID]string)
	for node := range moleLoadPushNodes {
		name := moleLoadNode(node)
		address, _ := startMoleTestNode(t, string(name), nil, &moleTestPeer{node: string(name), name: "load-agent", service: &moleLoadSink{}})
		routes[name] = address
	}
	writer := &moleLoadLog{}
	logger, err := logging.New(logging.Config{Writer: writer})
	if err != nil {
		t.Fatal(err)
	}
	closeLogger := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := logger.Close(ctx); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(closeLogger)
	owner := &moleLoadOwner{moleService: readyMole(), logger: logger, refresh: refresh}
	// 只测玩法 owner，固定在途标记避免发送到 Submission；最多 1,000 锤，低于 4,096 项积压上限。
	owner.accounting.inFlightID = "load-held"
	now := time.Now()
	for index := 0; index < members; index++ {
		uid := uint64(index + 1)
		name := fmt.Sprintf("load-group-%d", index/attackSeats)
		group := owner.groups[name]
		if group == nil {
			group = newMoleGroup(name, now)
			owner.addGroup(group)
		}
		oldSize := len(group.members)
		group.members[uid] = moleMember{UserID: uid, LoginID: "load", AccessNode: string(moleLoadNode(index % moleLoadPushNodes)), AgentName: "load-agent", Seat: int64(index % attackSeats)}
		owner.updateAvailableGroup(group, oldSize)
		owner.users[uid] = name
		owner.accounting.updateBalance(uid, silverBalance{Amount: 1000000, Version: 1})
	}
	codec, err := cluster.NewValueCodec(cluster.ValueCodecConfig{})
	if err != nil {
		t.Fatal(err)
	}
	observedCodec := &moleLoadCodec{PayloadCodec: codec}
	address, stopOwner := startMoleLoadOwner(t, routes, owner, observedCodec)
	routes["mole"] = address
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started, err := moleRPC(t, routes, "mole", moleServiceName, moleLoadStartCommand)
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Unix(0, started.(int64))
	results := make(chan moleLoadCallerResult, moleLoadCallers)
	var callers sync.WaitGroup
	for peer := range moleLoadCallers {
		callers.Add(1)
		go func() {
			defer callers.Done()
			results <- runMoleLoadCaller(routes, members, peer, begin)
		}()
	}
	callers.Wait()
	close(results)
	report := moleLoadReport{GoVersion: runtime.Version(), GOMAXPROCS: runtime.GOMAXPROCS(0), Members: members, Groups: (members + attackSeats - 1) / attackSeats, Refresh: refresh}
	var hits, snapshots, scheduledHits, scheduledViews []time.Duration
	var elapsed, lag time.Duration
	for result := range results {
		hits = append(hits, result.hit...)
		snapshots = append(snapshots, result.snapshot...)
		scheduledHits = append(scheduledHits, result.scheduledHit...)
		scheduledViews = append(scheduledViews, result.scheduledView...)
		report.AcceptedHits += result.accepted
		report.Unavailable += result.unavailable
		report.RequestErrors += result.errors
		if result.err != "" {
			report.Errors = append(report.Errors, result.err)
		}
		elapsed, lag = max(elapsed, result.elapsed), max(lag, result.lag)
	}
	received, err := moleRPC(t, routes, "mole", moleServiceName, moleLoadStopCommand)
	if err != nil {
		t.Fatal(err)
	}
	report.PushReceived = received.(uint64)
	runtime.ReadMemStats(&after)
	stopOwner() // 等待 owner 退出后再读观测切片；不跨 owner 读取可变状态。
	closeLogger()
	rejected := writer.lines.Load() + logger.DroppedCount()
	report.PushAttempted = observedCodec.pushes.Load()
	if rejected > report.PushAttempted {
		t.Fatal("business log contains an error unrelated to push rejection")
	}
	report.PushAccepted = report.PushAttempted - rejected
	report.Tick, report.Hit, report.Snapshot = moleLoadPercentiles(owner.ticks), moleLoadPercentiles(hits), moleLoadPercentiles(snapshots)
	report.ScheduledHit, report.ScheduledView = moleLoadPercentiles(scheduledHits), moleLoadPercentiles(scheduledViews)
	report.RefreshWork = moleLoadPercentiles(owner.refreshWork)
	report.Refreshes, report.RefreshMin, report.RefreshMax = owner.refreshes, owner.refreshMin, owner.refreshMax
	report.RefreshLag = float64(owner.refreshLag) / float64(time.Millisecond)
	report.Seconds, report.GeneratorLag = elapsed.Seconds(), float64(lag)/float64(time.Millisecond)
	report.RPCPerSecond = float64(len(hits)+len(snapshots)) / report.Seconds
	report.HeapBeforeMB, report.HeapAfterMB = float64(before.HeapAlloc)/(1<<20), float64(after.HeapAlloc)/(1<<20)
	report.AllocatedMB, report.GCCycles = float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), after.NumGC-before.NumGC
	report.Verified = report.RequestErrors == 0 && report.AcceptedHits == owner.acceptedHits && report.PushReceived <= report.PushAccepted && len(owner.ticks) > 0 && len(hits) > 0 && len(snapshots) > 0
	data, _ := json.Marshal(report)
	t.Log(string(data))
	if !report.Verified {
		t.Error("load integrity check failed; see report")
	}
	return report
}

func startMoleLoadOwner(t *testing.T, routes map[cluster.NodeID]string, owner gsr.Service, codec cluster.PayloadCodec) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	observed := &moleObservedMain{service: &moleTestPeer{node: "mole", name: moleServiceName, service: owner}, ready: make(chan string, 1)}
	nodes := make(map[cluster.NodeID]string)
	for node, address := range routes {
		nodes[node] = address
	}
	nodes["mole"] = "127.0.0.1:0"
	done := make(chan error, 1)
	go func() {
		done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Writer: io.Discard}, Cluster: gsr.ClusterConfig{Nodes: nodes, Codec: codec}}, observed)
	}()
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
				t.Fatal("load owner shutdown timed out")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case address := <-observed.ready:
		return address, stop
	case err := <-done:
		done <- err // Cleanup 仍需观察启动结果。
		t.Fatalf("load owner startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("load owner startup timed out")
	}
	return "", stop
}

func runMoleLoadCaller(routes map[cluster.NodeID]string, members, peer int, begin time.Time) moleLoadCallerResult {
	var result moleLoadCallerResult
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	caller := &moleTestCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		for index := 0; index < int(moleLoadDuration/moleLoadPairPeriod); index++ {
			due := begin.Add(time.Duration(index)*moleLoadPairPeriod + time.Duration(peer)*time.Millisecond)
			timer := time.NewTimer(max(time.Until(due), 0))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			if !time.Now().Before(begin.Add(moleLoadDuration)) {
				break // 不补发超过窗口的负载；最大迟到与实际完成率明确报告这种背压。
			}
			result.lag = max(result.lag, time.Since(due))
			group := (index*moleLoadCallers + peer) % ((members + attackSeats - 1) / attackSeats)
			uid, name := uint64(group*attackSeats+1), fmt.Sprintf("load-group-%d", group)
			before := time.Now()
			value, err := self.Cluster().Call(ctx, "mole", moleServiceName, moleSnapshotCommand, uid, "load", name)
			result.snapshot = append(result.snapshot, time.Since(before))
			result.scheduledView = append(result.scheduledView, time.Since(due))
			if err != nil {
				return err
			}
			view, ok := value.(map[string]any)
			if !ok || view["attack_name"] != name {
				return errInvalidMoleInput
			}
			targets, ok := view["targets"].([]any)
			if !ok {
				return errInvalidMoleInput
			}
			var target map[string]any
			for _, value := range targets {
				candidate := value.(map[string]any)
				if candidate["claimed"] == false {
					target = candidate
					break
				}
			}
			if target == nil {
				continue
			}
			before = time.Now()
			_, err = self.Cluster().Call(ctx, "mole", moleServiceName, moleHitCommand, uid, "load", name,
				fmt.Sprintf("load-%d-%d", peer, index), target["spawn_seq"], target["hole"])
			result.hit = append(result.hit, time.Since(before))
			result.scheduledHit = append(result.scheduledHit, time.Since(due))
			switch {
			case err == nil:
				result.accepted++
			case errors.Is(err, errTargetUnavailable):
				result.unavailable++
			default:
				return err
			}
		}
		result.elapsed = max(time.Since(begin), moleLoadDuration)
		return nil
	}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Writer: io.Discard}, Cluster: gsr.ClusterConfig{Nodes: routes}}, caller); err != nil {
		result.errors++
		result.err = err.Error()
	}
	if result.elapsed == 0 {
		result.elapsed = max(time.Since(begin), moleLoadDuration)
	}
	return result
}

func moleLoadPercentiles(values []time.Duration) moleLoadLatency {
	if len(values) == 0 {
		return moleLoadLatency{}
	}
	slices.Sort(values)
	ms := func(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }
	return moleLoadLatency{Samples: len(values), P95MS: ms(values[(len(values)*95+99)/100-1]), P99MS: ms(values[(len(values)*99+99)/100-1]), MaxMS: ms(values[len(values)-1])}
}
