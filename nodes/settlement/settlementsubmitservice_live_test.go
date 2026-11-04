package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/cluster"
)

// TestSettlementAsyncDuplicateLiveMySQL 保护不同回执地址并发重提同一批次时只入账一次，SQL 冲突后原身份重试仍返回一致终态。
func TestSettlementAsyncDuplicateLiveMySQL(t *testing.T) {
	store, _ := liveSettlementStore(t)
	store.db.SetMaxOpenConns(10)
	sample := newLiveSettlementSample(t, store, 2)
	id := "async-duplicate-" + rand.Text()
	ops := []map[string]any{testSingle(id+"-a", sample.Users[0], 10, 0), testSingle(id+"-b", sample.Users[1], 20, 0)}
	request := testBatch(id, ops...)
	sample.Operations = append(sample.Operations, id+"-a", id+"-b")
	sample.Batches = append(sample.Batches, id)
	results := make(chan asyncLoadResult, 8)
	callers := []*asyncLoadCaller{}
	for i := range 8 {
		callers = append(callers, &asyncLoadCaller{name: cluster.ServiceName(fmt.Sprintf("duplicate-%d", i)), jobs: []asyncLoadJob{{id, request}}, results: results})
	}
	stop := asyncLoadNodes(t, store.db, 8, 64, callers)
	defer stop()
	for range 8 {
		select {
		case result := <-results:
			if result.err != nil || result.receipt["batch_id"] != id {
				t.Fatalf("原批次未恢复: %v %v", result.receipt, result.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("重复提交未返回原回执")
		}
	}
	for i, uid := range sample.Users {
		ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
		fields, err := store.getBalance(ctx, uid)
		cancel()
		if err != nil || fields["amount"] != int64(1000-(i+1)*10) || fields["version"] != uint64(2) {
			t.Fatalf("重复入账: %v %v", fields, err)
		}
	}
	if count := asyncCountRows(t, store.db, "settlement_operations", "operation_id", []string{id + "-a", id + "-b"}); count != 2 {
		t.Fatalf("重复操作数量: %d", count)
	}
}
