package access

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
)

func (service *agent) observeMole(fields map[string]any) bool {
	snapshot, ok := moleView(fields)
	if !ok || service.session.GameKind != "mole" || snapshot.RoomId != service.session.Room.RoomID {
		return false
	}
	if snapshot.StateVersion <= service.moleSnapshot.GetStateVersion() || snapshot.StateVersion < service.session.RoomStateVersion {
		return false
	}
	// 该客户端视图由 access 新建，安装后只替换，不再修改它的字段。
	service.moleSnapshot = snapshot
	model.ObservePhase(&service.session, snapshot.RoomId, snapshot.RoundId, snapshot.Phase, snapshot.StateVersion)
	return true
}

func (service *agent) handleSnapshot(self gsr.CommandContext, client protocol.Context, _ *businesspb.MoleSnapshotRequest) error {
	code := model.Admission(service.session, service.pendingID(), uint32(businesspb.MsgId_C2S_MOLE_SNAPSHOT))
	if code == "ok" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		code = service.refreshMole(ctx, self)
	}
	return client.Response(uint32(businesspb.MsgId_S2C_MOLE_SNAPSHOT), &businesspb.MoleSnapshotResponse{Code: code, Snapshot: service.moleSnapshot})
}

// refreshMole 用于主动同步和位置操作恢复，共用调用方的总等待预算。
func (service *agent) refreshMole(ctx context.Context, self gsr.CommandContext) string {
	binding := service.session.Room
	value, err := self.Cluster().Call(ctx, "mole", cluster.ServiceName("mole/"+binding.RoomID), command.RoomSnapshot, binding.Values())
	reply, ok := model.ReadRoomReply(value)
	if err != nil || !ok {
		return "unavailable"
	}
	if reply.Code == "ok" {
		service.observeMole(reply.Snapshot)
	}
	return reply.Code
}

func (service *agent) handleHit(self gsr.CommandContext, client protocol.Context, request *businesspb.MoleHitRequest) error {
	reply := &businesspb.MoleHitResponse{Code: model.Admission(service.session, service.pendingID(), uint32(businesspb.MsgId_C2S_MOLE_HIT)), OperationId: request.OperationId}
	if reply.Code == "ok" && request.RoomId != service.session.Room.RoomID {
		reply.Code = "state_mismatch"
	}
	if reply.Code == "ok" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		binding := service.session.Room
		value, err := self.Cluster().Call(ctx, "mole", cluster.ServiceName("mole/"+binding.RoomID), command.Hit, binding.Values(), request.OperationId, request.RoomId, request.RoundId, request.InstanceId)
		roomReply, ok := model.ReadRoomReply(value)
		if err != nil || !ok {
			reply.Code = "unavailable"
		} else {
			reply.Code = roomReply.Code
			service.observeMole(roomReply.Snapshot)
		}
	}
	reply.Snapshot = service.moleSnapshot
	return client.Response(uint32(businesspb.MsgId_S2C_MOLE_HIT), reply)
}

func (service *agent) handleRoomState(self gsr.CommandContext, bindingFields, snapshot map[string]any) error {
	binding, ok := model.ReadBinding(bindingFields)
	if !ok {
		return runtime.ErrInvalidCommandArguments
	}
	if !model.SameBinding(service.session.Room, binding) || !service.observeMole(snapshot) {
		return nil
	}
	if err := service.protocolDispatcher.Push(uint32(businesspb.MsgId_S2C_MOLE_STATE), &businesspb.MoleStatePush{Snapshot: service.moleSnapshot}); err != nil {
		service.disconnect(self)
		self.Exit()
		return errors.Join(err, service.client.Close())
	}
	return nil
}

// moleView 在客户端出口组装 Protobuf 视图；内部快照始终是 GSR 支持的值集合。
func moleView(fields map[string]any) (*businesspb.MoleSnapshot, bool) {
	roomID, roomOK := fields["room_id"].(string)
	roundID, roundOK := fields["round_id"].(string)
	phase, phaseOK := fields["phase"].(string)
	version, versionOK := fields["state_version"].(uint64)
	endsAt, endsOK := fields["ends_at_ms"].(int64)
	members, membersOK := fields["members"].([]any)
	if !roomOK || roomID == "" || !roundOK || !phaseOK || (phase != "filling" && phase != "running" && phase != "ended") || !versionOK || version == 0 || !endsOK || !membersOK || len(members) > 8 {
		return nil, false
	}
	snapshot := &businesspb.MoleSnapshot{RoomId: roomID, RoundId: roundID, Phase: phase, StateVersion: version, EndsAtMs: endsAt}
	for _, value := range members {
		member, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		userID, userOK := member["user_id"].(string)
		seat, seatOK := member["seat"].(uint64)
		connected, connectedOK := member["connected"].(bool)
		score, scoreOK := member["score"].(int64)
		if !userOK || userID == "" || !seatOK || seat >= 8 || !connectedOK || !scoreOK {
			return nil, false
		}
		snapshot.Members = append(snapshot.Members, &businesspb.MemberView{UserId: userID, Seat: uint32(seat), Connected: connected, Score: score})
	}
	if value, exists := fields["target"]; exists {
		target, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		instanceID, instanceOK := target["instance_id"].(uint64)
		hole, holeOK := target["hole"].(uint64)
		opensAt, opensOK := target["opens_at_ms"].(int64)
		closesAt, closesOK := target["closes_at_ms"].(int64)
		if !instanceOK || instanceID == 0 || !holeOK || hole >= 9 || !opensOK || !closesOK {
			return nil, false
		}
		snapshot.Target = &businesspb.MoleTarget{InstanceId: instanceID, Hole: uint32(hole), OpensAtMs: opensAt, ClosesAtMs: closesAt}
	}
	if value, exists := fields["result"]; exists {
		result, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		settlementID, settlementOK := result["settlement_id"].(string)
		progress, progressOK := result["progress"].(string)
		deltas, deltasOK := result["deltas"].([]any)
		if !settlementOK || settlementID == "" || !progressOK || !deltasOK || len(deltas) > 8 {
			return nil, false
		}
		snapshot.Result = &businesspb.RoundResult{SettlementId: settlementID, Progress: progress}
		for _, value := range deltas {
			delta, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			userID, userOK := delta["user_id"].(string)
			points, pointsOK := delta["delta"].(int64)
			if !userOK || userID == "" || !pointsOK {
				return nil, false
			}
			snapshot.Result.Deltas = append(snapshot.Result.Deltas, &businesspb.PlayerDelta{UserId: userID, Delta: points})
		}
	}
	return snapshot, true
}
