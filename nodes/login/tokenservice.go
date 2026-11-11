package main

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// tokenService 的签发、校验、撤销共用固定逻辑入口。
//
//rpc:service token public name=tokenServiceName
const (
	tokenServiceName                   = "token"
	issueCommand     runtime.CommandID = 1
	verifyCommand    runtime.CommandID = 2
	revokeCommand    runtime.CommandID = 3
)

const tokenCallTimeout = 500 * time.Millisecond

// tokenService 只处理 Redis 凭据，不等待密码 SQL、渠道认证或 Online。
type tokenService struct {
	store      tokenStore
	ttl        time.Duration
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*tokenService)(nil)

func newTokenService(store tokenStore, ttl time.Duration) *tokenService {
	service := &tokenService{store: store, ttl: ttl}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		issueCommand:  service.handleIssue,
		verifyCommand: service.handleVerify,
		revokeCommand: service.handleRevoke,
	})
	return service
}

func (service *tokenService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	if service.store.client == nil || service.store.prefix == "" || service.ttl <= 0 {
		return errors.New("token requires Redis, key prefix and positive TTL")
	}
	return nil
}

func (service *tokenService) OnStop(_ context.Context) error {
	return nil
}

func (service *tokenService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *tokenService) handleIssue(_ gsr.CommandContext, uid uint64) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenCallTimeout)
	defer cancel()
	token, expires, err := service.store.issue(ctx, uid, service.ttl)
	if err != nil {
		return nil, err
	}
	return map[string]any{"user_id": uid, "access_token": token, "expires_at_ms": expires}, nil
}

func (service *tokenService) handleVerify(_ gsr.CommandContext, token string) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenCallTimeout)
	defer cancel()
	return service.store.verify(ctx, token)
}

func (service *tokenService) handleRevoke(_ gsr.CommandContext, token string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenCallTimeout)
	defer cancel()
	return service.store.revoke(ctx, token)
}
