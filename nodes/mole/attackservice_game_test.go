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
