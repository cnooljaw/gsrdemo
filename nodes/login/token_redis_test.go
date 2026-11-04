package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func startTokenRedis(t *testing.T) *redis.Client {
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
	_ = listener.Close()
	_, port, _ := net.SplitHostPort(address)
	command := exec.Command(binary, "--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	client := redis.NewClient(&redis.Options{Addr: address, Protocol: 2, DisableIdentity: true, MaxRetries: -1,
		ContextTimeoutEnabled: true, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for client.Ping(context.Background()).Err() != nil {
		if time.Now().After(deadline) {
			t.Fatal("temporary Redis did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return client
}

// TestTokenRedisLifecycle 保护随机 Token 只存摘要、固定到期、可跨对象校验和撤销，普通校验不续期。
func TestTokenRedisLifecycle(t *testing.T) {
	ctx := context.Background()
	client := startTokenRedis(t)
	store := tokenStore{client: client, prefix: "test:token"}
	token, expires, err := store.issue(ctx, 7, time.Hour)
	if err != nil || len(token) != 43 || expires <= time.Now().UnixMilli() {
		t.Fatalf("issue: expires=%d err=%v", expires, err)
	}
	hash := sha256.Sum256([]byte(token))
	key := "test:token:" + hex.EncodeToString(hash[:])
	if client.Exists(ctx, key).Val() != 1 || strings.Contains(client.Get(ctx, key).Val(), token) {
		t.Fatal("Redis 未按摘要保存 Token")
	}
	before := client.PTTL(ctx, key).Val()
	restarted := tokenStore{client: client, prefix: store.prefix}
	if uid, err := restarted.verify(ctx, token); err != nil || uid != 7 {
		t.Fatalf("verify: uid=%d err=%v", uid, err)
	}
	if after := client.PTTL(ctx, key).Val(); after <= 0 || after > before {
		t.Fatal("校验续期或丢失过期时间")
	}
	for _, invalid := range []string{"", token + "x", strings.Repeat("A", 43)} {
		if _, err := store.verify(ctx, invalid); !errors.Is(err, errInvalidToken) {
			t.Fatal("无效 Token 未拒绝", err)
		}
	}
	if removed, err := store.revoke(ctx, token); err != nil || !removed {
		t.Fatal("revoke", err)
	}
	if _, err := store.verify(ctx, token); !errors.Is(err, errInvalidToken) {
		t.Fatal("已撤销 Token 被接纳", err)
	}
	if removed, err := store.revoke(ctx, token); err != nil || removed {
		t.Fatal("重复撤销不幂等", err)
	}
}

// TestTokenExpiryAndRedisFailure 保护过期凭据被拒绝，Redis 故障不降级放行，错误不泄露 Token。
func TestTokenExpiryAndRedisFailure(t *testing.T) {
	ctx := context.Background()
	client := startTokenRedis(t)
	store := tokenStore{client: client, prefix: "test:expiry"}
	token, _, err := store.issue(ctx, 9, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	client.PExpire(ctx, store.key(token), time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for client.Exists(ctx, store.key(token)).Val() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := store.verify(ctx, token); !errors.Is(err, errInvalidToken) {
		t.Fatal("过期 Token 未拒绝", err)
	}
	_ = client.Close()
	if _, err := store.verify(ctx, token); !errors.Is(err, errTokenStoreUnavailable) || strings.Contains(err.Error(), token) {
		t.Fatal("Redis 故障未安全拒绝", err)
	}
	if _, _, err := store.issue(ctx, 9, time.Hour); !errors.Is(err, errTokenStoreUnavailable) {
		t.Fatal("Redis 故障仍签发 Token", err)
	}
}
