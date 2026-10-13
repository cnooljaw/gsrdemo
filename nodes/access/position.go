package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
)

func (service *agent) pendingID() string {
	if service.pending == nil {
		return ""
	}
	return service.pending.OperationID
}

func (service *agent) positionResult(code, operationID string) *businesspb.PositionResult {
	result := &businesspb.PositionResult{Code: code, OperationId: operationID, Session: model.View(service.session, service.pendingID())}
	if service.moleSnapshot != nil {
		result.Snapshot = service.moleSnapshot
	}
	return result
}

func (service *agent) installPosition(confirmed model.SessionRecord) bool {
	identity := confirmed.Identity
	if identity.UserID == "" || identity.LoginID != service.identity.LoginID || identity.UserID != service.identity.UserID {
		return false
	}
	current := service.session.Identity
	if current.UserID != "" && identity.LocationVersion < current.LocationVersion {
		return false
	}
	if current.UserID != "" && identity.LocationVersion == current.LocationVersion && (current != identity || service.session.Room != confirmed.Room || service.session.GameKind != confirmed.GameKind) {
		return false
	}
	if !model.SameBinding(service.session.Room, confirmed.Room) {
		service.moleSnapshot = nil
	} else if service.session.RoomStateVersion > confirmed.RoomStateVersion {
		confirmed.RoundID = service.session.RoundID
		confirmed.RoomPhase = service.session.RoomPhase
		confirmed.RoomStateVersion = service.session.RoomStateVersion
	}
	service.session = confirmed
	service.identity = identity
	return true
}

func (service *agent) positionCall(ctx context.Context, self gsr.CommandContext, request model.PositionCommand) *businesspb.PositionResult {
	value, err := self.Cluster().Call(ctx, "online", "sessions", command.Position, request.OperationID, request.Kind, request.Identity.Values(), request.RoomID)
	reply, ok := value.(map[string]any)
	code, _ := reply["code"].(string)
	if err != nil || !ok || code == "" {
		return service.positionUnavailable(request.OperationID)
	}
	if value, exists := reply["confirmed"]; exists {
		confirmed, ok := model.ReadSession(value)
		if !ok {
			return service.positionUnavailable(request.OperationID)
		}
		if service.installPosition(confirmed) {
			snapshot, _ := reply["snapshot"].(map[string]any)
			service.observeMole(snapshot)
			if service.session.GameKind == "mole" && service.session.Room.RoomID != "" &&
				(service.moleSnapshot == nil || service.moleSnapshot.StateVersion < service.session.RoomStateVersion) {
				// 后台可能已完成原操作；确认位置后从当前 Room 补取游戏数据。
				service.refreshMole(ctx, self)
			}
		}
	} else if code == "ok" {
		return service.positionUnavailable(request.OperationID)
	}
	pending, _ := reply["pending_operation_id"].(string)
	if service.pending != nil && pending != service.pending.OperationID {
		service.pending = nil
	}
	operationID, _ := reply["operation_id"].(string)
	result := service.positionResult(code, operationID)
	result.Refreshed, _ = reply["refreshed"].(bool)
	return result
}

func (service *agent) positionUnavailable(operationID string) *businesspb.PositionResult {
	if service.pending != nil {
		return service.positionResult("operation_pending", service.pending.OperationID)
	}
	return service.positionResult("unavailable", operationID)
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
	service.pending = &model.PositionCommand{OperationID: operationID, Kind: kind, Identity: service.identity, RoomID: roomID}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return service.positionCall(ctx, self, *service.pending)
}

func (service *agent) query(self gsr.CommandContext, id uint32, operationID string) *businesspb.PositionResult {
	if code := model.Admission(service.session, service.pendingID(), id); code != "ok" {
		return service.positionResult(code, operationID)
	}
	if service.pending != nil && operationID != "" && operationID != service.pending.OperationID {
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
	return service.positionCall(ctx, self, model.PositionCommand{Kind: kind, OperationID: operationID, Identity: service.identity})
}

func (service *agent) handleQuerySession(self gsr.CommandContext, client protocol.Context, _ *businesspb.QuerySessionRequest) error {
	return client.Response(uint32(businesspb.MsgId_S2C_QUERY_SESSION), &businesspb.QuerySessionResponse{Result: service.query(self, uint32(businesspb.MsgId_C2S_QUERY_SESSION), "")})
}

func (service *agent) handleQueryOperation(self gsr.CommandContext, client protocol.Context, request *businesspb.QueryOperationRequest) error {
	return client.Response(uint32(businesspb.MsgId_S2C_QUERY_OPERATION), &businesspb.QueryOperationResponse{Result: service.query(self, uint32(businesspb.MsgId_C2S_QUERY_OPERATION), request.OperationId)})
}
