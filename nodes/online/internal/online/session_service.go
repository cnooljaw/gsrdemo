package online

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
	"google.golang.org/protobuf/proto"
)

type positionOperation struct {
	request     *businesspb.PositionCommand
	target      *businesspb.SessionRecord
	expected    *businesspb.RoomBinding
	reservation *businesspb.RoomBinding
	connected   bool
	roomRequest *businesspb.RoomRequest
}
type positionOutcome struct {
	request *businesspb.PositionCommand
	code    string
}
type onlineUser struct {
	confirmed   *businesspb.SessionRecord
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

func (service *sessionService) begin(request *businesspb.PositionCommand) (*onlineUser, *businesspb.PositionReply) {
	invalid := &businesspb.PositionReply{Code: "invalid", OperationId: request.OperationId}
	identity := request.GetIdentity()
	if !model.ValidOperationID(request.OperationId) || identity.GetUserId() == "" || identity.GetLoginId() == "" || identity.GetAccessNode() == "" || identity.GetAgentName() == "" {
		return nil, invalid
	}
	user := service.users[identity.UserId]
	if user == nil {
		if request.Kind != "bind" {
			return nil, &businesspb.PositionReply{Code: "not_authenticated"}
		}
		if len(service.users) >= 512 {
			return nil, &businesspb.PositionReply{Code: "capacity"}
		}
		user = &onlineUser{outcomes: make(map[string]positionOutcome)}
		service.users[identity.UserId] = user
		service.userIDs = append(service.userIDs, identity.UserId)
	}
	if previous, ok := user.outcomes[request.OperationId]; ok {
		code := previous.code
		if !proto.Equal(previous.request, request) {
			code = "operation_conflict"
		}
		if user.confirmed.GetIdentity().GetLoginId() != identity.LoginId {
			code = "binding_replaced"
		}
		return user, service.reply(user, request.OperationId, code)
	}
	if user.pending != nil {
		if user.pending.request.OperationId != request.OperationId {
			code := "operation_pending"
			if user.pending.target.Identity.LoginId != identity.LoginId {
				code = "user_busy"
			}
			return user, service.reply(user, request.OperationId, code)
		}
		if !proto.Equal(user.pending.request, request) {
			return user, service.reply(user, request.OperationId, "operation_conflict")
		}
		return user, nil
	}
	if request.Kind != "bind" && !model.SameIdentity(user.confirmed.GetIdentity(), identity) {
		return user, service.reply(user, request.OperationId, "binding_replaced")
	}
	switch request.Kind {
	case "bind":
	case "enter":
		if code := model.Admission(user.confirmed, "", uint32(businesspb.MsgId_C2S_ENTER_MOLE)); code != "ok" {
			return user, service.reply(user, request.OperationId, code)
		}
	case "leave":
		if code := model.Admission(user.confirmed, "", uint32(businesspb.MsgId_C2S_LEAVE_ROOM)); code != "ok" {
			return user, service.reply(user, request.OperationId, code)
		}
		if request.RoomId != user.confirmed.GetRoom().GetRoomId() {
			return user, service.reply(user, request.OperationId, "state_mismatch")
		}
	default:
		return nil, invalid
	}
	// 操作记录不淘汰；达到有界容量后拒绝新操作，防止旧 ID 重新执行。
	if len(user.outcomes) >= 128 {
		return user, service.reply(user, request.OperationId, "capacity")
	}
	user.nextVersion++
	target := &businesspb.SessionRecord{Identity: proto.Clone(identity).(*businesspb.Identity)}
	target.Identity.LocationVersion = user.nextVersion
	operation := &positionOperation{request: proto.Clone(request).(*businesspb.PositionCommand), target: target, connected: user.connected}
	if user.confirmed.GetRoom() != nil {
		operation.expected = proto.Clone(user.confirmed.Room).(*businesspb.RoomBinding)
	}
	user.pending = operation
	if request.Kind == "bind" {
		operation.connected = true
	}
	return user, nil
}

func (service *sessionService) reply(user *onlineUser, operationID, code string) *businesspb.PositionReply {
	reply := &businesspb.PositionReply{Code: code, OperationId: operationID}
	if user.confirmed != nil {
		reply.Confirmed = proto.Clone(user.confirmed).(*businesspb.SessionRecord)
	}
	if user.pending != nil {
		reply.PendingOperationId = user.pending.request.OperationId
	}
	return reply
}

func (service *sessionService) complete(user *onlineUser, roomReply *businesspb.RoomReply) *businesspb.PositionReply {
	operation := user.pending
	if roomReply.Code == "ok" {
		if operation.request.Kind != "leave" && roomReply.Binding != nil {
			operation.target.Room = proto.Clone(roomReply.Binding).(*businesspb.RoomBinding)
			operation.target.Snapshot = proto.Clone(roomReply.Snapshot).(*businesspb.MoleSnapshot)
		}
		user.confirmed = operation.target
		user.connected = operation.connected
	} else if model.SameBinding(user.confirmed.GetRoom(), operation.expected) {
		service.observe(user, roomReply.Snapshot)
	}
	user.outcomes[operation.request.OperationId] = positionOutcome{request: operation.request, code: roomReply.Code}
	user.pending = nil
	return service.reply(user, operation.request.OperationId, roomReply.Code)
}

func (service *sessionService) drive(ctx context.Context, self gsr.CommandContext, user *onlineUser) *businesspb.PositionReply {
	operation := user.pending
	var oldIdentity *businesspb.Identity
	if user.confirmed != nil {
		oldIdentity = proto.Clone(user.confirmed.Identity).(*businesspb.Identity)
	}
	roomReply := &businesspb.RoomReply{Code: "ok"}
	if operation.request.Kind == "enter" && operation.reservation == nil {
		err := command.Call(ctx, self, "mole", "rooms", command.AllocateRoom, &businesspb.AllocateRequest{OperationId: operation.request.OperationId, Identity: operation.target.Identity}, roomReply)
		if err != nil || roomReply.Code == "unavailable" {
			return service.reply(user, operation.request.OperationId, "operation_pending")
		}
		if roomReply.Code != "ok" {
			return service.complete(user, roomReply)
		}
		if roomReply.Binding == nil {
			return service.reply(user, operation.request.OperationId, "operation_pending")
		}
		operation.reservation = proto.Clone(roomReply.Binding).(*businesspb.RoomBinding)
	}
	binding := operation.expected
	if operation.request.Kind == "enter" {
		binding = operation.reservation
	}
	if binding != nil {
		id := command.BindRoom
		if operation.request.Kind == "leave" {
			id = command.RemoveMember
		}
		request := operation.bindingRequest(binding)
		err := command.Call(ctx, self, "mole", "mole/"+binding.RoomId, id, request, roomReply)
		if err != nil {
			return service.reply(user, operation.request.OperationId, "operation_pending")
		}
	}
	reply := service.complete(user, roomReply)
	if reply.Code == "ok" && oldIdentity != nil && oldIdentity.LoginId != user.confirmed.Identity.LoginId {
		if err := command.Send(self, oldIdentity.AccessNode, oldIdentity.AgentName, command.Replaced, &businesspb.ReplacedNotice{LoginId: oldIdentity.LoginId}); err != nil {
			_ = self.Logger().Error("replacement notice rejected", "error", err)
		}
	}
	if reply.Code == "ok" && !user.connected && user.confirmed.Room != nil {
		_ = command.Send(self, "mole", "mole/"+user.confirmed.Room.RoomId, command.Disconnect, user.confirmed.Room)
	}
	return reply
}

func (operation *positionOperation) bindingRequest(binding *businesspb.RoomBinding) *businesspb.RoomRequest {
	if operation.roomRequest == nil {
		// 外部效果未知时重复原载荷；后续断线通过条件通知修正，不能改变同 ID 的内容。
		operation.roomRequest = &businesspb.RoomRequest{OperationId: operation.request.OperationId, Binding: proto.Clone(binding).(*businesspb.RoomBinding), Target: proto.Clone(operation.target.Identity).(*businesspb.Identity), Connected: operation.connected}
	}
	return operation.roomRequest
}

func (service *sessionService) handlePosition(self gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.PositionCommand{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
	defer cancel()
	if request.Kind == "query" || request.Kind == "operation" {
		user := service.users[request.GetIdentity().GetUserId()]
		if user == nil {
			return proto.Marshal(&businesspb.PositionReply{Code: "not_authenticated"})
		}
		loginID := request.GetIdentity().GetLoginId()
		if loginID == "" || (user.confirmed.GetIdentity().GetLoginId() != loginID && (user.pending == nil || user.pending.target.Identity.LoginId != loginID)) {
			return proto.Marshal(&businesspb.PositionReply{Code: "binding_replaced"})
		}
		if user.pending != nil {
			if request.Kind == "operation" && request.OperationId != user.pending.request.OperationId {
				return proto.Marshal(service.reply(user, request.OperationId, "operation_unknown"))
			}
			return proto.Marshal(service.drive(ctx, self, user))
		}
		if request.Kind == "operation" {
			outcome, ok := user.outcomes[request.OperationId]
			if !ok || outcome.request.Identity.LoginId != loginID {
				return proto.Marshal(service.reply(user, request.OperationId, "operation_unknown"))
			}
			return proto.Marshal(service.reply(user, request.OperationId, outcome.code))
		}
		reply := service.reply(user, "", "ok")
		if user.confirmed.Room != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			roomReply := &businesspb.RoomReply{}
			err := command.Call(ctx, self, "mole", "mole/"+user.confirmed.Room.RoomId, command.RoomSnapshot, user.confirmed.Room, roomReply)
			if err == nil && roomReply.Code == "ok" {
				service.observe(user, roomReply.Snapshot)
				reply = service.reply(user, "", "ok")
				reply.Refreshed = true
			}
		} else {
			reply.Refreshed = true
		}
		return proto.Marshal(reply)
	}
	user, reply := service.begin(request)
	if reply != nil {
		return proto.Marshal(reply)
	}
	return proto.Marshal(service.drive(ctx, self, user))
}

func (service *sessionService) observe(user *onlineUser, snapshot *businesspb.MoleSnapshot) {
	if snapshot == nil || snapshot.RoomId != user.confirmed.GetRoom().GetRoomId() || snapshot.StateVersion <= user.confirmed.GetSnapshot().GetStateVersion() {
		return
	}
	user.confirmed.Snapshot = proto.Clone(snapshot).(*businesspb.MoleSnapshot)
}

func (service *sessionService) handlePhase(_ gsr.CommandContext, payload []byte) error {
	notice := &businesspb.RoomNotice{}
	if err := proto.Unmarshal(payload, notice); err != nil {
		return err
	}
	user := service.users[notice.GetBinding().GetIdentity().GetUserId()]
	if user != nil && model.SameBinding(user.confirmed.GetRoom(), notice.Binding) {
		service.observe(user, notice.Snapshot)
	}
	return nil
}

func (service *sessionService) handleDisconnect(self gsr.CommandContext, payload []byte) error {
	identity := &businesspb.Identity{}
	if err := proto.Unmarshal(payload, identity); err != nil {
		return err
	}
	binding := service.connectionClosed(identity)
	if binding != nil {
		return command.Send(self, "mole", "mole/"+binding.RoomId, command.Disconnect, binding)
	}
	return nil
}

func (service *sessionService) connectionClosed(identity *businesspb.Identity) *businesspb.RoomBinding {
	user := service.users[identity.GetUserId()]
	if user == nil {
		return nil
	}
	if user.pending != nil && user.pending.target.Identity.LoginId == identity.LoginId {
		user.pending.connected = false
	}
	if !model.SameIdentity(user.confirmed.GetIdentity(), identity) {
		return nil
	}
	user.connected = false
	if user.confirmed.Room != nil {
		return proto.Clone(user.confirmed.Room).(*businesspb.RoomBinding)
	}
	return nil
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
		if user.confirmed.GetRoom() == nil {
			continue
		}
		room := user.confirmed.Room
		reply := &businesspb.RoomReply{}
		err := command.Call(ctx, self, "mole", "mole/"+room.RoomId, command.RoomSnapshot, room, reply)
		if err != nil || reply.Code != "ok" || reply.Snapshot == nil || reply.Snapshot.Phase == user.confirmed.GetSnapshot().GetPhase() {
			continue
		}
		service.observe(user, reply.Snapshot)
		if user.connected {
			notice := &businesspb.RoomNotice{Binding: room, Snapshot: user.confirmed.Snapshot}
			if err := command.Send(self, room.Identity.AccessNode, room.Identity.AgentName, command.RoomState, notice); err != nil {
				_ = self.Logger().Error("phase sync rejected", "user_id", userID, "error", err)
			}
		}
	}
	_, err := self.After(time.Second, command.Sweep)
	return err
}
