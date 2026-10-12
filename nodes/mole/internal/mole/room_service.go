package mole

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

func (service *roomService) handleReserve(_ gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.AllocateRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	return proto.Marshal(service.room.reserve(request))
}

func (service *roomService) handleBind(self gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.RoomRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	version, phase := service.room.version, service.room.phase
	reply := service.room.bind(request, time.Now())
	service.publish(self, version, phase)
	return proto.Marshal(reply)
}

func (service *roomService) handleRemove(self gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.RoomRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	version, phase := service.room.version, service.room.phase
	reply := service.room.remove(request, time.Now())
	service.publish(self, version, phase)
	return proto.Marshal(reply)
}

func (service *roomService) handleSnapshot(self gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.RoomBinding{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	version, phase := service.room.version, service.room.phase
	service.room.advance(time.Now())
	service.publish(self, version, phase)
	return proto.Marshal(&businesspb.RoomReply{Code: service.room.check(request), Snapshot: service.room.snapshot()})
}

func (service *roomService) handleHit(self gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &businesspb.HitCommand{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, err
	}
	version, phase := service.room.version, service.room.phase
	reply := service.room.hit(request, time.Now())
	service.publish(self, version, phase)
	return proto.Marshal(reply)
}

func (service *roomService) handleDisconnect(self gsr.CommandContext, payload []byte) error {
	binding := &businesspb.RoomBinding{}
	if err := proto.Unmarshal(payload, binding); err != nil {
		return err
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
	snapshot := service.room.snapshot()
	for _, member := range service.room.orderedMembers() {
		notice := &businesspb.RoomNotice{Binding: member.binding, Snapshot: snapshot}
		if member.connected {
			if err := command.Send(self, member.binding.Identity.AccessNode, member.binding.Identity.AgentName, command.RoomState, notice); err != nil {
				_ = self.Logger().Error("room push rejected", "room_id", service.room.id, "user_id", member.userID, "error", err)
			}
		}
		if service.room.phase != previousPhase {
			if err := command.Send(self, "online", "sessions", command.RoomPhase, notice); err != nil {
				_ = self.Logger().Error("room phase rejected", "room_id", service.room.id, "error", err)
			}
		}
	}
}
