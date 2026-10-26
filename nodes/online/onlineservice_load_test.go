package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
)

// TestOnlineClusterLoad 测量真实 TCP 多接入调用汇聚下两档目录规模的处理率和尾延迟，默认不运行容量测试。
func TestOnlineClusterLoad(t *testing.T) {
	if os.Getenv("GSRDEMO_ONLINE_LOAD") != "1" {
		t.Skip("set GSRDEMO_ONLINE_LOAD=1 to run the two one-minute capacity samples")
	}
	for _, sample := range []struct {
		name       string
		population int
		rate       int
	}{
		{"actual_12000", 12000, 2400},
		{"reserve_100000", 100000, 5000},
	} {
		t.Run(sample.name, func(t *testing.T) {
			measureOnlineLoad(t, sample.population, sample.rate)
		})
	}
}

func measureOnlineLoad(t *testing.T, population, rate int) {
	t.Helper()
	const peers = 4
	const seconds = 60
	address, _ := startOnline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ready, start := make(chan struct{}, peers), make(chan struct{})
	type outcome struct {
		latencies []time.Duration
		elapsed   time.Duration
		maxLag    time.Duration
		err       error
	}
	results := make(chan outcome, peers)
	var begin time.Time
	for peer := range peers {
		node, directory := fmt.Sprintf("access-%02d", peer+1), t.TempDir()
		go func() {
			result := outcome{latencies: make([]time.Duration, 0, seconds*rate/peers)}
			result.err = runOnlineAccess(ctx, address, node, directory, func(self gsr.ServiceContext) error {
				// 每个调用节点负责一组不重叠用户；全部走实际 Cluster，预装数据不计入定速样本。
				for index := peer; index < population; index += peers {
					agentName := fmt.Sprintf("agent-%d", index+1)
					if err := expectOnlineCall(self, bindCommand, nil, uint64(index+1), "seed", node, agentName); err != nil {
						return err
					}
				}
				ready <- struct{}{}
				if err := waitOnlineStep(ctx, start); err != nil {
					return err
				}
				currentLogins := make([]string, population/peers)
				for index := range currentLogins {
					currentLogins[index] = "seed"
				}
				operation := 0
				// 按绝对时刻投入 10ms 批次；迟到仍执行应有操作，并单独报告最大迟到。
				for tick := range seconds * 100 {
					due := begin.Add(time.Duration(tick) * 10 * time.Millisecond)
					if err := waitOnlineUntil(ctx, due); err != nil {
						return err
					}
					result.maxLag = max(result.maxLag, time.Since(due))
					limit := (tick + 1) * rate / (peers * 100)
					for operation < limit {
						index := (operation / 3) % len(currentLogins)
						userID := uint64(index*peers + peer + 1)
						agentName := fmt.Sprintf("agent-%d", userID)
						before := time.Now()
						var err error
						switch operation % 3 {
						case 0:
							loginID := fmt.Sprintf("load-%d-%d", peer, operation)
							err = expectOnlineCall(self, bindCommand, wantLocation(currentLogins[index], node, agentName), userID, loginID, node, agentName)
							currentLogins[index] = loginID
						case 1:
							err = expectOnlineCall(self, lookupCommand, wantLocation(currentLogins[index], node, agentName), userID)
						case 2:
							err = expectOnlineCall(self, unbindCommand, false, userID, "stale-login")
						}
						if err != nil {
							return err
						}
						result.latencies = append(result.latencies, time.Since(before))
						operation++
					}
				}
				if err := waitOnlineUntil(ctx, begin.Add(seconds*time.Second)); err != nil {
					return err
				}
				result.elapsed = time.Since(begin)
				// 检查每条最终记录；计时外清除自身记录，并确认成功解绑和重复解绑的结果。
				for index, loginID := range currentLogins {
					userID := uint64(index*peers + peer + 1)
					agentName := fmt.Sprintf("agent-%d", userID)
					if err := expectOnlineCall(self, lookupCommand, wantLocation(loginID, node, agentName), userID); err != nil {
						return err
					}
					if err := expectOnlineCall(self, unbindCommand, true, userID, loginID); err != nil {
						return err
					}
					if err := expectOnlineCall(self, unbindCommand, false, userID, loginID); err != nil {
						return err
					}
				}
				return nil
			})
			if result.err != nil {
				cancel()
			}
			results <- result
		}()
	}
	for range peers {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("directory preload did not complete")
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	begin = time.Now().Add(100 * time.Millisecond)
	close(start)
	var latencies []time.Duration
	var elapsed, maxLag time.Duration
	for range peers {
		result := <-results
		if result.err != nil {
			t.Errorf("load caller: %v", result.err)
		}
		latencies = append(latencies, result.latencies...)
		elapsed = max(elapsed, result.elapsed)
		maxLag = max(maxLag, result.maxLag)
	}
	runtime.ReadMemStats(&after)
	if t.Failed() {
		return
	}
	if len(latencies) != rate*seconds {
		t.Fatalf("completed=%d want=%d", len(latencies), rate*seconds)
	}
	slices.Sort(latencies)
	actualRate := float64(len(latencies)) / elapsed.Seconds()
	t.Logf("records=%d callers=%d target_rpc_s=%d actual_rpc_s=%.2f completed=%d elapsed=%s p50=%s p99=%s max=%s generator_max_lag=%s heap_before=%d heap_after=%d gc_cycles=%d gc_pause=%s",
		population, peers, rate, actualRate, len(latencies), elapsed,
		latencies[len(latencies)/2], latencies[(len(latencies)*99+99)/100-1], latencies[len(latencies)-1], maxLag,
		before.HeapAlloc, after.HeapAlloc, after.NumGC-before.NumGC, time.Duration(after.PauseTotalNs-before.PauseTotalNs))
	// 只检查发生器是否兑现输入预算；延迟指标用于本机观测，不擅自定义生产 SLA。
	if actualRate < float64(rate)*0.98 {
		t.Fatalf("actual input/completion rate is below 98%% of target: %.2f", actualRate)
	}
}

func waitOnlineUntil(ctx context.Context, due time.Time) error {
	timer := time.NewTimer(max(time.Until(due), 0))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
