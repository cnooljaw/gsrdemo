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

// TestAccessClientProcessesLiveMySQL 保护单客户端通过五个独立进程完成认证、即时命中和真实入账。
func TestAccessClientProcessesLiveMySQL(t *testing.T) {
	runAccessClientsLiveMySQL(t, 1)
}

// TestAccessThreeClientsLiveMySQL 保护三个不同账号并发进入同组、共享目标且各完成三次入账，余额/版本与逐锤结果一致。
func TestAccessThreeClientsLiveMySQL(t *testing.T) {
	runAccessClientsLiveMySQL(t, 3)
}

type accessDemoPlayer struct {
	id                 uint64
	username, password string
	command            *exec.Cmd
	output             bytes.Buffer
}

func runAccessClientsLiveMySQL(t *testing.T, playerCount int) {
	t.Helper()
	if os.Getenv("GSRDEMO_ACCESS_PROCESSES") != "1" {
		t.Skip("build all nodes/typeclient and set GSRDEMO_ACCESS_PROCESSES=1 for dedicated local databases")
	}
	loginDB, loginDSN := accessLiveDatabase(t, "login", "gsrdemo_login")
	settlementDB, settlementDSN := accessLiveDatabase(t, "settlement", "gsrdemo_settlement")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	players := make([]accessDemoPlayer, playerCount)
	operationIDs := []string{}
	for index := range players {
		player := &players[index]
		suffix, err := connectionID()
		if err != nil {
			t.Fatal(err)
		}
		player.username, player.password = "access-e2e-"+suffix[:12], "demo-"+suffix
		hash := sha256.Sum256([]byte(player.password))
		result, err := loginDB.ExecContext(ctx, "INSERT INTO accounts (username, password_hash) VALUES (?, ?)", player.username, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		signedID, err := result.LastInsertId()
		if err != nil || signedID <= 0 {
			t.Fatalf("account ID: %v", err)
		}
		player.id = uint64(signedID)
		t.Cleanup(func() {
			if _, err := loginDB.Exec("DELETE FROM accounts WHERE user_id = ? AND username = ?", player.id, player.username); err != nil {
				t.Error(err)
			}
		})
		var count int
		if err := settlementDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM silver_balances WHERE user_id = ?", player.id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("new account requires an unused test wallet, count=%d err=%v", count, err)
		}
		operationIDs = append(operationIDs, fmt.Sprintf("wallet-init-%d", player.id))
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		tx, err := settlementDB.BeginTx(cleanup, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback()
		batches := map[string]bool{}
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
			batches[batchID] = true
			if _, err := tx.ExecContext(cleanup, "DELETE FROM settlement_operations WHERE operation_id = ?", id); err != nil {
				t.Error(err)
				return
			}
		}
		for id := range batches {
			if _, err := tx.ExecContext(cleanup, "DELETE FROM settlement_batches WHERE batch_id = ?", id); err != nil {
				t.Error(err)
				return
			}
		}
		for _, player := range players {
			if _, err := tx.ExecContext(cleanup, "DELETE FROM silver_balances WHERE user_id = ?", player.id); err != nil {
				t.Error(err)
				return
			}
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
			for _, player := range players {
				command := exec.CommandContext(ctx, binary, "-config", path, "-init-wallet", strconv.FormatUint(player.id, 10), "-silver", "10000")
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("wallet initialization: %v %s", err, output)
				}
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
	wrong := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+gateAddress+"/ws", "--username", players[0].username, "--hits", "1")
	wrong.Stdin = strings.NewReader("wrong\n")
	if output, err := wrong.CombinedOutput(); err == nil || bytes.Contains(output, []byte(`"event":"enter"`)) {
		t.Fatalf("wrong credentials accepted: %v %s", err, output)
	}
	// 全部 Start 后再 Wait，确保是并发客户端，而不是逐个跑完的单客户端样本。
	for index := range players {
		player := &players[index]
		player.command = exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+gateAddress+"/ws", "--username", player.username, "--hits", "3")
		player.command.Stdin = strings.NewReader(player.password + "\n")
		player.command.Stdout, player.command.Stderr = &player.output, &player.output
		if err := player.command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// 后续客户端启动失败时，也回收此前已启动但尚未 Wait 的进程。
			if player.command.ProcessState == nil {
				_ = player.command.Process.Kill()
				_ = player.command.Wait()
			}
		})
	}
	// 先等待所有客户端退出，再解析/断言，避免失败时尚有客户端在写账务。
	clientErrors := make([]error, len(players))
	for index := range players {
		clientErrors[index] = players[index].command.Wait()
	}
	group := ""
	sawThreeMembers, totalMisses := false, 0
	seenHits := map[string]bool{}
	for index := range players {
		player := &players[index]
		hitCount, receiptCount := 0, 0
		var debit, credit int64
		playerGroup := ""
		for _, line := range bytes.Split(player.output.Bytes(), []byte{'\n'}) {
			var event struct {
				Event string          `json:"event"`
				Value json.RawMessage `json:"value"`
			}
			if json.Unmarshal(line, &event) != nil {
				continue
			}
			switch event.Event {
			case "enter":
				var value struct {
					Snapshot struct {
						AttackName string            `json:"attackName"`
						Members    []json.RawMessage `json:"members"`
					} `json:"snapshot"`
				}
				if err := json.Unmarshal(event.Value, &value); err != nil {
					t.Fatal(err)
				}
				playerGroup = value.Snapshot.AttackName
				sawThreeMembers = sawThreeMembers || len(value.Snapshot.Members) == 3
			case "snapshot":
				var value struct {
					Members []json.RawMessage `json:"members"`
				}
				if err := json.Unmarshal(event.Value, &value); err != nil {
					t.Fatal(err)
				}
				sawThreeMembers = sawThreeMembers || len(value.Members) == 3
			case "miss":
				totalMisses++
			case "hit":
				var value struct {
					Hit struct {
						ID     string `json:"hitId"`
						Status string `json:"status"`
						Debit  string `json:"debit"`
						Credit string `json:"credit"`
					} `json:"hit"`
				}
				if err := json.Unmarshal(event.Value, &value); err != nil {
					t.Fatal(err)
				}
				if value.Hit.ID == "" || seenHits[value.Hit.ID] || value.Hit.Status != "accepted" {
					t.Fatalf("invalid/duplicate hit: %s", event.Value)
				}
				seenHits[value.Hit.ID] = true
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
		if clientErrors[index] != nil {
			t.Errorf("client %d: %v %s", index+1, clientErrors[index], player.output.Bytes())
		}
		if hitCount != 3 || receiptCount != 3 {
			t.Errorf("client %d incomplete: hits=%d receipts=%d %s", index+1, hitCount, receiptCount, player.output.Bytes())
		}
		if playerGroup == "" || (group != "" && group != playerGroup) {
			t.Errorf("clients did not share a group: %q %q", group, playerGroup)
		}
		group = playerGroup
		var amount int64
		var version uint64
		if err := settlementDB.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).Scan(&amount, &version); err != nil {
			t.Fatal(err)
		}
		if amount != 10000-debit+credit || version != 4 {
			t.Errorf("client %d wallet amount=%d version=%d debit=%d credit=%d", index+1, amount, version, debit, credit)
		}
		t.Logf("客户端 %d：%d 命中 / %d 入账，扣 %d 返 %d，余额 %d，版本 %d", index+1, hitCount, receiptCount, debit, credit, amount, version)
	}
	if playerCount == 3 && !sawThreeMembers {
		t.Error("no snapshot showed all three members")
	}
	t.Logf("%d 个并发 CLI，共 %d 个唯一 HitID、%d 次目标失效后继续", playerCount, len(seenHits), totalMisses)
}
