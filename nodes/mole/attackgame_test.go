package main

import (
	"errors"
	"testing"
	"time"
)

// TestAttackTargetAndRequestIdentity 保护抢同一目标只接纳一锤、重试不抽样、拒绝后的新尝试使用新 HitID。
func TestAttackTargetAndRequestIdentity(t *testing.T) {
	game := newAttackGame("attack-test")
	now := time.Unix(100, 0)
	game.spawn(now)
	first, repeated, err := game.hit(now, 7, "login-a", "a1", 1, 0)
	if err != nil || repeated || first.Result.Event.Debit != 100 {
		t.Fatalf("first hit: %+v repeated=%v err=%v", first, repeated, err)
	}
	again, repeated, err := game.hit(now, 7, "login-a", "a1", 1, 0)
	if err != nil || !repeated || again != first {
		t.Fatalf("retry changed result: %+v %v %v", again, repeated, err)
	}
	if _, _, err := game.hit(now, 8, "login-b", "b1", 1, 0); !errors.Is(err, errTargetUnavailable) {
		t.Fatalf("second claimant: %v", err)
	}
	game.finish(first, "rejected", now)
	second, _, err := game.hit(now, 8, "login-b", "b1", 1, 0)
	if err != nil || second.Result.Event.HitID == first.Result.Event.HitID {
		t.Fatalf("new attempt reused operation: %+v %v", second, err)
	}
}

// TestAttackExpiredInstanceCannotHitReplacement 保护服务端时间窗口与迟到确认，不依赖 Timer 准点执行。
func TestAttackExpiredInstanceCannotHitReplacement(t *testing.T) {
	game := newAttackGame("attack-test")
	now := time.Unix(100, 0)
	game.spawn(now)
	first, _, err := game.hit(now, 7, "login", "a1", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := game.hit(now.Add(2*time.Second), 8, "other", "a2", 1, 0); !errors.Is(err, errTargetUnavailable) {
		t.Fatalf("expired target: %v", err)
	}
	for range 9 {
		game.spawn(now.Add(3 * time.Second))
	}
	game.finish(first, "rejected", now.Add(3*time.Second))
	if _, _, err := game.hit(now.Add(3*time.Second), 7, "login", "old", 1, 0); !errors.Is(err, errTargetUnavailable) {
		t.Fatalf("old instance hit replacement: %v", err)
	}
	if game.targets[0].Sequence != 10 || game.targets[0].Claim != "" {
		t.Fatalf("late result changed new target: %+v", game.targets[0])
	}
}

// TestAttackAcceptAndSnapshotOwnState 保护确认结果由游戏对象校验，快照修改不会改写目标或命中记录。
func TestAttackAcceptAndSnapshotOwnState(t *testing.T) {
	game := newAttackGame("attack-test")
	now := time.Unix(100, 0)
	game.spawn(now)
	record, _, err := game.hit(now, 7, "login", "action", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	result := memoryHitResult{Event: record.Result.Event, Balance: 900, Available: 900, Revision: 2}
	wrong := result
	wrong.Event.HitID = "different"
	if err := game.acceptHit(record, wrong, now); !errors.Is(err, errMoleOperationUnknown) || record.Status != "pending" {
		t.Fatalf("错误确认改变记录: %v", err)
	}
	if err := game.acceptHit(record, result, now); err != nil {
		t.Fatal(err)
	}
	if record.Status != "accepted" || record.Result != result {
		t.Fatal("确认未保存完整结果")
	}
	snapshot := game.view(now, 7)
	targets := snapshot["targets"].([]any)
	targets[0].(map[string]any)["spawn_seq"] = uint64(999)
	hits := snapshot["hits"].([]any)
	hits[0].(map[string]any)["status"] = "rejected"
	if game.targets[0].Sequence != 1 || record.Status != "accepted" {
		t.Fatal("快照暴露可变状态")
	}
	if len(game.view(now, 8)["hits"].([]any)) != 0 {
		t.Fatal("其他用户读到了私人命中记录")
	}
}
