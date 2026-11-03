package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

func startSettlementTestNodes(t *testing.T, db *sql.DB, count int) []string {
	t.Helper()
	addresses := make([]string, 0, count)
	for range count {
		address, _ := startSettlementTestNode(t, db)
		addresses = append(addresses, address)
	}
	return addresses
}

func retryLiveSettlement(t *testing.T, address, name string, command runtime.CommandID, request map[string]any) (map[string]any, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		value, err := businessRPC(t, map[cluster.NodeID]string{"settlement": address}, "settlement", name, command, request)
		if err == nil {
			return value.(map[string]any), nil
		}
		if !errors.Is(err, errSettlementUnavailable) && !errors.Is(err, runtime.ErrCallTimeout) && !errors.Is(err, runtime.ErrMailboxFull) && !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("original identity did not finish: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func liveGroupRequest(id, business string, from, to uint64, debit int64) map[string]any {
	return map[string]any{"version": uint64(2), "operation_id": id, "business": business, "business_ref": id, "detail": map[string]any{}, "changes": []any{
		map[string]any{"user_id": from, "reason": "game_win_loss", "debit": debit, "credit": int64(0)},
		map[string]any{"user_id": to, "reason": "game_win_loss", "debit": int64(0), "credit": debit},
	}}
}

// TestSettlementHotWalletLiveMySQL 保护真实持锁钱包快速失败、其他钱包继续入账，原多人操作解锁重试后只生效一次。
func TestSettlementHotWalletLiveMySQL(t *testing.T) {
	store, _ := liveSettlementStore(t)
	sample := newLiveSettlementSample(t, store, 3)
	sample.Business = "isolation-" + rand.Text()
	users := sample.Users
	addresses := startSettlementTestNodes(t, store.db, 4)
	address := addresses[0]
	var version, binlog string
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	if err := store.db.QueryRowContext(ctx, "SELECT VERSION(), @@binlog_format").Scan(&version, &binlog); err != nil {
		t.Fatal(err)
	}
	t.Logf("MySQL %s, binlog_format=%s", version, binlog)
	lock, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var amount int64
	if err := lock.QueryRowContext(ctx, "SELECT amount FROM silver_balances WHERE user_id = ? FOR UPDATE", users[0]).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	for index, peerAddress := range addresses {
		id := fmt.Sprintf("%s-locked-%d", sample.Business, index)
		request := liveGroupRequest(id, sample.Business, users[0], users[2], 100)
		sample.Operations = append(sample.Operations, id)
		sample.Batches = append(sample.Batches, derivedBatchID("group", id))
		begin := time.Now()
		if _, err := businessRPC(t, map[cluster.NodeID]string{"settlement": peerAddress}, "settlement", settlementWriteName, settleGroupCommand, request); !errors.Is(err, errSettlementUnavailable) {
			t.Fatalf("locked group claimed a terminal result: %v", err)
		}
		if elapsed := time.Since(begin); elapsed >= 500*time.Millisecond {
			t.Fatalf("locked wallet occupied %s: %v", peerAddress, elapsed)
		}
	}
	// 持锁期间向每个写入口提交另一个钱包，不能只证明读查询仍可用。
	for index, peerAddress := range addresses {
		id := fmt.Sprintf("%s-healthy-%d", sample.Business, index)
		request := testSingle(id, users[1], 10, 0)
		request["version"], request["business"] = uint64(2), sample.Business
		sample.Operations = append(sample.Operations, id)
		sample.Batches = append(sample.Batches, derivedBatchID("single", id))
		value, err := businessRPC(t, map[cluster.NodeID]string{"settlement": peerAddress}, "settlement", settlementWriteName, settleSingleCommand, request)
		if err != nil || value.(map[string]any)["results"].([]any)[0].(map[string]any)["status"] != "committed" {
			t.Fatalf("unrelated wallet blocked: %v %v", value, err)
		}
	}
	for _, peerAddress := range addresses {
		value, err := businessRPC(t, map[cluster.NodeID]string{"settlement": peerAddress}, "settlement", settlementReadName, getBalanceCommand, users[2])
		if err != nil || value.(map[string]any)["amount"] != int64(1000) {
			t.Fatalf("locked group partly applied: %v %v", value, err)
		}
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	id := sample.Business + "-locked-0"
	request := liveGroupRequest(id, sample.Business, users[0], users[2], 100)
	for _, peerAddress := range addresses {
		value, err := retryLiveSettlement(t, peerAddress, settlementWriteName, settleGroupCommand, request)
		if err != nil || value["results"].([]any)[0].(map[string]any)["status"] != "committed" {
			t.Fatalf("original group did not recover: %v %v", value, err)
		}
	}
	for index, want := range []int64{900, 960, 1100} {
		value := liveCall(t, address, getBalanceCommand, users[index])
		if value["amount"] != want || value["version"] != []uint64{2, 5, 2}[index] {
			t.Fatalf("wallet %d mismatch: %v", index, value)
		}
	}
}

// TestSettlementConcurrentIdentitiesLiveMySQL 保护跨结算节点的重复身份不多扣，独立同钱包操作不透支、多人输赢整体原子。
func TestSettlementConcurrentIdentitiesLiveMySQL(t *testing.T) {
	for _, kind := range []string{"duplicate", "debit", "group"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := liveSettlementStore(t)
			sample := newLiveSettlementSample(t, store, 3)
			sample.Business = "isolation-" + rand.Text()
			addresses := startSettlementTestNodes(t, store.db, 4)
			address := addresses[0]
			type job struct {
				address string
				command runtime.CommandID
				request map[string]any
			}
			var jobs []job
			for index := range 16 {
				id := fmt.Sprintf("%s-%s-%d", sample.Business, kind, index)
				if kind == "duplicate" {
					id = sample.Business + "-same"
				}
				request := testSingle(id, sample.Users[0], 100, 0)
				request["version"], request["business"] = uint64(2), sample.Business
				command, prefix := settleSingleCommand, "single"
				if kind == "group" {
					command, prefix = settleGroupCommand, "group"
					request = liveGroupRequest(id, sample.Business, sample.Users[0], sample.Users[1+index%2], 100)
				}
				jobs = append(jobs, job{addresses[index%len(addresses)], command, request})
				if kind != "duplicate" || index == 0 {
					sample.Operations = append(sample.Operations, id)
					sample.Batches = append(sample.Batches, derivedBatchID(prefix, id))
				}
			}
			type outcome struct {
				receipt map[string]any
				err     error
			}
			done := make(chan outcome, len(jobs))
			var workers sync.WaitGroup
			workers.Add(len(jobs))
			defer workers.Wait()
			for _, item := range jobs {
				go func() {
					defer workers.Done()
					value, err := retryLiveSettlement(t, item.address, settlementWriteName, item.command, item.request)
					done <- outcome{value, err}
				}()
			}
			committed, rejected := 0, 0
			for range jobs {
				select {
				case result := <-done:
					if result.err != nil {
						t.Fatal(result.err)
					}
					switch result.receipt["results"].([]any)[0].(map[string]any)["status"] {
					case "committed":
						committed++
					case "rejected":
						rejected++
					default:
						t.Fatal("unknown financial result")
					}
				case <-time.After(8 * time.Second):
					t.Fatal("concurrent settlement did not finish")
				}
			}
			wantAmount, wantVersion := int64(0), uint64(11)
			if kind == "duplicate" {
				wantAmount, wantVersion = 900, 2
			} else if committed != 10 || rejected != 6 {
				t.Fatalf("overdraft or missing operation: committed=%d rejected=%d", committed, rejected)
			}
			value := liveCall(t, address, getBalanceCommand, sample.Users[0])
			if value["amount"] != wantAmount || value["version"] != wantVersion {
				t.Fatalf("concurrent wallet mismatch: %v", value)
			}
			var total int64
			for _, userID := range sample.Users {
				total += liveCall(t, address, getBalanceCommand, userID)["amount"].(int64)
			}
			wantTotal := int64(2000)
			if kind == "duplicate" {
				wantTotal = 2900
			}
			if kind == "group" {
				wantTotal = 3000
			}
			if total != wantTotal {
				t.Fatalf("multi-wallet conservation failed: %d != %d", total, wantTotal)
			}
			ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
			defer cancel()
			var count int
			if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE business = ?", sample.Business).Scan(&count); err != nil || count != len(sample.Operations)-len(sample.Users) {
				t.Fatalf("idempotency record count=%d: %v", count, err)
			}
			t.Logf("%s: %d completed replies, %d rejected replies; unique operations=%d", kind, committed, rejected, count)
		})
	}
}

// TestSettlementConflictingConcurrentIdentityLiveMySQL 保护同 ID 不同内容并发时最多一个请求生效，另一方重试得到冲突。
func TestSettlementConflictingConcurrentIdentityLiveMySQL(t *testing.T) {
	store, _ := liveSettlementStore(t)
	sample := newLiveSettlementSample(t, store, 1)
	sample.Business = "conflict-" + rand.Text()
	addresses := startSettlementTestNodes(t, store.db, 2)
	address := addresses[0]
	id := "conflict-" + rand.Text()
	sample.Operations = append(sample.Operations, id)
	sample.Batches = append(sample.Batches, derivedBatchID("single", id))
	done := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	defer workers.Wait()
	for index, debit := range []int64{100, 200} {
		request := testSingle(id, sample.Users[0], debit, 0)
		request["version"], request["business"] = uint64(2), sample.Business
		go func() {
			defer workers.Done()
			_, err := retryLiveSettlement(t, addresses[index], settlementWriteName, settleSingleCommand, request)
			done <- err
		}()
	}
	successes, conflicts := 0, 0
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				successes++
			} else if errors.Is(err, errSettlementConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("conflict did not resolve")
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
	value := liveCall(t, address, getBalanceCommand, sample.Users[0])
	if (value["amount"] != int64(900) && value["amount"] != int64(800)) || value["version"] != uint64(2) {
		t.Fatalf("conflict applied twice: %v", value)
	}
}
