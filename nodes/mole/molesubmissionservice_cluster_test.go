package main

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

// TestMolePoisonBatchPausesSubmission 保护永久错误保留原批次并停止新记账，不持续冲击结算节点。
func TestMolePoisonBatchPausesSubmission(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.rejectUser = 1
	routes, _ := prepareMoleCluster(t, fixture)
	first := enterMoleTestUser(t, routes, 1, "login-1", "access-01")
	_ = enterMoleTestUser(t, routes, 2, "login-2", "access-01")
	_ = enterMoleTestUser(t, routes, 3, "login-3", "access-01")
	other := enterMoleTestUser(t, routes, 4, "login-4", "access-02")
	bad := testHitTarget(t, routes, first, 1, "login-1", "bad")
	deadline := time.Now().Add(2 * time.Second)
	for {
		value, err := moleRPC(t, routes, "mole", moleSubmissionName, submissionStateCommand)
		if err == nil && value.(map[string]any)["blocked"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("submission did not pause: %v %v", value, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(4)); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("提交暂停仍接纳新记账: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", other["attack_name"].(string), attackSnapshotCommand, uint64(4), "login-4"); err != nil {
		t.Fatal("提交暂停阻塞了房间快照", err)
	}
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingStatusCommand, uint64(1), bad["hit_id"])
	if err != nil || value.(map[string]any)["status"] != "accepted" {
		t.Fatalf("unknown result was discarded: %v %v", value, err)
	}
	state, err := fixture.checkpoint.load(context.Background())
	if err != nil || state.Pending == nil || state.Pending.Events[0].HitID != bad["hit_id"] {
		t.Fatalf("poison batch lost: %+v %v", state, err)
	}
	// 停止重试永久错误，不用持续请求冲击结算节点。
	requests := fixture.submitCount.Load()
	time.Sleep(3 * accountingPeriod)
	if fixture.submitCount.Load() != requests {
		t.Fatal("permanent conflict kept retrying")
	}
}

// TestMoleSubmissionPanicPausesAccounting 保护唯一提交 Service 退出后明确暂停记账，房间快照仍可查询。
func TestMoleSubmissionPanicPausesAccounting(t *testing.T) {
	fixture := newSettlementFixture()
	routes, _ := prepareMoleCluster(t, fixture)
	first := enterMoleTestUser(t, routes, 1, "login-1", "access-01")
	_ = enterMoleTestUser(t, routes, 2, "login-2", "access-01")
	_ = enterMoleTestUser(t, routes, 3, "login-3", "access-01")
	other := enterMoleTestUser(t, routes, 4, "login-4", "access-02")
	var enabled atomic.Bool
	fixture.checkpoint.client.AddHook(moleRedisFaultHook{panicKey: fixture.checkpoint.key, panicEnabled: &enabled})
	enabled.Store(true)
	_ = testHitTarget(t, routes, first, 1, "login-1", "panic")
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
		if errors.Is(err, errAccountingUnavailable) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dead submission did not pause accounting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(4)); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("提交退出后仍显示账务可用: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", other["attack_name"].(string), attackSnapshotCommand, uint64(4), "login-4"); err != nil {
		t.Fatal(err)
	}
}

// TestMoleDamagedCheckpointBlocksAccounting 保护损坏检查点不被当作零统计，单提交链路明确拒绝进入且不覆盖损坏内容。
func TestMoleDamagedCheckpointBlocksAccounting(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.checkpoint = testMoleCheckpoint(t)
	broken := fixture.checkpoint
	if err := broken.client.Set(context.Background(), broken.key, "broken", 0).Err(); err != nil {
		t.Fatal(err)
	}
	routes, _ := prepareMoleCluster(t, fixture)
	deadline := time.Now().Add(2 * time.Second)
	for {
		value, err := moleRPC(t, routes, "mole", moleSubmissionName, submissionStateCommand)
		if err == nil && value.(map[string]any)["blocked"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("检查点损坏未显示暂停: %v %v", value, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("损坏检查点被当作可用统计: %v", err)
	}
	if _, err := moleRPC(t, routes, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uint64(4), "login-4", "access-02", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMoleEnter(t, routes, uint64(4), "login-4", "access-02", "agent"); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("损坏检查点未明确拒绝进入: %v", err)
	}
	if raw, err := broken.client.Get(context.Background(), broken.key).Result(); err != nil || raw != "broken" {
		t.Fatal("损坏状态被覆盖")
	}
}

// TestMoleSettlementCallTimeoutAllowsHitsAndRetriesOriginalBatch 保护 Call 等待和超时期间其他组仍返回内存余额，重试保持原批次且只入账一次。
func TestMoleSettlementCallTimeoutAllowsHitsAndRetriesOriginalBatch(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.firstReplyDelay = 3100 * time.Millisecond
	routes, _ := prepareMoleCluster(t, fixture)
	first := enterMoleTestUser(t, routes, 1, "login-1", "access-01")
	_ = enterMoleTestUser(t, routes, 2, "login-2", "access-01")
	_ = enterMoleTestUser(t, routes, 3, "login-3", "access-02")
	other := enterMoleTestUser(t, routes, 4, "login-4", "access-02")
	firstHit := testHitTarget(t, routes, first, 1, "login-1", "first")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("first batch did not reach settlement")
	}
	secondHit := testHitTarget(t, routes, other, 4, "login-4", "second")
	status, err := moleRPC(t, routes, "mole", moleAccountingName, accountingStatusCommand, uint64(1), firstHit["hit_id"])
	if err != nil || status.(map[string]any)["status"] != "accepted" {
		t.Fatalf("第二组命中未在首批回执缺失期间完成: %v %v", status, err)
	}
	for _, hit := range []map[string]any{firstHit, secondHit} {
		want := int64(10000) - hit["debit"].(int64) + hit["credit"].(int64)
		if hit["status"] != "accepted" || hit["balance"] != want || hit["revision"] == nil {
			t.Fatalf("hit did not return a complete memory result: %v", hit)
		}
	}
	waitMoleOperation(t, routes, 1, firstHit["hit_id"].(string), "committed")
	waitMoleOperation(t, routes, 4, secondHit["hit_id"].(string), "committed")
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand)
	if err != nil || value.(map[string]any)["hits"] != int64(2) {
		t.Fatalf("duplicate or missing statistics: %v %v", value, err)
	}
	// 第一批的观测已被消费；这里只应剩第二批的一次实际入账。
	if len(fixture.submitted) != 1 {
		t.Fatal("lost reply caused another financial application")
	}
	if len(fixture.batchQueries) != 0 || len(fixture.submitRequests) < 2 {
		t.Fatal("expected direct idempotent submission and retry, without GetBatch")
	}
	firstRequest, retry := <-fixture.submitRequests, <-fixture.submitRequests
	if !reflect.DeepEqual(firstRequest, retry) {
		t.Fatal("retry changed the batch identity or contents")
	}
}

// TestMoleSubmissionRetriesUnconfirmedCallResults 保护临时远端错误、满邮箱或畸形回执不丢弃批次，原请求重试后只确认一次。
func TestMoleSubmissionRetriesUnconfirmedCallResults(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		err       error
		malformed bool
	}{
		{"database_unavailable", runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "injected SQL error"), false},
		{"mailbox_full", runtime.ErrMailboxFull, false},
		{"malformed_receipt", nil, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newSettlementFixture()
			fixture.firstError, fixture.badFirstReceipt = scenario.err, scenario.malformed
			routes, _ := prepareMoleCluster(t, fixture)
			snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
			hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
			waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
			if fixture.submitCount.Load() != 2 || len(fixture.submitRequests) != 2 || len(fixture.submitted) != 1 || len(fixture.batchQueries) != 0 {
				t.Fatal("未按原请求直接重试，或发生重复入账/额外查账")
			}
			first, retry := <-fixture.submitRequests, <-fixture.submitRequests
			if !reflect.DeepEqual(first, retry) {
				t.Fatal("重试改变了批次身份或内容")
			}
			state, err := fixture.checkpoint.load(context.Background())
			if err != nil || state.Pending != nil || state.Sequence != 1 || state.Pool.Hits != 1 {
				t.Fatalf("批次确认丢失或重复: %+v %v", state, err)
			}
		})
	}
}

// TestMoleCheckpointRetryKeepsReceivedReceipt 保护 Call 已返回后 Redis 故障只重试确认和交付，不再次调用结算或重复统计。
func TestMoleCheckpointRetryKeepsReceivedReceipt(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.firstReplyDelay = 300 * time.Millisecond
	routes, _ := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	var unavailable atomic.Bool
	fixture.checkpoint.client.AddHook(moleRedisFaultHook{unavailable: &unavailable})
	defer unavailable.Store(false)
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("批次未提交")
	}
	unavailable.Store(true)
	// 此查询在同一 Submission 邮箱中等待 Call 返回，确保已走过 Redis 确认失败。
	value, err := moleRPC(t, routes, "mole", moleSubmissionName, submissionStateCommand)
	if err != nil || value.(map[string]any)["blocked"] != false || value.(map[string]any)["batch_id"] == nil {
		t.Fatalf("Redis 故障未保留当前批次: %v %v", value, err)
	}
	unavailable.Store(false)
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	if fixture.submitCount.Load() != 1 {
		t.Fatal("已有有效回执仍重复调用了结算")
	}
	state, err := fixture.checkpoint.load(context.Background())
	if err != nil || state.Pending != nil || state.Pool.Hits != 1 {
		t.Fatalf("检查点重试丢失或重复统计: %+v %v", state, err)
	}
}

// TestMoleRestartReloadsSettlementState 保护结算已提交但回执丢失后，重启从 Redis 原批次补齐统计且不重复扣款。
func TestMoleRestartReloadsSettlementState(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.hideReceipts = true
	routes, stop := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("batch was not received by settlement")
	}
	stop()
	state, err := fixture.checkpoint.load(context.Background())
	if err != nil || state.Pending == nil || state.Sequence != 0 {
		t.Fatalf("lost reply was not recoverable: %+v %v", state, err)
	}
	if _, err := moleRPC(t, routes, "settlement", "settlement", 997, false); err != nil {
		t.Fatal(err)
	}
	address, _ := startMoleTestNode(t, "mole", routes, newMoleTestMain(t, fixture))
	routes["mole"] = address
	reconnected := enterMoleTestUser(t, routes, 1, "new-login", "access-02")
	want := int64(10000) - hit["debit"].(int64) + hit["credit"].(int64)
	if reconnected["silver"].(map[string]any)["balance"] != want {
		t.Fatal("restart did not reload settlement balance")
	}
	value, err := moleRPC(t, routes, "settlement", "settlement", settlementOperationCommand, uint64(1), hit["hit_id"])
	if err != nil || value == nil || value.(map[string]any)["status"] != "committed" {
		t.Fatalf("durable operation was not queryable: %v %v", value, err)
	}
	value, err = moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand)
	if err != nil || value.(map[string]any)["hits"] != int64(1) {
		t.Fatalf("statistics were not rebuilt: %v %v", value, err)
	}
}

// TestMoleExitDoesNotCancelHandedOffHit 保护退出回收空组后记账继续完成，重进加载确认余额且不沿用已退出组实例。
func TestMoleExitDoesNotCancelHandedOffHit(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	name := snapshot["attack_name"].(string)
	if _, err := moleRPC(t, routes, "mole", name, attackLeaveCommand, uint64(1), "login"); err != nil {
		t.Fatal(err)
	}
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	reentered := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if reentered["attack_name"] == name {
		t.Fatal("empty group instance was reused")
	}
	want := int64(10000) - hit["debit"].(int64) + hit["credit"].(int64)
	if reentered["silver"].(map[string]any)["balance"] != want {
		t.Fatalf("exit lost the submitted hit: %v", reentered["silver"])
	}
}

// TestMoleRedisFailureStopsSQLUntilCheckpointSaved 保护 Redis 故障时命中仍可先返回内存结果，但结算不得越过检查点；恢复后原结果入账。
func TestMoleRedisFailureStopsSQLUntilCheckpointSaved(t *testing.T) {
	fixture := newSettlementFixture()
	routes, _ := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	var unavailable atomic.Bool
	fixture.checkpoint.client.AddHook(moleRedisFaultHook{unavailable: &unavailable})
	unavailable.Store(true)
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	if hit["status"] != "accepted" {
		t.Fatal("Redis blocked immediate memory result")
	}
	time.Sleep(250 * time.Millisecond)
	if len(fixture.submitted) != 0 {
		t.Fatal("SQL executed without durable pending batch")
	}
	unavailable.Store(false)
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	state, err := fixture.checkpoint.load(context.Background())
	if err != nil || state.Pool.Hits != 1 || state.Pending != nil {
		t.Fatalf("recovery missed accounting: %+v %v", state, err)
	}
}

// TestMoleMissingCheckpointNeedsExplicitInitialization 保护 Redis 键丢失时拒绝计费进入，只有显式初始化后才开放业务。
func TestMoleMissingCheckpointNeedsExplicitInitialization(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.checkpoint = moleCheckpoint{client: startMoleRedis(t), key: "test:missing"}
	routes, _ := prepareMoleCluster(t, fixture)
	if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingResumeCommand, uint64(1), "member-test"); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("missing checkpoint allowed balance access: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("missing checkpoint initialized zero pool: %v", err)
	}
	if err := fixture.checkpoint.initialize(context.Background(), molePool{}); err != nil {
		t.Fatal(err)
	}
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if snapshot["silver"].(map[string]any)["balance"] != int64(10000) {
		t.Fatal("explicit initialization did not open accounting")
	}
}
