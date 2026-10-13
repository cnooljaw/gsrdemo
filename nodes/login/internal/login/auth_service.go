package login

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
)

type authService struct {
	accounts   map[string]account
	dispatcher *gsr.Dispatcher
	tokens     float64
	updated    time.Time
}

var _ gsr.Service = (*authService)(nil)

func newAuthService(path string) (*authService, error) {
	accounts, err := loadAccounts(path)
	if err != nil {
		return nil, err
	}
	service := &authService{accounts: accounts, tokens: 8, updated: time.Now()}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{command.Authenticate: service.handleAuth})
	return service, nil
}

func (service *authService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

func (service *authService) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, id, args...)
}

func (service *authService) OnStop(_ context.Context) error {
	return nil
}

// 认证只传两个字符串；返回 GSR 支持的映射，不额外定义内部 Protobuf。
func (service *authService) handleAuth(_ gsr.CommandContext, username, password string) (map[string]any, error) {
	if len(service.accounts) == 0 {
		return map[string]any{"code": "authentication_unavailable"}, nil
	}
	now := time.Now()
	service.tokens = min(8, service.tokens+now.Sub(service.updated).Seconds()*8)
	service.updated = now
	if service.tokens < 1 {
		return map[string]any{"code": "rate_limited"}, nil
	}
	service.tokens--
	userID := authenticate(service.accounts, username, password)
	if userID == "" {
		return map[string]any{"code": "invalid_credentials"}, nil
	}
	return map[string]any{"code": "ok", "user_id": userID}, nil
}
