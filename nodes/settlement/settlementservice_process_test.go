package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/redisclient"
)

func freeBusinessAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func startBusinessProcess(t *testing.T, node, config, address string) func() {
	t.Helper()
	_, stop := startBusinessProcessPID(t, node, config, address)
	return stop
}

func startBusinessProcessPID(t *testing.T, node, config, address string) (int, func()) {
	t.Helper()
	binary, err := filepath.Abs("../../bin/" + node)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("build %s before process integration: %v", node, err)
	}
	output, err := os.OpenFile(filepath.Join(t.TempDir(), node+".log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-config", config)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		output.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s exit: %v", node, err)
				}
			case <-time.After(5 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Errorf("%s stop timed out", node)
			}
			_ = output.Close()
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			return command.Process.Pid, stop
		}
		select {
		case err := <-done:
			done <- err
			contents, _ := os.ReadFile(output.Name())
			t.Fatalf("%s startup: %v %s", node, err, contents)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s listener not ready", node)
	return command.Process.Pid, stop
}

// TestSettlementMoleProcessesLiveMySQL 保护三个独立 OS 节点通过真实 TCP、Redis 检查点和 MySQL 跑完命中到持久入账。
func TestSettlementMoleProcessesLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_PROCESSES") != "1" {
		t.Skip("build online/mole/settlement and set GSRDEMO_SETTLEMENT_PROCESSES=1")
	}
	store, config := liveSettlementStore(t)
	sample := newLiveSettlementSample(t, store, 1)
	userID := sample.Users[0]
	directory := t.TempDir()
	redisAddress := startBusinessRedis(t)
	routes := map[cluster.NodeID]string{"settlement": freeBusinessAddress(t), "online": freeBusinessAddress(t), "mole": freeBusinessAddress(t)}
	nodes := fmt.Sprintf("nodes:\n  settlement: %s\n  online: %s\n  mole: %s\n", routes["settlement"], routes["online"], routes["mole"])
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte(nodes), 0600); err != nil {
		t.Fatal(err)
	}
	stops := []func(){}
	for _, node := range []string{"settlement", "online", "mole"} {
		body := fmt.Sprintf("instance: %s-live\nlisten: %s\nnodes_file: nodes.yaml\nlog_directory: %s\n", node, node, strconv.Quote(filepath.Join(directory, "logs-"+node)))
		if node == "settlement" {
			body += "mysql_dsn: " + strconv.Quote(config.MySQLDSN) + "\n"
		}
		if node == "mole" {
			body += "redis_url: redis://" + redisAddress + "/0\nredis_key: gsrdemo:test:mole\n"
		}
		path := filepath.Join(directory, node+".yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if node == "mole" {
			binary, err := filepath.Abs("../../bin/mole")
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(binary, "-config", path, "-init-accounting")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("initialize mole checkpoint: %v %s", err, output)
			}
		}
		stops = append(stops, startBusinessProcess(t, node, path, routes[cluster.NodeID(node)]))
	}
	if _, err := businessRPC(t, routes, "online", "online", 1, userID, "login-live", "access-01", "agent-live"); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := businessRPC(t, routes, "mole", "mole", 1, userID, "login-live", "access-01", "agent-live")
		if err == nil {
			address := value.(map[string]any)["entry_name"].(string)
			value, err = businessRPC(t, routes, "mole", address, 1, userID, "login-live", "access-01", "agent-live")
			if err == nil {
				snapshot = value.(map[string]any)
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snapshot == nil {
		t.Fatal("mole entry did not initialize")
	}
	target := snapshot["targets"].([]any)[0].(map[string]any)
	value, err := businessRPC(t, routes, "mole", snapshot["attack_name"].(string), 2, userID, "login-live", "action-live", target["spawn_seq"], target["hole"])
	if err != nil {
		t.Fatal(err)
	}
	hit := value.(map[string]any)
	if hit["status"] != "accepted" {
		t.Fatal("hit did not return memory result")
	}
	id := hit["hit_id"].(string)
	sample.Operations = append(sample.Operations, id)
	// 即使后续断言失败，也定位本次独占 HitID 对应的归档批次进行清理。
	t.Cleanup(func() {
		for index := len(stops) - 1; index >= 0; index-- {
			stops[index]()
		}
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		defer cancel()
		var batchID string
		if err := store.db.QueryRowContext(ctx, "SELECT origin_batch FROM settlement_operations WHERE operation_id = ?", id).Scan(&batchID); err == nil {
			sample.Batches = append(sample.Batches, batchID)
		}
	})
	deadline = time.Now().Add(5 * time.Second)
	committed := false
	for time.Now().Before(deadline) {
		result, err := settlementRPC(t, routes["settlement"], getOperationCommand, userID, id)
		if err == nil && result != nil {
			if result.(map[string]any)["status"] != "committed" {
				t.Fatal("financial operation rejected")
			}
			committed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !committed {
		t.Fatal("hit did not persist")
	}
	balance := liveCall(t, routes["settlement"], getBalanceCommand, userID)
	if balance["amount"] != hit["balance"] {
		t.Fatalf("memory and persistent balances differ: %v %v", hit, balance)
	}
	client, err := redisclient.New(redisAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deadline = time.Now().Add(3 * time.Second)
	verified := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		key := "gsrdemo:test:mole"
		if lane := userID % 4; lane != 0 {
			key = fmt.Sprintf("%s:lane:%d", key, lane)
		}
		encoded, err := client.Get(ctx, key).Bytes()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var checkpoint struct {
			Pool     struct{ Hits, Debit, Credit int64 }
			Sequence uint64
			Pending  json.RawMessage
		}
		if err := json.Unmarshal(encoded, &checkpoint); err != nil {
			t.Fatal(err)
		}
		if checkpoint.Sequence == 1 && checkpoint.Pool.Hits == 1 && len(checkpoint.Pending) == 0 {
			if checkpoint.Pool.Debit != hit["debit"] || checkpoint.Pool.Credit != hit["credit"] {
				t.Fatal("Redis statistics differ from confirmed hit")
			}
			verified = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !verified {
		t.Fatal("Redis checkpoint was not finalized")
	}
	t.Log("独立 online/mole/settlement 进程已完成：绑定 → 命中即时结果 → MySQL 入账 → Redis 统计及检查点确认")
}

func startBusinessRedis(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required for process integration")
	}
	address := freeBusinessAddress(t)
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "yes", "--appendfsync", "always", "--dir", t.TempDir())
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return address
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("temporary Redis did not listen")
	return ""
}
