package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	"github.com/cnooljaw/gsrdemo/common/model"
)

const roomTick runtime.CommandID = 100

type roomService struct {
	room       *room
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*roomService)(nil)

func newRoomService(id string, cfg model.MoleConfig) *roomService {
	service := &roomService{room: newRoom(id, cfg)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		command.ReserveSeat: service.handleReserve, command.BindRoom: service.handleBind, command.RemoveMember: service.handleRemove,
		command.RoomSnapshot: service.handleSnapshot, command.Hit: service.handleHit, command.Disconnect: service.handleDisconnect, command.Advance: service.handleAdvance, roomTick: service.handleTick,
	})
	return service
}

func (service *roomService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(100*time.Millisecond, roomTick)
	return err
}

func (service *roomService) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, id, args...)
}

func (service *roomService) OnStop(_ context.Context) error {
	return nil
}

// ReserveSeat 仅由同节点 Directory 调用，普通 Go 值拷贝无需编码。
func (service *roomService) handleReserve(_ gsr.CommandContext, request allocateRequest) (model.RoomReply, error) {
	return service.room.reserve(request), nil
}

func (service *roomService) handleBind(self gsr.CommandContext, operationID string, bindingFields, targetFields map[string]any, connected bool) (map[string]any, error) {
	binding, bindingOK := model.ReadBinding(bindingFields)
	target, targetOK := model.ReadIdentity(targetFields)
	if !bindingOK || !targetOK {
		return nil, runtime.ErrInvalidCommandArguments
	}
	version, phase := service.room.version, service.room.phase
	reply := service.room.bind(model.RoomRequest{OperationID: operationID, Binding: binding, Target: target, Connected: connected}, time.Now())
	service.publish(self, version, phase)
	return reply.Values(), nil
}

func (service *roomService) handleRemove(self gsr.CommandContext, operationID string, bindingFields, targetFields map[string]any, connected bool) (map[string]any, error) {
	binding, bindingOK := model.ReadBinding(bindingFields)
	target, targetOK := model.ReadIdentity(targetFields)
	if !bindingOK || !targetOK {
		return nil, runtime.ErrInvalidCommandArguments
	}
	version, phase := service.room.version, service.room.phase
	reply := service.room.remove(model.RoomRequest{OperationID: operationID, Binding: binding, Target: target, Connected: connected}, time.Now())
	service.publish(self, version, phase)
	return reply.Values(), nil
}

func (service *roomService) handleSnapshot(self gsr.CommandContext, fields map[string]any) (map[string]any, error) {
	binding, ok := model.ReadBinding(fields)
	if !ok {
		return nil, runtime.ErrInvalidCommandArguments
	}
	version, phase := service.room.version, service.room.phase
	service.room.advance(time.Now())
	service.publish(self, version, phase)
	return (model.RoomReply{Code: service.room.check(binding), Snapshot: service.room.snapshot()}).Values(), nil
}

func (service *roomService) handleHit(self gsr.CommandContext, fields map[string]any, operationID, roomID, roundID string, instanceID uint64) (map[string]any, error) {
	binding, ok := model.ReadBinding(fields)
	if !ok {
		return nil, runtime.ErrInvalidCommandArguments
	}
	version, phase := service.room.version, service.room.phase
	reply := service.room.hit(hitRequest{Binding: binding, OperationID: operationID, RoomID: roomID, RoundID: roundID, InstanceID: instanceID}, time.Now())
	service.publish(self, version, phase)
	return reply.Values(), nil
}

func (service *roomService) handleDisconnect(self gsr.CommandContext, fields map[string]any) error {
	binding, ok := model.ReadBinding(fields)
	if !ok {
		return runtime.ErrInvalidCommandArguments
	}
	version, phase := service.room.version, service.room.phase
	service.room.disconnect(binding, time.Now())
	service.publish(self, version, phase)
	return nil
}

func (service *roomService) handleAdvance(self gsr.CommandContext) error {
	version, phase := service.room.version, service.room.phase
	service.room.advance(time.Now())
	service.publish(self, version, phase)
	return nil
}

func (service *roomService) handleTick(self gsr.CommandContext) error {
	if err := service.handleAdvance(self); err != nil {
		return err
	}
	// 巡检不登记房间 Timer，保证最多一条自用唤醒链。
	if service.room.phase != "ended" {
		_, err := self.After(100*time.Millisecond, roomTick)
		return err
	}
	return nil
}

func (service *roomService) publish(self gsr.CommandContext, previousVersion uint64, previousPhase string) {
	if service.room.version == previousVersion {
		return
	}
	for _, member := range service.room.orderedMembers() {
		binding := member.binding
		if member.connected {
			if err := self.Cluster().Send(cluster.NodeID(binding.Identity.AccessNode), cluster.ServiceName(binding.Identity.AgentName), command.RoomState, binding.Values(), service.room.snapshot()); err != nil {
				_ = self.Logger().Error("room push rejected", "room_id", service.room.id, "user_id", member.userID, "error", err)
			}
		}
		if service.room.phase != previousPhase {
			if err := self.Cluster().Send("online", "sessions", command.RoomPhase, binding.Values(), service.room.id+"/1", service.room.phase, service.room.version); err != nil {
				_ = self.Logger().Error("room phase rejected", "room_id", service.room.id, "error", err)
			}
		}
	}
}
