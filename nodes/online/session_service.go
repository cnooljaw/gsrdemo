package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
)

type positionOperation struct {
	request     model.PositionCommand
	target      model.SessionRecord
	expected    model.RoomBinding
	reservation model.RoomBinding
	connected   bool
	roomRequest *model.RoomRequest
}
type positionOutcome struct {
	request model.PositionCommand
	code    string
}
type onlineUser struct {
	confirmed   model.SessionRecord
	connected   bool
	nextVersion uint64
	pending     *positionOperation
	outcomes    map[string]positionOutcome
}
type sessionService struct {
	dispatcher *gsr.Dispatcher
	users      map[string]*onlineUser
	userIDs    []string
	sweepIndex int
}

var _ gsr.Service = (*sessionService)(nil)

func newSessionService() *sessionService {
	service := &sessionService{users: make(map[string]*onlineUser)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{command.Position: service.handlePosition, command.Disconnect: service.handleDisconnect, command.RoomPhase: service.handlePhase, command.Sweep: service.handleSweep})
	return service
}

func (service *sessionService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(time.Second, command.Sweep)
	return err
}

func (service *sessionService) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, id, args...)
}

func (service *sessionService) OnStop(_ context.Context) error {
	return nil
}

func (service *sessionService) begin(request model.PositionCommand) (*onlineUser, map[string]any) {
	invalid := map[string]any{"code": "invalid", "operation_id": request.OperationID}
	identity := request.Identity
	if !model.ValidOperationID(request.OperationID) || identity.UserID == "" || identity.LoginID == "" || identity.AccessNode == "" || identity.AgentName == "" {
		return nil, invalid
	}
	user := service.users[identity.UserID]
	if user == nil {
		if request.Kind != "bind" {
			return nil, map[string]any{"code": "not_authenticated"}
		}
		if len(service.users) >= 512 {
			return nil, map[string]any{"code": "capacity"}
		}
		user = &onlineUser{outcomes: make(map[string]positionOutcome)}
		service.users[identity.UserID] = user
		service.userIDs = append(service.userIDs, identity.UserID)
	}
	if previous, ok := user.outcomes[request.OperationID]; ok {
		code := previous.code
		if previous.request != request {
			code = "operation_conflict"
		}
		if user.confirmed.Identity.LoginID != identity.LoginID {
			code = "binding_replaced"
		}
		return user, service.reply(user, request.OperationID, code)
	}
	if user.pending != nil {
		if user.pending.request.OperationID != request.OperationID {
			code := "operation_pending"
			if user.pending.target.Identity.LoginID != identity.LoginID {
				code = "user_busy"
			}
			return user, service.reply(user, request.OperationID, code)
		}
		if user.pending.request != request {
			return user, service.reply(user, request.OperationID, "operation_conflict")
		}
		return user, nil
	}
	if request.Kind != "bind" && !model.SameIdentity(user.confirmed.Identity, identity) {
		return user, service.reply(user, request.OperationID, "binding_replaced")
	}
	switch request.Kind {
	case "bind":
	case "enter":
		if code := model.Admission(user.confirmed, "", uint32(businesspb.MsgId_C2S_ENTER_MOLE)); code != "ok" {
			return user, service.reply(user, request.OperationID, code)
		}
	case "leave":
		if code := model.Admission(user.confirmed, "", uint32(businesspb.MsgId_C2S_LEAVE_ROOM)); code != "ok" {
			return user, service.reply(user, request.OperationID, code)
		}
		if request.RoomID != user.confirmed.Room.RoomID {
			return user, service.reply(user, request.OperationID, "state_mismatch")
		}
	default:
		return nil, invalid
	}
	// 操作记录不淘汰；达到有界容量后拒绝新操作，防止旧 ID 重新执行。
	if len(user.outcomes) >= 128 {
		return user, service.reply(user, request.OperationID, "capacity")
	}
	user.nextVersion++
	target := model.SessionRecord{Identity: identity}
	target.Identity.LocationVersion = user.nextVersion
	operation := &positionOperation{request: request, target: target, expected: user.confirmed.Room, connected: user.connected}
	user.pending = operation
	if request.Kind == "bind" {
		operation.connected = true
	}
	return user, nil
}

func (service *sessionService) reply(user *onlineUser, operationID, code string) map[string]any {
	reply := map[string]any{"code": code, "operation_id": operationID, "pending_operation_id": "", "refreshed": false}
	if user.confirmed.Identity.UserID != "" {
		reply["confirmed"] = user.confirmed.Values()
	}
	if user.pending != nil {
		reply["pending_operation_id"] = user.pending.request.OperationID
	}
	return reply
}

func (service *sessionService) complete(user *onlineUser, roomReply model.RoomReply) map[string]any {
	operation := user.pending
	if roomReply.Code == "ok" {
		if operation.request.Kind != "leave" && roomReply.Binding.RoomID != "" {
			operation.target.Room = roomReply.Binding
			operation.target.GameKind = "mole"
			service.observe(&operation.target, roomReply.Snapshot)
		}
		user.confirmed = operation.target
		user.connected = operation.connected
	} else if model.SameBinding(user.confirmed.Room, operation.expected) {
		service.observe(&user.confirmed, roomReply.Snapshot)
	}
	user.outcomes[operation.request.OperationID] = positionOutcome{request: operation.request, code: roomReply.Code}
	user.pending = nil
	reply := service.reply(user, operation.request.OperationID, roomReply.Code)
	if roomReply.Snapshot != nil && roomReply.Snapshot["room_id"] == user.confirmed.Room.RoomID {
		reply["snapshot"] = roomReply.Snapshot
	}
	return reply
}

func (service *sessionService) drive(ctx context.Context, self gsr.CommandContext, user *onlineUser) map[string]any {
	operation := user.pending
	oldIdentity := user.confirmed.Identity
	roomReply := model.RoomReply{Code: "ok"}
	if operation.request.Kind == "enter" && operation.reservation.RoomID == "" {
		result, err := self.Cluster().Call(ctx, "mole", "rooms", command.AllocateRoom, operation.request.OperationID, operation.target.Identity.Values())
		var ok bool
		roomReply, ok = model.ReadRoomReply(result)
		if err != nil || !ok || roomReply.Code == "unavailable" {
			return service.reply(user, operation.request.OperationID, "operation_pending")
		}
		if roomReply.Code != "ok" {
			return service.complete(user, roomReply)
		}
		if roomReply.Binding.RoomID == "" || roomReply.Binding.Identity != operation.target.Identity {
			return service.reply(user, operation.request.OperationID, "operation_pending")
		}
		operation.reservation = roomReply.Binding
	}
	binding := operation.expected
	if operation.request.Kind == "enter" {
		binding = operation.reservation
	}
	if binding.RoomID != "" {
		id := command.BindRoom
		if operation.request.Kind == "leave" {
			id = command.RemoveMember
		}
		request := operation.bindingRequest(binding)
		result, err := self.Cluster().Call(ctx, "mole", cluster.ServiceName("mole/"+binding.RoomID), id, request.OperationID, request.Binding.Values(), request.Target.Values(), request.Connected)
		var ok bool
		roomReply, ok = model.ReadRoomReply(result)
		if err != nil || !ok || (roomReply.Code == "ok" && operation.request.Kind != "leave" && (roomReply.Binding.RoomID != binding.RoomID || roomReply.Binding.Identity != operation.target.Identity)) {
			return service.reply(user, operation.request.OperationID, "operation_pending")
		}
	}
	reply := service.complete(user, roomReply)
	if reply["code"] == "ok" && oldIdentity.UserID != "" && oldIdentity.LoginID != user.confirmed.Identity.LoginID {
		if err := self.Cluster().Send(cluster.NodeID(oldIdentity.AccessNode), cluster.ServiceName(oldIdentity.AgentName), command.Replaced, oldIdentity.LoginID); err != nil {
			_ = self.Logger().Error("replacement notice rejected", "error", err)
		}
	}
	if reply["code"] == "ok" && !user.connected && user.confirmed.Room.RoomID != "" {
		_ = self.Cluster().Send("mole", cluster.ServiceName("mole/"+user.confirmed.Room.RoomID), command.Disconnect, user.confirmed.Room.Values())
	}
	return reply
}

func (operation *positionOperation) bindingRequest(binding model.RoomBinding) model.RoomRequest {
	if operation.roomRequest == nil {
		// 外部效果未知时重复原参数；后续断线通过条件通知修正，不能改变同 ID 的内容。
		operation.roomRequest = &model.RoomRequest{OperationID: operation.request.OperationID, Binding: binding, Target: operation.target.Identity, Connected: operation.connected}
	}
	return *operation.roomRequest
}

func (service *sessionService) handlePosition(self gsr.CommandContext, operationID, kind string, fields map[string]any, roomID string) (map[string]any, error) {
	identity, ok := model.ReadIdentity(fields)
	if !ok {
		return nil, runtime.ErrInvalidCommandArguments
	}
	request := model.PositionCommand{OperationID: operationID, Kind: kind, Identity: identity, RoomID: roomID}
	ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
	defer cancel()
	if kind == "query" || kind == "operation" {
		user := service.users[identity.UserID]
		if user == nil {
			return map[string]any{"code": "not_authenticated"}, nil
		}
		if user.confirmed.Identity.LoginID != identity.LoginID && (user.pending == nil || user.pending.target.Identity.LoginID != identity.LoginID) {
			return map[string]any{"code": "binding_replaced"}, nil
		}
		if user.pending != nil {
			if kind == "operation" && operationID != user.pending.request.OperationID {
				return service.reply(user, operationID, "operation_unknown"), nil
			}
			return service.drive(ctx, self, user), nil
		}
		if kind == "operation" {
			outcome, ok := user.outcomes[operationID]
			if !ok || outcome.request.Identity.LoginID != identity.LoginID {
				return service.reply(user, operationID, "operation_unknown"), nil
			}
			return service.reply(user, operationID, outcome.code), nil
		}
		reply := service.reply(user, "", "ok")
		if user.confirmed.Room.RoomID != "" {
			result, err := self.Cluster().Call(ctx, "mole", cluster.ServiceName("mole/"+user.confirmed.Room.RoomID), command.RoomSnapshot, user.confirmed.Room.Values())
			roomReply, ok := model.ReadRoomReply(result)
			if err == nil && ok && roomReply.Code == "ok" {
				service.observe(&user.confirmed, roomReply.Snapshot)
				reply = service.reply(user, "", "ok")
				reply["snapshot"] = roomReply.Snapshot
				reply["refreshed"] = true
			}
		} else {
			reply["refreshed"] = true
		}
		return reply, nil
	}
	user, reply := service.begin(request)
	if reply != nil {
		return reply, nil
	}
	return service.drive(ctx, self, user), nil
}

func (service *sessionService) observe(session *model.SessionRecord, snapshot map[string]any) {
	roomID, roomOK := snapshot["room_id"].(string)
	roundID, roundOK := snapshot["round_id"].(string)
	phase, phaseOK := snapshot["phase"].(string)
	version, versionOK := snapshot["state_version"].(uint64)
	if roomOK && roundOK && phaseOK && versionOK {
		model.ObservePhase(session, roomID, roundID, phase, version)
	}
}

func (service *sessionService) handlePhase(_ gsr.CommandContext, fields map[string]any, roundID, phase string, version uint64) error {
	binding, ok := model.ReadBinding(fields)
	if !ok {
		return runtime.ErrInvalidCommandArguments
	}
	user := service.users[binding.Identity.UserID]
	if user != nil && model.SameBinding(user.confirmed.Room, binding) {
		model.ObservePhase(&user.confirmed, binding.RoomID, roundID, phase, version)
	}
	return nil
}

func (service *sessionService) handleDisconnect(self gsr.CommandContext, fields map[string]any) error {
	identity, ok := model.ReadIdentity(fields)
	if !ok {
		return runtime.ErrInvalidCommandArguments
	}
	binding := service.connectionClosed(identity)
	if binding.RoomID != "" {
		return self.Cluster().Send("mole", cluster.ServiceName("mole/"+binding.RoomID), command.Disconnect, binding.Values())
	}
	return nil
}

func (service *sessionService) connectionClosed(identity model.Identity) model.RoomBinding {
	user := service.users[identity.UserID]
	if user == nil {
		return model.RoomBinding{}
	}
	if user.pending != nil && user.pending.target.Identity.LoginID == identity.LoginID {
		user.pending.connected = false
	}
	if !model.SameIdentity(user.confirmed.Identity, identity) {
		return model.RoomBinding{}
	}
	user.connected = false
	return user.confirmed.Room
}

func (service *sessionService) handleSweep(self gsr.CommandContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	for count := 0; count < 8 && count < len(service.userIDs) && ctx.Err() == nil; count++ {
		userID := service.userIDs[service.sweepIndex%len(service.userIDs)]
		service.sweepIndex = (service.sweepIndex + 1) % len(service.userIDs)
		user := service.users[userID]
		if user.pending != nil {
			// 客户端断线也继续核对原操作；不要求旧连接活着才能解除待确认。
			service.drive(ctx, self, user)
			continue
		}
		room := user.confirmed.Room
		if room.RoomID == "" {
			continue
		}
		result, err := self.Cluster().Call(ctx, "mole", cluster.ServiceName("mole/"+room.RoomID), command.RoomSnapshot, room.Values())
		reply, ok := model.ReadRoomReply(result)
		if err != nil || !ok || reply.Code != "ok" || reply.Snapshot == nil || reply.Snapshot["phase"] == user.confirmed.RoomPhase {
			continue
		}
		service.observe(&user.confirmed, reply.Snapshot)
		if user.connected {
			if err := self.Cluster().Send(cluster.NodeID(room.Identity.AccessNode), cluster.ServiceName(room.Identity.AgentName), command.RoomState, room.Values(), reply.Snapshot); err != nil {
				_ = self.Logger().Error("phase sync rejected", "user_id", userID, "error", err)
			}
		}
	}
	_, err := self.After(time.Second, command.Sweep)
	return err
}
