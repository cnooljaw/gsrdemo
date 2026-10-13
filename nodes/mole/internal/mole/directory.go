package mole

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	"github.com/cnooljaw/gsrdemo/common/model"
)

type allocation struct {
	request allocateRequest
	reply   model.RoomReply
	roomID  string
}
type directory struct {
	cfg         model.MoleConfig
	dispatcher  *gsr.Dispatcher
	waiting     string
	rooms       []string
	sweepIndex  int
	allocations map[string]allocation
}

var _ gsr.Service = (*directory)(nil)

func newDirectory(cfg model.MoleConfig) *directory {
	service := &directory{cfg: cfg, allocations: make(map[string]allocation)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{command.AllocateRoom: service.handleAllocate, command.Sweep: service.handleSweep})
	return service
}

func (service *directory) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(200*time.Millisecond, command.Sweep)
	return err
}

func (service *directory) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, id, args...)
}

func (service *directory) OnStop(_ context.Context) error {
	return nil
}

func (service *directory) handleAllocate(self gsr.CommandContext, operationID string, fields map[string]any) (map[string]any, error) {
	identity, ok := model.ReadIdentity(fields)
	if !ok || !model.ValidOperationID(operationID) {
		return (model.RoomReply{Code: "invalid"}).Values(), nil
	}
	request := allocateRequest{OperationID: operationID, Identity: identity}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	key := request.Identity.UserID + "/" + request.OperationID
	if previous, ok := service.allocations[key]; ok {
		if previous.request != request {
			return (model.RoomReply{Code: "operation_conflict"}).Values(), nil
		}
		if previous.reply.Code != "" {
			return previous.reply.Values(), nil
		}
		reply, err := service.reserve(ctx, self, key, previous)
		if err != nil {
			return nil, err
		}
		return reply.Values(), nil
	}
	if len(service.allocations) >= 1024 {
		return (model.RoomReply{Code: "capacity"}).Values(), nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		if service.waiting == "" {
			// 本切片未保存结果，结束房间仍保留冻结记录；上限到达后拒绝新组。
			if len(service.rooms) >= 64 {
				return (model.RoomReply{Code: "capacity"}).Values(), nil
			}
			roomID, err := model.NewID()
			if err != nil {
				return nil, err
			}
			id, err := self.CreateService(ctx, newRoomService(roomID, service.cfg))
			if err != nil {
				return nil, err
			}
			if err := self.Register(cluster.ServiceName("mole/"+roomID), id); err != nil {
				return nil, err
			}
			service.waiting = roomID
			service.rooms = append(service.rooms, roomID)
		}
		pending := allocation{request: request, roomID: service.waiting}
		// 先记目标再等待，未知结果重试必须仍去原房间。
		service.allocations[key] = pending
		reply, err := service.reserve(ctx, self, key, pending)
		if err != nil {
			return nil, err
		}
		if reply.Code == "room_full" {
			delete(service.allocations, key)
			service.waiting = ""
			continue
		}
		return reply.Values(), nil
	}
	return (model.RoomReply{Code: "capacity"}).Values(), nil
}

func (service *directory) reserve(ctx context.Context, self gsr.CommandContext, key string, pending allocation) (model.RoomReply, error) {
	request := pending.request
	result, err := self.CallName(ctx, cluster.ServiceName("mole/"+pending.roomID), command.ReserveSeat, request)
	if err != nil {
		return model.RoomReply{}, err
	}
	reply, ok := result.(model.RoomReply)
	if !ok || reply.Code == "" {
		return model.RoomReply{Code: "unavailable"}, nil
	}
	pending.reply = reply
	service.allocations[key] = pending
	return reply, nil
}

func (service *directory) handleSweep(self gsr.CommandContext) error {
	// 使用独立控制 Mailbox 有界巡检，单房间 Timer admission 丢失仍可结束。
	for count := 0; count < 16 && count < len(service.rooms); count++ {
		roomID := service.rooms[service.sweepIndex%len(service.rooms)]
		service.sweepIndex = (service.sweepIndex + 1) % len(service.rooms)
		if err := self.SendName(cluster.ServiceName("mole/"+roomID), command.Advance); err != nil {
			_ = self.Logger().Error("room sweep rejected", "room_id", roomID, "error", err)
		}
	}
	_, err := self.After(200*time.Millisecond, command.Sweep)
	return err
}
