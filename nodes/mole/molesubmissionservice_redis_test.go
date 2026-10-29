package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func startMoleRedis(t *testing.T) *redis.Client {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	client := redis.NewClient(&redis.Options{Addr: address, Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true,
		MaxRetries: -1, DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := client.Ping(ctx).Err()
		cancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatal("temporary Redis did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testMoleCheckpoint(t *testing.T) moleCheckpoint {
	t.Helper()
	store := moleCheckpoint{client: startMoleRedis(t), key: "test:mole"}
	if err := store.initializeLanes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestMoleLaneInitializationPreservesLegacyState 保护旧统计不重置，旧在途批次必须先结清，才能明确迁移到固定通道。
func TestMoleLaneInitializationPreservesLegacyState(t *testing.T) {
	store := moleCheckpoint{client: startMoleRedis(t), key: "legacy"}
	ctx := context.Background()
	old := moleCheckpointState{Version: 1, Pool: molePool{Hits: 10, Debit: 1000, Credit: 1980}, Sequence: 3, LastBatchID: "batch-3",
		Pending: &moleBatch{ID: "batch-4", Sequence: 4, Events: []hitEvent{exampleHit("old", 1)}}}
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.client.Set(ctx, store.key, encoded, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.initializeLanes(ctx); err == nil {
		t.Fatal("migrated while legacy batch was pending")
	}
	state, err := store.load(ctx)
	if err != nil || !reflect.DeepEqual(state, old) {
		t.Fatal("failed migration changed old accounting")
	}
	if count, err := store.client.Exists(ctx, store.forLane(1).key).Result(); err != nil || count != 0 {
		t.Fatal("opened new lane before legacy recovery")
	}
	// 模拟旧版已结清并保存检查点；迁移只增加路由元数据。
	old.Pending = nil
	encoded, _ = json.Marshal(old)
	if err := store.client.Set(ctx, store.key, encoded, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.initializeLanes(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = store.load(ctx)
	old.Routing = moleSubmissionCount
	if err != nil || !reflect.DeepEqual(state, old) {
		t.Fatalf("migration reset statistics or sequence: %+v %v", state, err)
	}
	for lane := 1; lane < moleSubmissionCount; lane++ {
		state, err := store.forLane(lane).load(ctx)
		if err != nil || state.Routing != moleSubmissionCount || state.Pool.Hits != 0 {
			t.Fatalf("new lane not initialized: %+v %v", state, err)
		}
	}
}

// TestMoleCheckpointRecoversOriginalBatch 保护提交前的批次可由新 owner 原样恢复，且不能被另一个批次覆盖。
func TestMoleCheckpointRecoversOriginalBatch(t *testing.T) {
	store := testMoleCheckpoint(t)
	ctx := context.Background()
	batch := moleBatch{ID: "batch-1", Sequence: 1, Events: []hitEvent{exampleHit("hit-1", 1)}}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	recovered := moleCheckpoint{client: store.client, key: store.key}
	state, err := recovered.load(ctx)
	if err != nil || !reflect.DeepEqual(state.Pending, &batch) {
		t.Fatalf("recover: %+v %v", state, err)
	}
	if err := recovered.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	different := batch.clone()
	different.Events[0].Credit++
	if err := recovered.prepare(ctx, different); err == nil {
		t.Fatal("overwrote original batch")
	}
	different = batch.clone()
	different.ID = "another"
	if err := recovered.prepare(ctx, different); err == nil {
		t.Fatal("accepted second in-flight batch")
	}
}

// TestMoleCheckpointAppliesOnlyFreshCommittedResults 保护统计与检查点一起推进，重复回执、拒绝及跨批旧操作不重复累计。
func TestMoleCheckpointAppliesOnlyFreshCommittedResults(t *testing.T) {
	store := testMoleCheckpoint(t)
	ctx := context.Background()
	batch := moleBatch{ID: "batch-1", Sequence: 1, Events: []hitEvent{exampleHit("hit-1", 1), exampleHit("hit-2", 2), exampleHit("hit-3", 3)}}
	receipt := batchReceipt{BatchID: batch.ID, Results: []operationResult{
		{HitID: "hit-1", Status: "committed", OriginBatch: batch.ID},
		{HitID: "hit-2", Status: "rejected", OriginBatch: batch.ID},
		{HitID: "hit-3", Status: "committed", OriginBatch: "original"},
	}}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	first, err := store.finish(ctx, batch, receipt)
	if err != nil || first.Pending != nil || first.Sequence != 1 || first.Pool.Hits != 1 || first.Pool.Debit != batch.Events[0].Debit || first.Pool.Credit != batch.Events[0].Credit {
		t.Fatalf("finish: %+v %v", first, err)
	}
	repeated, err := store.finish(ctx, batch, receipt)
	if err != nil || !reflect.DeepEqual(first, repeated) {
		t.Fatalf("duplicate completion: %+v %v", repeated, err)
	}
	if err := store.initialize(ctx, molePool{}); err != nil {
		t.Fatal(err)
	}
	state, err := store.load(ctx)
	if err != nil || !reflect.DeepEqual(first, state) {
		t.Fatal("initialization reset confirmed statistics")
	}
}

// TestMoleCheckpointFailurePreservesPending 保护统计溢出或 Redis 故障不清除待处理批次，未知或损坏状态不当成零统计。
func TestMoleCheckpointFailurePreservesPending(t *testing.T) {
	store := testMoleCheckpoint(t)
	ctx := context.Background()
	state := moleCheckpointState{Version: 1, Pool: molePool{Hits: math.MaxInt64}}
	if err := store.client.Del(ctx, store.key).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(ctx); err == nil {
		t.Fatal("missing checkpoint treated as zero")
	}
	if err := store.initialize(ctx, state.Pool); err != nil {
		t.Fatal(err)
	}
	batch := moleBatch{ID: "batch-1", Sequence: 1, Events: []hitEvent{exampleHit("hit-1", 1)}}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.finish(ctx, batch, batchReceipt{BatchID: batch.ID, Results: []operationResult{{HitID: "hit-1", Status: "committed", OriginBatch: batch.ID}}}); err == nil {
		t.Fatal("overflow accepted")
	}
	loaded, err := store.load(ctx)
	if err != nil || loaded.Pending == nil || loaded.Sequence != 0 {
		t.Fatal("failed completion lost recovery state")
	}
	if err := store.client.Set(ctx, store.key, "broken", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(ctx); err == nil {
		t.Fatal("corrupt checkpoint treated as zero")
	}
	if err := store.client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(ctx); err == nil {
		t.Fatal("Redis error treated as empty state")
	}
}

// TestMoleCheckpointCASDoesNotOverwriteAnotherWriter 保护过期快照不能覆盖已经保存的检查点。
func TestMoleCheckpointCASDoesNotOverwriteAnotherWriter(t *testing.T) {
	store := testMoleCheckpoint(t)
	ctx := context.Background()
	state, raw, err := store.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	batch := moleBatch{ID: "batch-1", Sequence: 1, Events: []hitEvent{exampleHit("hit-1", 1)}}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := store.replace(ctx, raw, state); err == nil {
		t.Fatal("stale snapshot overwrote pending batch")
	}
}

type moleRedisFaultHook struct {
	unavailable  *atomic.Bool
	loseReply    *atomic.Bool
	panicKey     string
	panicEnabled *atomic.Bool
}

func (hook moleRedisFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (hook moleRedisFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (hook moleRedisFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if hook.panicEnabled != nil && hook.panicEnabled.Load() && command.Name() == "get" && len(command.Args()) > 1 && command.Args()[1] == hook.panicKey {
			panic("injected submission panic")
		}
		if hook.unavailable != nil && hook.unavailable.Load() {
			return errors.New("test Redis unavailable")
		}
		err := next(ctx, command)
		if err == nil && (command.Name() == "eval" || command.Name() == "evalsha") && hook.loseReply != nil && hook.loseReply.CompareAndSwap(true, false) {
			return errors.New("test Redis reply lost after checkpoint committed")
		}
		return err
	}
}

// TestMoleCheckpointLostWriteReply 保护 Redis 已更新但回复丢失时，重试不会重复累计或复活已完成批次。
func TestMoleCheckpointLostWriteReply(t *testing.T) {
	store := testMoleCheckpoint(t)
	ctx := context.Background()
	batch := moleBatch{ID: "batch-1", Sequence: 1, Events: []hitEvent{exampleHit("hit-1", 1)}}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	var lose atomic.Bool
	lose.Store(true)
	store.client.AddHook(moleRedisFaultHook{loseReply: &lose})
	receipt := batchReceipt{BatchID: batch.ID, Results: []operationResult{{HitID: "hit-1", Status: "committed", OriginBatch: batch.ID}}}
	if _, err := store.finish(ctx, batch, receipt); err == nil {
		t.Fatal("did not inject lost acknowledgement")
	}
	state, err := store.load(ctx)
	if err != nil || state.Pending != nil || state.Sequence != 1 || state.Pool.Hits != 1 {
		t.Fatalf("committed checkpoint lost: %+v %v", state, err)
	}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	again, err := store.finish(ctx, batch, receipt)
	if err != nil || !reflect.DeepEqual(state, again) {
		t.Fatalf("lost reply applied twice: %+v %v", again, err)
	}
}

// TestMoleCheckpointKeepsIntegerPrecision 保护超过 Lua 精确整数范围的累计值仍按 int64 精确更新。
func TestMoleCheckpointKeepsIntegerPrecision(t *testing.T) {
	store := moleCheckpoint{client: startMoleRedis(t), key: "test:large"}
	ctx := context.Background()
	base := int64(1<<53) + 17
	if err := store.initialize(ctx, molePool{Hits: base, Debit: base, Credit: base}); err != nil {
		t.Fatal(err)
	}
	event := exampleHit("hit-1", 1)
	event.Credit = hammerReward
	batch := moleBatch{ID: "batch-1", Sequence: 1, Events: []hitEvent{event}}
	if err := store.prepare(ctx, batch); err != nil {
		t.Fatal(err)
	}
	state, err := store.finish(ctx, batch, batchReceipt{BatchID: batch.ID, Results: []operationResult{{HitID: event.HitID, Status: "committed", OriginBatch: batch.ID}}})
	want := molePool{Hits: base + 1, Debit: base + hammerCost, Credit: base + hammerReward}
	if err != nil || state.Pool != want {
		t.Fatalf("integer precision lost: %+v want %+v error %v", state.Pool, want, err)
	}
	loaded, err := store.load(ctx)
	if err != nil || loaded.Pool != want {
		t.Fatalf("persisted integer precision lost: %+v %v", loaded.Pool, err)
	}
}
