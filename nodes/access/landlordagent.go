package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// landlordAgent 只持有本游戏接入路由，由所属 Agent 串行调用，不保存父 Agent 或回调上下文。
type landlordAgent struct {
	identity   func() agentSession
	allowEnter func(string) error
	push       func(uint32, proto.Message) error
	room       cluster.ServiceName
}

const landlordPushCommand runtime.CommandID = 110

func (landlord *landlordAgent) entered() bool {
	return landlord.room != ""
}

func (landlord *landlordAgent) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordEnterRequest) error {
	identity := landlord.identity()
	if err := landlord.allowEnter("landlord"); err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		value, err := callBackend(self, "landlord", "landlord", 1, identity.userID, identity.loginID)
		if err != nil {
			return err
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return errInvalidBackendReply
		}
		room, roomOK := fields["room_name"].(string)
		membership, memberOK := fields["membership"].(string)
		if !roomOK || !memberOK || room == "" || membership == "" {
			return errInvalidBackendReply
		}
		value, err = callBackend(self, "landlord", cluster.ServiceName(room), 1, identity.userID, identity.loginID, string(identity.node), string(identity.name), membership)
		if err != nil {
			// 只有明确房间消失才丢弃目录；超时可能已加入，不能随机换桌。
			if errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed) || errors.Is(err, runtime.ErrServicePanicked) {
				if _, discardErr := callBackend(self, "landlord", "landlord", 13, room); discardErr != nil {
					return discardErr
				}
				continue
			}
			return err
		}
		snapshot, err := landlordSnapshot(value)
		if err != nil {
			return err
		}
		landlord.room = cluster.ServiceName(room)
		return landlordResponse(reply, demopb.MsgId_S2C_LANDLORD_ENTER, snapshot)
	}
	return runtime.ErrServiceNotFound
}

func (landlord *landlordAgent) callRoom(self gsr.CommandContext, reply protocol.Context, command runtime.CommandID, responseID demopb.MsgId, args ...any) error {
	identity := landlord.identity()
	if err := identity.requireLogin(); err != nil {
		return err
	}
	if landlord.room == "" {
		return errEnterRequired
	}
	values := append([]any{identity.userID, identity.loginID}, args...)
	value, err := callBackend(self, "landlord", landlord.room, command, values...)
	if err != nil {
		return err
	}
	snapshot, err := landlordSnapshot(value)
	if err != nil {
		return err
	}
	return landlordResponse(reply, responseID, snapshot)
}

func (landlord *landlordAgent) onReady(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordReadyRequest) error {
	return landlord.callRoom(self, reply, 2, demopb.MsgId_S2C_LANDLORD_READY)
}

func (landlord *landlordAgent) onPlay(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordPlayRequest) error {
	cards := make([]any, len(request.Cards))
	for i, c := range request.Cards {
		cards[i] = c
	}
	return landlord.callRoom(self, reply, 3, demopb.MsgId_S2C_LANDLORD_PLAY, request.Turn, cards)
}

func (landlord *landlordAgent) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordSnapshotRequest) error {
	return landlord.callRoom(self, reply, 4, demopb.MsgId_S2C_LANDLORD_SNAPSHOT)
}

func (landlord *landlordAgent) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordLeaveRequest) error {
	identity := landlord.identity()
	if err := identity.requireLogin(); err != nil {
		return err
	}
	if landlord.room == "" {
		return errEnterRequired
	}
	value, err := callBackend(self, "landlord", landlord.room, 5, identity.userID, identity.loginID)
	if err != nil {
		return err
	}
	if value != true {
		return errInvalidBackendReply
	}
	landlord.room = ""
	return reply.Response(uint32(demopb.MsgId_S2C_LANDLORD_LEAVE), &demopb.LandlordLeaveResponse{})
}

func (landlord *landlordAgent) onOperation(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordOperationRequest) error {
	identity := landlord.identity()
	if err := identity.requireLogin(); err != nil {
		return err
	}
	value, err := callBackend(self, "settlement", cluster.ServiceName(routing.ReadService(request.OperationId)), 3, identity.userID, request.OperationId)
	if err != nil {
		return err
	}
	response := &demopb.LandlordOperationResponse{OperationId: request.OperationId, Found: value != nil}
	if value != nil {
		fields, ok := value.(map[string]any)
		if !ok || fields["operation_id"] != request.OperationId {
			return errInvalidBackendReply
		}
		status, statusOK := fields["status"].(string)
		reason, reasonOK := fields["reason"].(string)
		if !statusOK || !reasonOK || (status != "committed" && status != "rejected") {
			return errInvalidBackendReply
		}
		response.Status, response.Reason = status, reason
	}
	return reply.Response(uint32(demopb.MsgId_S2C_LANDLORD_OPERATION), response)
}

func landlordSnapshot(value any) (*demopb.LandlordSnapshot, error) {
	snapshot := &demopb.LandlordSnapshot{}
	if err := clientMessage(value, snapshot); err != nil {
		return nil, err
	}
	if snapshot.RoomName == "" {
		return nil, errInvalidBackendReply
	}
	return snapshot, nil
}

func (landlord *landlordAgent) handlePush(_ gsr.CommandContext, login string, payload map[string]any) error {
	identity := landlord.identity()
	if identity.userID == 0 || login != identity.loginID || landlord.room == "" {
		return nil
	}
	snapshot, err := landlordSnapshot(payload)
	if err != nil {
		return err
	}
	if cluster.ServiceName(snapshot.RoomName) != landlord.room {
		return nil
	}
	return landlord.push(uint32(demopb.MsgId_S2C_LANDLORD_PUSH), &demopb.LandlordPush{Snapshot: snapshot})
}

func landlordResponse(reply protocol.Context, id demopb.MsgId, snapshot *demopb.LandlordSnapshot) error {
	switch id {
	case demopb.MsgId_S2C_LANDLORD_ENTER:
		return reply.Response(uint32(id), &demopb.LandlordEnterResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_READY:
		return reply.Response(uint32(id), &demopb.LandlordReadyResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_PLAY:
		return reply.Response(uint32(id), &demopb.LandlordPlayResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_SNAPSHOT:
		return reply.Response(uint32(id), &demopb.LandlordSnapshotResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_CALL:
		return reply.Response(uint32(id), &demopb.LandlordCallResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_ROB:
		return reply.Response(uint32(id), &demopb.LandlordRobResponse{Snapshot: snapshot})
	}
	return errInvalidBackendReply
}

func (landlord *landlordAgent) onCall(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordCallRequest) error {
	return landlord.callRoom(self, reply, 6, demopb.MsgId_S2C_LANDLORD_CALL, request.Turn, request.Yes)
}

func (landlord *landlordAgent) onRob(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordRobRequest) error {
	return landlord.callRoom(self, reply, 7, demopb.MsgId_S2C_LANDLORD_ROB, request.Turn, request.Yes)
}

func (landlord *landlordAgent) protocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_LANDLORD_ENTER):     landlord.onEnter,
		uint32(demopb.MsgId_C2S_LANDLORD_READY):     landlord.onReady,
		uint32(demopb.MsgId_C2S_LANDLORD_CALL):      landlord.onCall,
		uint32(demopb.MsgId_C2S_LANDLORD_ROB):       landlord.onRob,
		uint32(demopb.MsgId_C2S_LANDLORD_PLAY):      landlord.onPlay,
		uint32(demopb.MsgId_C2S_LANDLORD_SNAPSHOT):  landlord.onSnapshot,
		uint32(demopb.MsgId_C2S_LANDLORD_LEAVE):     landlord.onLeave,
		uint32(demopb.MsgId_C2S_LANDLORD_OPERATION): landlord.onOperation,
	}
}

func (landlord *landlordAgent) commandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		landlordPushCommand: landlord.handlePush,
	}
}

func (landlord *landlordAgent) disconnect(self gsr.ServiceContext) error {
	defer func() { landlord.room = "" }()
	if landlord.room == "" {
		return nil
	}
	identity := landlord.identity()
	_, err := callBackend(self, "landlord", landlord.room, 5, identity.userID, identity.loginID)
	return err
}
