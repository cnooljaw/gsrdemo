package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

const landlordPushCommand runtime.CommandID = 110

func (agent *agent) onLandlordEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordEnterRequest) error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	if agent.attackName != "" {
		return errEnterRequired
	}
	for attempt := 0; attempt < 2; attempt++ {
		value, err := agent.call(self, "landlord", "landlord", 1, agent.userID, agent.loginID)
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
		value, err = agent.call(self, "landlord", cluster.ServiceName(room), 1, agent.userID, agent.loginID, string(agent.node), string(agent.name), membership)
		if err != nil {
			// 只有明确房间消失才丢弃目录；超时可能已加入，不能随机换桌。
			if errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed) || errors.Is(err, runtime.ErrServicePanicked) {
				if _, discardErr := agent.call(self, "landlord", "landlord", 13, room); discardErr != nil {
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
		agent.landlordRoom = cluster.ServiceName(room)
		return landlordResponse(reply, demopb.MsgId_S2C_LANDLORD_ENTER, snapshot)
	}
	return runtime.ErrServiceNotFound
}

func (agent *agent) landlordCall(self gsr.CommandContext, reply protocol.Context, command runtime.CommandID, responseID demopb.MsgId, args ...any) error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	if agent.landlordRoom == "" {
		return errEnterRequired
	}
	values := append([]any{agent.userID, agent.loginID}, args...)
	value, err := agent.call(self, "landlord", agent.landlordRoom, command, values...)
	if err != nil {
		return err
	}
	snapshot, err := landlordSnapshot(value)
	if err != nil {
		return err
	}
	return landlordResponse(reply, responseID, snapshot)
}

func (agent *agent) onLandlordReady(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordReadyRequest) error {
	return agent.landlordCall(self, reply, 2, demopb.MsgId_S2C_LANDLORD_READY)
}

func (agent *agent) onLandlordPlay(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordPlayRequest) error {
	cards := make([]any, len(request.Cards))
	for i, c := range request.Cards {
		cards[i] = c
	}
	return agent.landlordCall(self, reply, 3, demopb.MsgId_S2C_LANDLORD_PLAY, request.Turn, cards)
}

func (agent *agent) onLandlordSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordSnapshotRequest) error {
	return agent.landlordCall(self, reply, 4, demopb.MsgId_S2C_LANDLORD_SNAPSHOT)
}

func (agent *agent) onLandlordLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordLeaveRequest) error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	if agent.landlordRoom == "" {
		return errEnterRequired
	}
	value, err := agent.call(self, "landlord", agent.landlordRoom, 5, agent.userID, agent.loginID)
	if err != nil {
		return err
	}
	if value != true {
		return errInvalidBackendReply
	}
	agent.landlordRoom = ""
	return reply.Response(uint32(demopb.MsgId_S2C_LANDLORD_LEAVE), &demopb.LandlordLeaveResponse{})
}

func (agent *agent) onLandlordOperation(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordOperationRequest) error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	value, err := agent.call(self, "settlement", cluster.ServiceName(routing.ReadService(request.OperationId)), 3, agent.userID, request.OperationId)
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

func (agent *agent) handleLandlordPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if agent.userID == 0 || login != agent.loginID || agent.landlordRoom == "" {
		return nil
	}
	snapshot, err := landlordSnapshot(payload)
	if err != nil {
		return err
	}
	if cluster.ServiceName(snapshot.RoomName) != agent.landlordRoom {
		return nil
	}
	return agent.protocolDispatcher.Push(uint32(demopb.MsgId_S2C_LANDLORD_PUSH), &demopb.LandlordPush{Snapshot: snapshot})
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

func (agent *agent) onLandlordCall(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordCallRequest) error {
	return agent.landlordCall(self, reply, 6, demopb.MsgId_S2C_LANDLORD_CALL, request.Turn, request.Yes)
}

func (agent *agent) onLandlordRob(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordRobRequest) error {
	return agent.landlordCall(self, reply, 7, demopb.MsgId_S2C_LANDLORD_ROB, request.Turn, request.Yes)
}
