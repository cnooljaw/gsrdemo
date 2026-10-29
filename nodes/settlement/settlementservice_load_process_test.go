package main

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type loadProcessPoint struct {
	Seconds    float64 `json:"seconds"`
	CPUPercent float64 `json:"cpu_percent"`
	RSSMiB     float64 `json:"rss_mib"`
}

type loadProcessUsage struct {
	Name              string             `json:"name"`
	PID               int                `json:"pid"`
	CPUSeconds        float64            `json:"cpu_seconds"`
	WallSeconds       float64            `json:"wall_seconds"`
	AverageCPUPercent float64            `json:"average_cpu_percent"`
	P95CPUPercent     float64            `json:"p95_cpu_percent"`
	MaxCPUPercent     float64            `json:"max_cpu_percent"`
	PeakRSSMiB        float64            `json:"peak_rss_mib"`
	Samples           []loadProcessPoint `json:"samples"`
}

type loadProcessSnapshot struct {
	at  time.Time
	cpu float64
	rss float64
}

// parseProcessCPU 读取 ps 的累计 CPU 时间；不使用带历史平滑的 %cpu 列。
func parseProcessCPU(value string) (float64, error) {
	days := 0.0
	if before, after, ok := strings.Cut(value, "-"); ok {
		parsed, err := strconv.ParseFloat(before, 64)
		if err != nil {
			return 0, err
		}
		days, value = parsed, after
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid ps CPU time %q", value)
	}
	total := 0.0
	for _, part := range parts {
		number, err := strconv.ParseFloat(part, 64)
		if err != nil || number < 0 {
			return 0, fmt.Errorf("invalid ps CPU time %q", value)
		}
		total = total*60 + number
	}
	return days*86400 + total, nil
}

func readLoadProcess(pid int) (loadProcessSnapshot, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "time=", "-o", "rss=").Output()
	if err != nil {
		return loadProcessSnapshot{}, fmt.Errorf("sample PID %d: %w", pid, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return loadProcessSnapshot{}, fmt.Errorf("unexpected ps columns for PID %d", pid)
	}
	cpu, err := parseProcessCPU(fields[0])
	if err != nil {
		return loadProcessSnapshot{}, err
	}
	rss, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return loadProcessSnapshot{}, err
	}
	return loadProcessSnapshot{at: time.Now(), cpu: cpu, rss: rss / 1024}, nil
}

// startLoadProcessSampler 由测试宿主启动和回收；每秒采样，只观察 MySQL，不停止或修改它。
func startLoadProcessSampler(t *testing.T, serverPID int) func() []loadProcessUsage {
	t.Helper()
	output, err := exec.Command("pgrep", "-x", "mysqld").Output()
	if err != nil {
		t.Fatalf("find local mysqld PID: %v", err)
	}
	matches := strings.Fields(string(output))
	if len(matches) != 1 {
		t.Fatal("CPU sampling requires exactly one local mysqld process")
	}
	mysqlPID, err := strconv.Atoi(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	usages := []loadProcessUsage{{Name: "settlement", PID: serverPID}, {Name: "mysqld", PID: mysqlPID}}
	first, previous := make([]loadProcessSnapshot, len(usages)), make([]loadProcessSnapshot, len(usages))
	for i := range usages {
		point, err := readLoadProcess(usages[i].PID)
		if err != nil {
			t.Fatal(err)
		}
		first[i], previous[i] = point, point
		usages[i].PeakRSSMiB = point.rss
	}
	stop, done := make(chan struct{}), make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		sample := func() error {
			for i := range usages {
				point, err := readLoadProcess(usages[i].PID)
				if err != nil {
					return err
				}
				seconds := point.at.Sub(previous[i].at).Seconds()
				cpu := 100 * (point.cpu - previous[i].cpu) / seconds
				if cpu < 0 {
					return fmt.Errorf("CPU counter moved backwards for PID %d", usages[i].PID)
				}
				usages[i].Samples = append(usages[i].Samples, loadProcessPoint{Seconds: point.at.Sub(first[i].at).Seconds(), CPUPercent: cpu, RSSMiB: point.rss})
				usages[i].CPUSeconds = point.cpu - first[i].cpu
				usages[i].WallSeconds = point.at.Sub(first[i].at).Seconds()
				usages[i].AverageCPUPercent = 100 * usages[i].CPUSeconds / usages[i].WallSeconds
				usages[i].MaxCPUPercent = max(usages[i].MaxCPUPercent, cpu)
				usages[i].PeakRSSMiB = max(usages[i].PeakRSSMiB, point.rss)
				previous[i] = point
			}
			return nil
		}
		for {
			select {
			case <-ticker.C:
				if err := sample(); err != nil {
					done <- err
					return
				}
			case <-stop:
				// 不把不足 100ms 的收尾片段当成 CPU 峰值，避免 ps 10ms 精度放大误差。
				if time.Since(previous[0].at) >= 100*time.Millisecond {
					if err := sample(); err != nil {
						done <- err
						return
					}
				}
				for i := range usages {
					points := append([]loadProcessPoint(nil), usages[i].Samples...)
					sort.Slice(points, func(a, b int) bool { return points[a].CPUPercent < points[b].CPUPercent })
					if len(points) > 0 {
						usages[i].P95CPUPercent = points[(len(points)-1)*95/100].CPUPercent
					}
				}
				done <- nil
				return
			}
		}
	}()
	var once sync.Once
	finish := func() []loadProcessUsage {
		once.Do(func() {
			close(stop)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
		return usages
	}
	t.Cleanup(func() { finish() })
	return finish
}

// TestLoadProcessCPUParsing 保护分钟、小时及跨日累计 CPU 时间正确换算，防止负载百分比单位出错。
func TestLoadProcessCPUParsing(t *testing.T) {
	for value, want := range map[string]float64{"0:00.01": 0.01, "12:34.50": 754.5, "1:02:03": 3723, "2-01:02:03": 176523} {
		got, err := parseProcessCPU(value)
		if err != nil || got != want {
			t.Fatalf("%s: %f %v want %f", value, got, err, want)
		}
	}
	if _, err := parseProcessCPU("bad"); err == nil {
		t.Fatal("invalid CPU time accepted")
	}
}
