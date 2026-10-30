package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

var errMoleTargetUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.target_unavailable", "target is unavailable")

const (
	moleResolveEntryCommand runtime.CommandID = 1
	moleEnterCommand        runtime.CommandID = 1
	moleHitCommand          runtime.CommandID = 2
	moleSnapshotCommand     runtime.CommandID = 3
	moleLeaveCommand        runtime.CommandID = 4
	moleOperationCommand    runtime.CommandID = 3
	molePushCommand         runtime.CommandID = 100
)

func (agent *agent) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.EnterRequest) error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	identity := []any{agent.userID, agent.loginID, string(agent.node), string(agent.name)}
	value, err := agent.call(self, "mole", "mole", moleResolveEntryCommand, identity...)
	if err != nil {
		return err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return errInvalidBackendReply
	}
	entry, ok := fields["entry_name"].(string)
	if !ok || entry == "" {
		return errInvalidBackendReply
	}
	agent.entryName = cluster.ServiceName(entry)
	value, err = agent.call(self, "mole", agent.entryName, moleEnterCommand, identity...)
	if err != nil {
		return err
	}
	snapshot, err := clientSnapshot(value)
	if err != nil {
		return err
	}
	agent.attackName = cluster.ServiceName(snapshot.AttackName)
	fields = value.(map[string]any)
	silver, err := clientBalance(fields["silver"])
	if err != nil {
		return err
	}
	return reply.Response(uint32(demopb.MsgId_S2C_ENTER), &demopb.EnterResponse{Snapshot: snapshot, Silver: silver})
}

func (agent *agent) requireGame() error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	if agent.attackName == "" {
		return errEnterRequired
	}
	return nil
}

func (agent *agent) onHit(self gsr.CommandContext, reply protocol.Context, request *demopb.HitRequest) error {
	if err := agent.requireGame(); err != nil {
		return err
	}
	// 不换 ActionID、不自动重试；未知结果由客户端按原身份查询/重试。
	value, err := agent.call(self, "mole", agent.attackName, moleHitCommand, agent.userID, agent.loginID,
		request.GetActionId(), request.GetSpawnSeq(), request.GetHole())
	if errors.Is(err, errMoleTargetUnavailable) {
		// 该拒绝发生在 Mole 接纳记账之前；不伪造 HitID、奖励或余额。
		return reply.Response(uint32(demopb.MsgId_S2C_HIT), &demopb.HitResponse{Rejection: "target_unavailable"})
	}
	if err != nil {
		return err
	}
	hit, err := clientHit(value)
	if err != nil {
		return err
	}
	var silver *demopb.Balance
	if hit.Status == "accepted" {
		silver, err = clientBalance(value)
		if err != nil {
			return err
		}
	}
	return reply.Response(uint32(demopb.MsgId_S2C_HIT), &demopb.HitResponse{Hit: hit, Silver: silver})
}

func (agent *agent) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.SnapshotRequest) error {
	if err := agent.requireGame(); err != nil {
		return err
	}
	value, err := agent.call(self, "mole", agent.attackName, moleSnapshotCommand, agent.userID, agent.loginID)
	if err != nil {
		return err
	}
	snapshot, err := clientSnapshot(value)
	if err != nil {
		return err
	}
	return reply.Response(uint32(demopb.MsgId_S2C_SNAPSHOT), &demopb.SnapshotResponse{Snapshot: snapshot})
}

func (agent *agent) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LeaveRequest) error {
	if err := agent.requireGame(); err != nil {
		return err
	}
	value, err := agent.call(self, "mole", agent.attackName, moleLeaveCommand, agent.userID, agent.loginID)
	if err != nil {
		return err
	}
	left, ok := value.(bool)
	if !ok {
		return errInvalidBackendReply
	}
	agent.attackName = ""
	return reply.Response(uint32(demopb.MsgId_S2C_LEAVE), &demopb.LeaveResponse{Left: left})
}

func (agent *agent) onOperation(self gsr.CommandContext, reply protocol.Context, request *demopb.OperationRequest) error {
	if agent.userID == 0 {
		return errLoginRequired
	}
	if agent.entryName == "" {
		return errEnterRequired
	}
	value, err := agent.call(self, "mole", agent.entryName, moleOperationCommand, agent.userID, request.GetHitId())
	if err != nil {
		return err
	}
	response := &demopb.OperationResponse{HitId: request.GetHitId(), Found: value != nil}
	if value != nil {
		fields, ok := value.(map[string]any)
		if !ok || fields["operation_id"] != request.GetHitId() {
			return errInvalidBackendReply
		}
		status, statusOK := fields["status"].(string)
		reason, reasonOK := fields["reason"].(string)
		if !statusOK || !reasonOK || (status != "committed" && status != "rejected") {
			return errInvalidBackendReply
		}
		response.Status, response.Reason = status, reason
	}
	return reply.Response(uint32(demopb.MsgId_S2C_OPERATION), response)
}

func (agent *agent) push(_ gsr.CommandContext, loginID, kind string, payload map[string]any) error {
	if agent.userID == 0 || loginID != agent.loginID || agent.attackName == "" {
		return nil
	}
	switch kind {
	case "snapshot":
		snapshot, err := clientSnapshot(payload)
		if err != nil {
			return err
		}
		// 离开/重新进入后的旧组快照不能替换当前组。
		if cluster.ServiceName(snapshot.AttackName) != agent.attackName {
			return nil
		}
		return agent.protocolDispatcher.Push(uint32(demopb.MsgId_S2C_SNAPSHOT_PUSH), &demopb.SnapshotPush{Snapshot: snapshot})
	case "balance_correction":
		silver, err := clientBalance(payload)
		if err != nil {
			return err
		}
		return agent.protocolDispatcher.Push(uint32(demopb.MsgId_S2C_BALANCE_PUSH), &demopb.BalancePush{Silver: silver})
	}
	return errInvalidBackendReply
}
