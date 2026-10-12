package login

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"google.golang.org/protobuf/proto"
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

func (service *authService) handleAuth(_ gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.AuthRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	if len(service.accounts) == 0 {
		return proto.Marshal(&businesspb.AuthReply{Code: "authentication_unavailable"})
	}
	now := time.Now()
	service.tokens = min(8, service.tokens+now.Sub(service.updated).Seconds()*8)
	service.updated = now
	if service.tokens < 1 {
		return proto.Marshal(&businesspb.AuthReply{Code: "rate_limited"})
	}
	service.tokens--
	userID := authenticate(service.accounts, request.Username, request.Password)
	if userID == "" {
		return proto.Marshal(&businesspb.AuthReply{Code: "invalid_credentials"})
	}
	return proto.Marshal(&businesspb.AuthReply{Code: "ok", UserId: userID})
}
