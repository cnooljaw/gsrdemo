package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/mysqlclient"
	"github.com/cnooljaw/gsrdemo/common/model"
	"github.com/go-sql-driver/mysql"
)

func accessLiveDatabase(t *testing.T, node, database string) (*sql.DB, string) {
	t.Helper()
	var config struct {
		Node model.NodeConfig `yaml:",inline"`
		DSN  string           `yaml:"mysql_dsn"`
	}
	if err := model.ReadYAMLFile("../../configs/"+node+"-01.local.yaml", &config); err != nil {
		t.Fatal(err)
	}
	parsed, err := mysql.ParseDSN(config.DSN)
	if err != nil || parsed.DBName != database || parsed.Net != "tcp" || parsed.Addr != "127.0.0.1:3306" {
		t.Fatal("integration requires dedicated local database " + database)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := mysqlclient.Open(ctx, config.DSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, config.DSN
}

func freeAccessAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

// startAccessProcess 只回收本用例创建的子进程；输出文件归测试临时目录。
func startAccessProcess(t *testing.T, binary string, arguments []string, address string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "process.log")
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, arguments...)
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error("integration process shutdown timed out")
			}
			_ = output.Close()
		})
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case err := <-done:
			done <- err
			contents, _ := os.ReadFile(logPath)
			t.Fatalf("process exited: %v %s", err, contents)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("integration process listener not ready")
}

// TestAccessClientProcessesLiveMySQL 保护 TS 客户端通过五个独立进程完成认证、进入、即时命中和 MySQL 入账，错误密码不改变钱包。
func TestAccessClientProcessesLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_ACCESS_PROCESSES") != "1" {
		t.Skip("build all nodes/typeclient and set GSRDEMO_ACCESS_PROCESSES=1 for dedicated local databases")
	}
	loginDB, loginDSN := accessLiveDatabase(t, "login", "gsrdemo_login")
	settlementDB, settlementDSN := accessLiveDatabase(t, "settlement", "gsrdemo_settlement")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	suffix, err := connectionID()
	if err != nil {
		t.Fatal(err)
	}
	username, password := "access-e2e-"+suffix[:12], "demo-"+suffix
	hash := sha256.Sum256([]byte(password))
	result, err := loginDB.ExecContext(ctx, "INSERT INTO accounts (username, password_hash) VALUES (?, ?)", username, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	signedID, err := result.LastInsertId()
	if err != nil || signedID <= 0 {
		t.Fatalf("account ID: %v", err)
	}
	userID := uint64(signedID)
	t.Cleanup(func() {
		if _, err := loginDB.Exec("DELETE FROM accounts WHERE user_id = ? AND username = ?", userID, username); err != nil {
			t.Error(err)
		}
	})
	var count int
	if err := settlementDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM silver_balances WHERE user_id = ?", userID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("new account requires an unused test wallet, count=%d err=%v", count, err)
	}
	operationIDs := []string{fmt.Sprintf("wallet-init-%d", userID)}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		tx, err := settlementDB.BeginTx(cleanup, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		for _, id := range operationIDs {
			var batchID string
			err := tx.QueryRowContext(cleanup, "SELECT origin_batch FROM settlement_operations WHERE operation_id = ?", id).Scan(&batchID)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := tx.ExecContext(cleanup, "DELETE FROM settlement_operations WHERE operation_id = ?", id); err != nil {
				t.Error(err)
				return
			}
			if _, err := tx.ExecContext(cleanup, "DELETE FROM settlement_batches WHERE batch_id = ?", batchID); err != nil {
				t.Error(err)
				return
			}
		}
		if _, err := tx.ExecContext(cleanup, "DELETE FROM silver_balances WHERE user_id = ?", userID); err != nil {
			t.Error(err)
			return
		}
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	})
	directory := t.TempDir()
	routes := make(map[string]string)
	for _, node := range []string{"access", "login", "online", "mole", "settlement"} {
		routes[node] = freeAccessAddress(t)
	}
	gateAddress, redisAddress := freeAccessAddress(t), freeAccessAddress(t)
	nodes := "nodes:\n"
	for _, node := range []string{"access", "login", "online", "mole", "settlement"} {
		nodes += "  " + node + ": " + routes[node] + "\n"
	}
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte(nodes), 0600); err != nil {
		t.Fatal(err)
	}
	redisBinary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Fatal("redis-server required for explicit process integration")
	}
	_, redisPort, _ := net.SplitHostPort(redisAddress)
	startAccessProcess(t, redisBinary, []string{"--bind", "127.0.0.1", "--port", redisPort, "--save", "", "--appendonly", "yes", "--appendfsync", "always", "--dir", t.TempDir()}, redisAddress)
	for _, node := range []string{"login", "online", "settlement", "mole", "access"} {
		body := fmt.Sprintf("instance: %s-e2e\nlisten: %s\nnodes_file: nodes.yaml\nlog_directory: %s\n", node, node, strconv.Quote(filepath.Join(directory, "logs-"+node)))
		if node == "login" {
			body += "mysql_dsn: " + strconv.Quote(loginDSN) + "\n"
		}
		if node == "settlement" {
			body += "mysql_dsn: " + strconv.Quote(settlementDSN) + "\n"
		}
		if node == "mole" {
			body += "redis_url: redis://" + redisAddress + "/0\nredis_key: gsrdemo:test:access\n"
		}
		if node == "access" {
			body += fmt.Sprintf("gate:\n  address: %s\n  max_connections: 8\n  max_frame_bytes: 4096\n  max_outbound_queue_bytes: 65536\n", gateAddress)
		}
		path := filepath.Join(directory, node+".yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		binary, err := filepath.Abs("../../bin/" + node)
		if err != nil {
			t.Fatal(err)
		}
		if node == "settlement" {
			command := exec.CommandContext(ctx, binary, "-config", path, "-init-wallet", strconv.FormatUint(userID, 10), "-silver", "10000")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("wallet initialization: %v %s", err, output)
			}
		}
		if node == "mole" {
			command := exec.CommandContext(ctx, binary, "-config", path, "-init-accounting")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("accounting initialization: %v %s", err, output)
			}
		}
		address := routes[node]
		if node == "access" {
			address = gateAddress
		}
		startAccessProcess(t, binary, []string{"-config", path}, address)
	}
	clientPath, err := filepath.Abs("../../typeclient/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	client := func(password string, hits string) ([]byte, error) {
		command := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+gateAddress+"/ws", "--username", username, "--hits", hits)
		command.Stdin = strings.NewReader(password + "\n")
		return command.CombinedOutput()
	}
	if output, err := client("wrong", "1"); err == nil || bytes.Contains(output, []byte(`"event":"enter"`)) {
		t.Fatalf("wrong credentials accepted: %v %s", err, output)
	}
	output, clientErr := client(password, "3")
	hitCount, receiptCount := 0, 0
	var debit, credit int64
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		var event struct {
			Event string          `json:"event"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		switch event.Event {
		case "hit":
			var value struct {
				Hit struct {
					ID     string `json:"hitId"`
					Debit  string `json:"debit"`
					Credit string `json:"credit"`
				} `json:"hit"`
			}
			if err := json.Unmarshal(event.Value, &value); err != nil {
				t.Fatal(err)
			}
			operationIDs = append(operationIDs, value.Hit.ID)
			debitValue, err := strconv.ParseInt(value.Hit.Debit, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			creditValue, err := strconv.ParseInt(value.Hit.Credit, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			debit += debitValue
			credit += creditValue
			hitCount++
		case "settlement":
			var value struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(event.Value, &value); err != nil || value.Status != "committed" {
				t.Fatalf("receipt: %s %v", event.Value, err)
			}
			receiptCount++
		}
	}
	if clientErr != nil {
		t.Fatalf("TS client: %v %s", clientErr, output)
	}
	if hitCount != 3 || receiptCount != 3 {
		t.Fatalf("incomplete flow: %s", output)
	}
	var amount int64
	var version uint64
	if err := settlementDB.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", userID).Scan(&amount, &version); err != nil {
		t.Fatal(err)
	}
	if amount != 10000-debit+credit || version != 4 {
		t.Fatalf("wallet amount=%d version=%d debit=%d credit=%d", amount, version, debit, credit)
	}
	t.Logf("五节点 + TS 客户端：3 次命中 / 3 次入账，扣 %d 返 %d，余额 %d，版本 %d", debit, credit, amount, version)
}
