package access

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
	"google.golang.org/protobuf/proto"
)

func (service *agent) pendingID() string {
	return service.pending.GetOperationId()
}

func (service *agent) positionResult(code, operationID string) *businesspb.PositionResult {
	result := &businesspb.PositionResult{Code: code, OperationId: operationID, Session: model.View(service.session, service.pendingID())}
	if service.session.GetSnapshot() != nil {
		result.Snapshot = proto.Clone(service.session.Snapshot).(*businesspb.MoleSnapshot)
	}
	return result
}

func (service *agent) install(confirmed *businesspb.SessionRecord) bool {
	identity := confirmed.GetIdentity()
	if identity == nil || identity.LoginId != service.identity.GetLoginId() || identity.UserId != service.identity.GetUserId() {
		return false
	}
	current := service.session.GetIdentity()
	if current != nil && identity.LocationVersion < current.LocationVersion {
		return false
	}
	if current != nil && identity.LocationVersion == current.LocationVersion && (!model.SameIdentity(current, identity) || !proto.Equal(service.session.Room, confirmed.Room)) {
		return false
	}
	snapshot := service.session.GetSnapshot()
	installed := proto.Clone(confirmed).(*businesspb.SessionRecord)
	if model.SameBinding(service.session.GetRoom(), confirmed.Room) && snapshot != nil && snapshot.StateVersion > confirmed.GetSnapshot().GetStateVersion() {
		installed.Snapshot = proto.Clone(snapshot).(*businesspb.MoleSnapshot)
	}
	service.session = installed
	service.identity = proto.Clone(identity).(*businesspb.Identity)
	return true
}

func (service *agent) positionCall(ctx context.Context, self gsr.CommandContext, request *businesspb.PositionCommand) *businesspb.PositionResult {
	reply := &businesspb.PositionReply{}
	if err := command.Call(ctx, self, "online", "sessions", command.Position, request, reply); err != nil {
		if service.pending != nil {
			return service.positionResult("operation_pending", service.pending.OperationId)
		}
		return service.positionResult("unavailable", request.OperationId)
	}
	service.install(reply.Confirmed)
	if service.pending != nil && reply.PendingOperationId != service.pending.OperationId {
		service.pending = nil
	}
	result := service.positionResult(reply.Code, reply.OperationId)
	result.Refreshed = reply.Refreshed
	return result
}

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
	auth := &businesspb.AuthReply{}
	err := command.Call(ctx, self, "login", "auth", command.Authenticate, &businesspb.AuthRequest{Username: request.Username, Password: request.Password}, auth)
	if err != nil {
		auth.Code = "unavailable"
	}
	if auth.Code != "ok" {
		return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: service.positionResult(auth.Code, request.OperationId)})
	}
	if err := service.register(self); err != nil {
		return err
	}
	if service.identity == nil || service.identity.UserId != auth.UserId {
		loginID, err := model.NewID()
		if err != nil {
			return err
		}
		service.identity = &businesspb.Identity{UserId: auth.UserId, LoginId: loginID, AccessNode: "access", AgentName: string(service.name)}
	}
	service.pending = &businesspb.PositionCommand{OperationId: request.OperationId, Kind: "bind", Identity: proto.Clone(service.identity).(*businesspb.Identity)}
	result := service.positionCall(ctx, self, service.pending)
	return client.Response(uint32(businesspb.MsgId_S2C_PASSWORD_LOGIN), &businesspb.PasswordLoginResponse{Result: result})
}

func (service *agent) handleEnter(self gsr.CommandContext, client protocol.Context, request *businesspb.EnterMoleRequest) error {
	result := service.changePosition(self, uint32(businesspb.MsgId_C2S_ENTER_MOLE), "enter", request.OperationId, "")
	return client.Response(uint32(businesspb.MsgId_S2C_ENTER_MOLE), &businesspb.EnterMoleResponse{Result: result})
}

func (service *agent) handleLeave(self gsr.CommandContext, client protocol.Context, request *businesspb.LeaveRoomRequest) error {
	result := service.changePosition(self, uint32(businesspb.MsgId_C2S_LEAVE_ROOM), "leave", request.OperationId, request.RoomId)
	return client.Response(uint32(businesspb.MsgId_S2C_LEAVE_ROOM), &businesspb.LeaveRoomResponse{Result: result})
}

func (service *agent) changePosition(self gsr.CommandContext, id uint32, kind, operationID, roomID string) *businesspb.PositionResult {
	if code := model.Admission(service.session, service.pendingID(), id); code != "ok" {
		return service.positionResult(code, operationID)
	}
	if !model.ValidOperationID(operationID) {
		return service.positionResult("invalid", operationID)
	}
	service.pending = &businesspb.PositionCommand{OperationId: operationID, Kind: kind, Identity: proto.Clone(service.identity).(*businesspb.Identity), RoomId: roomID}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return service.positionCall(ctx, self, service.pending)
}

func (service *agent) query(self gsr.CommandContext, id uint32, operationID string) *businesspb.PositionResult {
	if code := model.Admission(service.session, service.pendingID(), id); code != "ok" {
		return service.positionResult(code, operationID)
	}
	if service.pending != nil && operationID != "" && operationID != service.pending.OperationId {
		return service.positionResult("operation_unknown", operationID)
	}
	kind := "query"
	if id == uint32(businesspb.MsgId_C2S_QUERY_OPERATION) {
		kind = "operation"
		if !model.ValidOperationID(operationID) {
			return service.positionResult("invalid", operationID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return service.positionCall(ctx, self, &businesspb.PositionCommand{Kind: kind, OperationId: operationID, Identity: service.identity})
}

func (service *agent) handleQuerySession(self gsr.CommandContext, client protocol.Context, _ *businesspb.QuerySessionRequest) error {
	return client.Response(uint32(businesspb.MsgId_S2C_QUERY_SESSION), &businesspb.QuerySessionResponse{Result: service.query(self, uint32(businesspb.MsgId_C2S_QUERY_SESSION), "")})
}

func (service *agent) handleQueryOperation(self gsr.CommandContext, client protocol.Context, request *businesspb.QueryOperationRequest) error {
	return client.Response(uint32(businesspb.MsgId_S2C_QUERY_OPERATION), &businesspb.QueryOperationResponse{Result: service.query(self, uint32(businesspb.MsgId_C2S_QUERY_OPERATION), request.OperationId)})
}

func (service *agent) observe(snapshot *businesspb.MoleSnapshot) bool {
	if snapshot == nil || snapshot.RoomId != service.session.GetRoom().GetRoomId() || snapshot.StateVersion <= service.session.GetSnapshot().GetStateVersion() {
		return false
	}
	service.session.Snapshot = proto.Clone(snapshot).(*businesspb.MoleSnapshot)
	return true
}

func (service *agent) handleSnapshot(self gsr.CommandContext, client protocol.Context, _ *businesspb.MoleSnapshotRequest) error {
	reply := &businesspb.RoomReply{Code: model.Admission(service.session, service.pendingID(), uint32(businesspb.MsgId_C2S_MOLE_SNAPSHOT))}
	if reply.Code == "ok" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		err := command.Call(ctx, self, "mole", "mole/"+service.session.Room.RoomId, command.RoomSnapshot, service.session.Room, reply)
		if err != nil {
			reply.Code = "unavailable"
		}
		if reply.Code == "ok" {
			service.observe(reply.Snapshot)
		}
	}
	return client.Response(uint32(businesspb.MsgId_S2C_MOLE_SNAPSHOT), &businesspb.MoleSnapshotResponse{Code: reply.Code, Snapshot: service.session.GetSnapshot()})
}

func (service *agent) handleHit(self gsr.CommandContext, client protocol.Context, request *businesspb.MoleHitRequest) error {
	reply := &businesspb.MoleHitResponse{Code: model.Admission(service.session, service.pendingID(), uint32(businesspb.MsgId_C2S_MOLE_HIT)), OperationId: request.OperationId}
	if reply.Code == "ok" && request.RoomId != service.session.Room.RoomId {
		reply.Code = "state_mismatch"
	}
	if reply.Code == "ok" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		err := command.Call(ctx, self, "mole", "mole/"+service.session.Room.RoomId, command.Hit, &businesspb.HitCommand{Binding: service.session.Room, Hit: request}, reply)
		if err != nil {
			reply.Code = "unavailable"
		}
		service.observe(reply.Snapshot)
	}
	reply.Snapshot = service.session.GetSnapshot()
	return client.Response(uint32(businesspb.MsgId_S2C_MOLE_HIT), reply)
}

func (service *agent) handleRoomState(self gsr.CommandContext, payload []byte) error {
	notice := &businesspb.RoomNotice{}
	if err := proto.Unmarshal(payload, notice); err != nil {
		return err
	}
	if !model.SameBinding(service.session.GetRoom(), notice.Binding) || !service.observe(notice.Snapshot) {
		return nil
	}
	if err := service.protocolDispatcher.Push(uint32(businesspb.MsgId_S2C_MOLE_STATE), &businesspb.MoleStatePush{Snapshot: service.session.Snapshot}); err != nil {
		service.disconnect(self)
		self.Exit()
		return errors.Join(err, service.client.Close())
	}
	return nil
}

func (service *agent) handleReplaced(self gsr.CommandContext, payload []byte) error {
	notice := &businesspb.ReplacedNotice{}
	if err := proto.Unmarshal(payload, notice); err != nil {
		return err
	}
	if notice.LoginId == "" || notice.LoginId != service.identity.GetLoginId() {
		return nil
	}
	_ = service.protocolDispatcher.Push(uint32(businesspb.MsgId_S2C_SESSION_REPLACED), &businesspb.SessionReplacedPush{})
	service.disconnect(self)
	self.Exit()
	return service.client.Close()
}

func (service *agent) disconnect(self gsr.CommandContext) {
	if service.identity == nil {
		return
	}
	if err := command.Send(self, "online", "sessions", command.Disconnect, service.identity); err != nil {
		_ = self.Logger().Error("disconnect notice rejected", "error", err)
	}
}
