package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/redis/go-redis/v9"
)

var (
	errInvalidToken          = runtime.NewStatusError(runtime.ErrorClassDomain, "auth.invalid_token", "access token is invalid or expired")
	errTokenStoreUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "auth.token_store_unavailable", "token storage is unavailable")
)

// tokenStore 无内存凭据副本，Redis 是 Token 的唯一依据；不保存明文 Token。
type tokenStore struct {
	client *redis.Client
	prefix string
}

type tokenRecord struct {
	UserID      uint64 `json:"user_id"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

func (store tokenStore) key(token string) string {
	hash := sha256.Sum256([]byte(token))
	return store.prefix + ":" + hex.EncodeToString(hash[:])
}

func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(data) == 32 && base64.RawURLEncoding.EncodeToString(data) == token
}

func (store tokenStore) issue(ctx context.Context, uid uint64, ttl time.Duration) (string, int64, error) {
	if uid == 0 || ttl <= 0 {
		return "", 0, errInvalidLoginInput
	}
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", 0, errTokenStoreUnavailable
	}
	token := base64.RawURLEncoding.EncodeToString(data[:])
	expires := time.Now().Add(ttl).UnixMilli()
	encoded, err := json.Marshal(tokenRecord{UserID: uid, ExpiresAtMS: expires})
	if err != nil {
		return "", 0, errTokenStoreUnavailable
	}
	// TTL 与记录一起写入；碰撞或存储结果未知时不把 Token 交给客户端。
	stored, err := store.client.SetNX(ctx, store.key(token), encoded, ttl).Result()
	if err != nil || !stored {
		return "", 0, errTokenStoreUnavailable
	}
	return token, expires, nil
}

func (store tokenStore) verify(ctx context.Context, token string) (uint64, error) {
	if !validToken(token) {
		return 0, errInvalidToken
	}
	data, err := store.client.Get(ctx, store.key(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return 0, errInvalidToken
	}
	if err != nil {
		return 0, errTokenStoreUnavailable
	}
	var record tokenRecord
	if json.Unmarshal(data, &record) != nil || record.UserID == 0 || record.ExpiresAtMS <= 0 {
		return 0, errTokenStoreUnavailable
	}
	if time.Now().UnixMilli() >= record.ExpiresAtMS {
		return 0, errInvalidToken
	}
	return record.UserID, nil
}

func (store tokenStore) revoke(ctx context.Context, token string) (bool, error) {
	if !validToken(token) {
		return false, errInvalidToken
	}
	count, err := store.client.Del(ctx, store.key(token)).Result()
	if err != nil {
		return false, errTokenStoreUnavailable
	}
	return count != 0, nil
}
