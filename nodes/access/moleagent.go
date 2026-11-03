package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// moleAgent 只持有本游戏接入路由，由所属 Agent 串行调用，不保存父 Agent 或回调上下文。
type moleAgent struct {
	identity   func() agentSession
	allowEnter func(string) error
	push       func(uint32, proto.Message) error
	entryName  cluster.ServiceName
	attackName cluster.ServiceName
}

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

func (mole *moleAgent) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.EnterRequest) error {
	identity := mole.identity()
	if err := mole.allowEnter("mole"); err != nil {
		return err
	}
	args := []any{identity.userID, identity.loginID, string(identity.node), string(identity.name)}
	value, err := callBackend(self, "mole", "mole", moleResolveEntryCommand, args...)
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
	mole.entryName = cluster.ServiceName(entry)
	value, err = callBackend(self, "mole", mole.entryName, moleEnterCommand, args...)
	if err != nil {
		return err
	}
	snapshot, err := clientSnapshot(value)
	if err != nil {
		return err
	}
	mole.attackName = cluster.ServiceName(snapshot.AttackName)
	fields = value.(map[string]any)
	silver, err := clientBalance(fields["silver"])
	if err != nil {
		return err
	}
	return reply.Response(uint32(demopb.MsgId_S2C_ENTER), &demopb.EnterResponse{Snapshot: snapshot, Silver: silver})
}

func (mole *moleAgent) requireGame() error {
	identity := mole.identity()
	if err := identity.requireLogin(); err != nil {
		return err
	}
	if mole.attackName == "" {
		return errEnterRequired
	}
	return nil
}

func (mole *moleAgent) entered() bool {
	return mole.attackName != ""
}

func (mole *moleAgent) onHit(self gsr.CommandContext, reply protocol.Context, request *demopb.HitRequest) error {
	identity := mole.identity()
	if err := mole.requireGame(); err != nil {
		return err
	}
	// 不换 ActionID、不自动重试；未知结果由客户端按原身份查询/重试。
	value, err := callBackend(self, "mole", mole.attackName, moleHitCommand, identity.userID, identity.loginID,
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

func (mole *moleAgent) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.SnapshotRequest) error {
	identity := mole.identity()
	if err := mole.requireGame(); err != nil {
		return err
	}
	value, err := callBackend(self, "mole", mole.attackName, moleSnapshotCommand, identity.userID, identity.loginID)
	if err != nil {
		return err
	}
	snapshot, err := clientSnapshot(value)
	if err != nil {
		return err
	}
	return reply.Response(uint32(demopb.MsgId_S2C_SNAPSHOT), &demopb.SnapshotResponse{Snapshot: snapshot})
}

func (mole *moleAgent) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LeaveRequest) error {
	identity := mole.identity()
	if err := mole.requireGame(); err != nil {
		return err
	}
	value, err := callBackend(self, "mole", mole.attackName, moleLeaveCommand, identity.userID, identity.loginID)
	if err != nil {
		return err
	}
	left, ok := value.(bool)
	if !ok {
		return errInvalidBackendReply
	}
	mole.attackName = ""
	return reply.Response(uint32(demopb.MsgId_S2C_LEAVE), &demopb.LeaveResponse{Left: left})
}

func (mole *moleAgent) onOperation(self gsr.CommandContext, reply protocol.Context, request *demopb.OperationRequest) error {
	identity := mole.identity()
	if err := identity.requireLogin(); err != nil {
		return err
	}
	if mole.entryName == "" {
		return errEnterRequired
	}
	value, err := callBackend(self, "mole", mole.entryName, moleOperationCommand, identity.userID, request.GetHitId())
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

func (mole *moleAgent) handlePush(_ gsr.CommandContext, loginID, kind string, payload map[string]any) error {
	identity := mole.identity()
	if identity.userID == 0 || loginID != identity.loginID || mole.attackName == "" {
		return nil
	}
	switch kind {
	case "snapshot":
		snapshot, err := clientSnapshot(payload)
		if err != nil {
			return err
		}
		// 离开/重新进入后的旧组快照不能替换当前组。
		if cluster.ServiceName(snapshot.AttackName) != mole.attackName {
			return nil
		}
		return mole.push(uint32(demopb.MsgId_S2C_SNAPSHOT_PUSH), &demopb.SnapshotPush{Snapshot: snapshot})
	case "balance_correction":
		silver, err := clientBalance(payload)
		if err != nil {
			return err
		}
		return mole.push(uint32(demopb.MsgId_S2C_BALANCE_PUSH), &demopb.BalancePush{Silver: silver})
	}
	return errInvalidBackendReply
}

func (mole *moleAgent) protocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_ENTER):     mole.onEnter,
		uint32(demopb.MsgId_C2S_HIT):       mole.onHit,
		uint32(demopb.MsgId_C2S_SNAPSHOT):  mole.onSnapshot,
		uint32(demopb.MsgId_C2S_LEAVE):     mole.onLeave,
		uint32(demopb.MsgId_C2S_OPERATION): mole.onOperation,
	}
}

func (mole *moleAgent) commandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		molePushCommand: mole.handlePush,
	}
}

func (mole *moleAgent) disconnect(self gsr.ServiceContext) error {
	defer func() { mole.entryName, mole.attackName = "", "" }()
	if mole.attackName == "" {
		return nil
	}
	identity := mole.identity()
	_, err := callBackend(self, "mole", mole.attackName, moleLeaveCommand, identity.userID, identity.loginID)
	return err
}
