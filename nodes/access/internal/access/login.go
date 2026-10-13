package access

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
)

func (service *agent) handleLogin(self gsr.CommandContext, client protocol.Context, request *businesspb.PasswordLoginRequest) error {
	id := uint32(businesspb.MsgId_C2S_PASSWORD_LOGIN)
	code := model.Admission(service.session, service.pendingID(), id)
	if code != "ok" {
		return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: service.positionResult(code, request.OperationId)})
	}
	if !model.ValidOperationID(request.OperationId) || len(request.Username) > 32 || len(request.Password) < 8 || len(request.Password) > 128 {
		return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: service.positionResult("invalid", request.OperationId)})
	}
	now := time.Now()
	if !service.lastAuth.IsZero() && now.Sub(service.lastAuth) < 250*time.Millisecond {
		return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: service.positionResult("rate_limited", request.OperationId)})
	}
	service.lastAuth = now
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := self.Cluster().Call(ctx, "login", "auth", command.Authenticate, request.Username, request.Password)
	auth, ok := result.(map[string]any)
	code, _ = auth["code"].(string)
	userID, _ := auth["user_id"].(string)
	if err != nil || !ok || code == "" || (code == "ok" && userID == "") {
		code = "unavailable"
	}
	if code != "ok" {
		return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: service.positionResult(code, request.OperationId)})
	}
	if err := service.register(self); err != nil {
		return err
	}
	if service.identity.UserID == "" || service.identity.UserID != userID {
		loginID, err := model.NewID()
		if err != nil {
			return err
		}
		service.identity = model.Identity{UserID: userID, LoginID: loginID, AccessNode: "access", AgentName: string(service.name)}
	}
	service.pending = &model.PositionCommand{OperationID: request.OperationId, Kind: "bind", Identity: service.identity}
	position := service.positionCall(ctx, self, *service.pending)
	return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: position})
}

func (service *agent) handleReplaced(self gsr.CommandContext, loginID string) error {
	if loginID == "" || loginID != service.identity.LoginID {
		return nil
	}
	_ = service.protocolDispatcher.Push(uint32(businesspb.MsgId_S2C_SESSION_REPLACED), &businesspb.SessionReplacedPush{})
	service.disconnect(self)
	self.Exit()
	return service.client.Close()
}

func (service *agent) disconnect(self gsr.CommandContext) {
	if service.identity.UserID == "" {
		return
	}
	if err := self.Cluster().Send("online", "sessions", command.Disconnect, service.identity.Values()); err != nil {
		_ = self.Logger().Error("disconnect notice rejected", "error", err)
	}
}
