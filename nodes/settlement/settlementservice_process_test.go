package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/cluster"
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
			return stop
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
	return stop
}

// TestSettlementTwoNodesShareWalletLiveMySQL 保护两个独立结算进程共用权威账本，跨节点重提原操作只扣款一次。
func TestSettlementTwoNodesShareWalletLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_SETTLEMENT_PROCESSES") != "1" {
		t.Skip("build settlement and set GSRDEMO_SETTLEMENT_PROCESSES=1")
	}
	store, config := liveSettlementStore(t)
	sample := newLiveSettlementSample(t, store, 1)
	sample.Business = fmt.Sprintf("two-nodes-%d", sample.Users[0])
	directory := t.TempDir()
	routes := map[cluster.NodeID]string{
		"settlement-01": freeBusinessAddress(t),
		"settlement-02": freeBusinessAddress(t),
	}
	nodes := fmt.Sprintf("nodes:\n  settlement-01: %s\n  settlement-02: %s\n", routes["settlement-01"], routes["settlement-02"])
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte(nodes), 0600); err != nil {
		t.Fatal(err)
	}
	for _, node := range []cluster.NodeID{"settlement-01", "settlement-02"} {
		body := fmt.Sprintf("instance: %s\nlisten: %s\nnodes_file: nodes.yaml\nlog_directory: %s\nmysql_dsn: %s\n",
			node, node, strconv.Quote(filepath.Join(directory, string(node)+"-logs")), strconv.Quote(config.MySQLDSN))
		path := filepath.Join(directory, string(node)+".yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		stop := startBusinessProcess(t, "settlement", path, routes[node])
		defer stop()
	}
	id := sample.Business + "-two-nodes"
	sample.Operations = append(sample.Operations, id)
	sample.Batches = append(sample.Batches, derivedBatchID("single", id))
	request := testSingle(id, sample.Users[0], 100, 0)
	request["version"], request["business"] = uint64(2), sample.Business
	var original any
	for _, node := range []string{"settlement-01", "settlement-02"} {
		value, err := businessRPC(t, routes, node, settlementWriteName, settleSingleCommand, request)
		if err != nil {
			t.Fatal(err)
		}
		if original == nil {
			original = value
		} else if !reflect.DeepEqual(value, original) {
			t.Fatalf("跨节点重复提交改变历史回执: %v %v", original, value)
		}
		value, err = businessRPC(t, routes, node, settlementReadName, getBalanceCommand, sample.Users[0])
		if err != nil || value.(map[string]any)["amount"] != int64(900) || value.(map[string]any)["version"] != uint64(2) {
			t.Fatalf("%s 重复扣款或未共享余额: %v %v", node, value, err)
		}
	}
}
