package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleattack"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleentry"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// comMole 处理本游戏接入协议，只持有路由，由所属 Agent 串行调用。
type comMole struct {
	deps       componentDependencies
	attackName cluster.ServiceName
}

var _ gameComponent = (*comMole)(nil)

// newComMole 创建每连接独立的游戏接入组件，构造时不调用注入能力。
func newComMole(deps componentDependencies) *comMole {
	return &comMole{deps: deps}
}

// Name 返回用于接入互斥与日志的游戏名称。
func (mole *comMole) Name() string {
	return "mole"
}

var errMoleTargetUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.target_unavailable", "target is unavailable")

func (mole *comMole) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.EnterRequest) error {
	identity := mole.deps.Identity()
	if err := mole.deps.AllowEnter(mole.Name()); err != nil {
		return err
	}
	value, err := mole.deps.Call(self, "mole", moleentry.ServiceName, moleentry.MoleEnterCommand,
		identity.UserID, identity.LoginID, string(identity.AccessNode), string(identity.AgentName))
	if err != nil {
		return enterFailure(err)
	}
	snapshot, err := clientSnapshot(value)
	if err != nil {
		return err
	}
	// Join 已返回，先保留断线清理路由；余额非法时由 Agent 封闭业务并关闭连接。
	mole.attackName = cluster.ServiceName(snapshot.AttackName)
	fields := value.(map[string]any)
	silver, err := clientBalance(fields["silver"])
	if err != nil {
		return err
	}
	return reply.Response(uint32(demopb.MsgId_S2C_ENTER), &demopb.EnterResponse{Snapshot: snapshot, Silver: silver})
}

func (mole *comMole) requireGame() error {
	identity := mole.deps.Identity()
	if err := identity.RequireLogin(); err != nil {
		return err
	}
	if mole.attackName == "" {
		return errEnterRequired
	}
	return nil
}

// Entered 从本游戏路由判断是否已进入，不另存状态。
func (mole *comMole) Entered() bool {
	return mole.attackName != ""
}

func (mole *comMole) onHit(self gsr.CommandContext, reply protocol.Context, request *demopb.HitRequest) error {
	identity := mole.deps.Identity()
	if err := mole.requireGame(); err != nil {
		return err
	}
	// 不换 ActionID、不自动重试；未知结果停止操作，重入刷新余额，原操作留给受控工具核查。
	value, err := mole.deps.Call(self, "mole", mole.attackName, moleattack.AttackHitCommand, identity.UserID, identity.LoginID,
		request.GetActionId(), request.GetSpawnSeq(), request.GetHole())
	if errors.Is(err, errMoleTargetUnavailable) {
		// 该拒绝发生在 comMole 接纳记账之前；不伪造 HitID、奖励或余额。
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

func (mole *comMole) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.SnapshotRequest) error {
	identity := mole.deps.Identity()
	if err := mole.requireGame(); err != nil {
		return err
	}
	value, err := mole.deps.Call(self, "mole", mole.attackName, moleattack.AttackSnapshotCommand, identity.UserID, identity.LoginID)
	if err != nil {
		return err
	}
	snapshot, err := clientSnapshot(value)
	if err != nil {
		return err
	}
	if cluster.ServiceName(snapshot.AttackName) != mole.attackName {
		return errInvalidBackendReply
	}
	return reply.Response(uint32(demopb.MsgId_S2C_SNAPSHOT), &demopb.SnapshotResponse{Snapshot: snapshot})
}

func (mole *comMole) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LeaveRequest) error {
	identity := mole.deps.Identity()
	if err := mole.requireGame(); err != nil {
		return err
	}
	value, err := mole.deps.Call(self, "mole", mole.attackName, moleattack.AttackLeaveCommand, identity.UserID, identity.LoginID)
	if err != nil {
		return err
	}
	left, ok := value.(bool)
	if !ok || !left {
		return errInvalidBackendReply
	}
	mole.attackName = ""
	return reply.Response(uint32(demopb.MsgId_S2C_LEAVE), &demopb.LeaveResponse{Left: left})
}

func (mole *comMole) handleSnapshotPush(_ gsr.CommandContext, loginID string, payload map[string]any) error {
	identity := mole.deps.Identity()
	if identity.UserID == 0 || loginID != identity.LoginID || mole.attackName == "" {
		return nil
	}
	snapshot, err := clientSnapshot(payload)
	if err != nil {
		return err
	}
	// 离开/重新进入后的旧组快照不能替换当前组。
	if cluster.ServiceName(snapshot.AttackName) != mole.attackName {
		return nil
	}
	return mole.deps.Push(uint32(demopb.MsgId_S2C_SNAPSHOT_PUSH), &demopb.SnapshotPush{Snapshot: snapshot})
}

func (mole *comMole) handleBalanceCorrectionPush(_ gsr.CommandContext, loginID string, payload map[string]any) error {
	identity := mole.deps.Identity()
	if identity.UserID == 0 || loginID != identity.LoginID || mole.attackName == "" {
		return nil
	}
	silver, err := clientBalance(payload)
	if err != nil {
		return err
	}
	return mole.deps.Push(uint32(demopb.MsgId_S2C_BALANCE_PUSH), &demopb.BalancePush{Silver: silver})
}

// ProtocolHandlers 返回本游戏的客户端协议入口。
func (mole *comMole) ProtocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_ENTER):    mole.onEnter,
		uint32(demopb.MsgId_C2S_HIT):      mole.onHit,
		uint32(demopb.MsgId_C2S_SNAPSHOT): mole.onSnapshot,
		uint32(demopb.MsgId_C2S_LEAVE):    mole.onLeave,
	}
}

// CommandHandlers 返回本游戏的节点推送入口。
func (mole *comMole) CommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		moleSnapshotPushCommand:          mole.handleSnapshotPush,
		moleBalanceCorrectionPushCommand: mole.handleBalanceCorrectionPush,
	}
}

// Disconnect 尽力离开并清空路由；失败由所属 Agent 记录，组件不关闭连接。
func (mole *comMole) Disconnect(self gsr.ServiceContext) error {
	defer func() { mole.attackName = "" }()
	if mole.attackName == "" {
		return nil
	}
	identity := mole.deps.Identity()
	_, err := mole.deps.Call(self, "mole", mole.attackName, moleattack.AttackLeaveCommand, identity.UserID, identity.LoginID)
	return err
}
