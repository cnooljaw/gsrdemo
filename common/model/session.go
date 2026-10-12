package model

import (
	"crypto/rand"
	"encoding/hex"
	"slices"

	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
)

// NewID 生成跨节点/重启不复用的业务关联标识。
func NewID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// ValidOperationID 限制业务操作关联字段的输入大小。
func ValidOperationID(value string) bool {
	return len(value) > 0 && len(value) <= 64
}

// SameIdentity 比较服务端身份及其确认的接入代次。
func SameIdentity(first, second *businesspb.Identity) bool {
	return first != nil && second != nil && first.UserId == second.UserId && first.LoginId == second.LoginId && first.LocationVersion == second.LocationVersion && first.AccessNode == second.AccessNode && first.AgentName == second.AgentName
}

// SameBinding 防止旧连接、迟到推送和旧房间命令作用到新绑定。
func SameBinding(first, second *businesspb.RoomBinding) bool {
	return first != nil && second != nil && first.RoomId == second.RoomId && first.BindingToken == second.BindingToken && SameIdentity(first.Identity, second.Identity)
}

// Allowed 从已确认位置推导准入，不存第二份业务状态或动态重建 Dispatcher。
func Allowed(session *businesspb.SessionRecord, pending string) []uint32 {
	if pending != "" {
		return []uint32{uint32(businesspb.MsgId_C2S_QUERY_SESSION), uint32(businesspb.MsgId_C2S_QUERY_OPERATION)}
	}
	if session.GetIdentity().GetUserId() == "" {
		return []uint32{uint32(businesspb.MsgId_C2S_PASSWORD_LOGIN)}
	}
	ids := []uint32{uint32(businesspb.MsgId_C2S_QUERY_SESSION), uint32(businesspb.MsgId_C2S_QUERY_OPERATION)}
	if session.GetRoom() == nil {
		return append(ids, uint32(businesspb.MsgId_C2S_ENTER_MOLE))
	}
	ids = append(ids, uint32(businesspb.MsgId_C2S_MOLE_SNAPSHOT))
	switch session.GetSnapshot().GetPhase() {
	case "filling", "ended":
		ids = append(ids, uint32(businesspb.MsgId_C2S_LEAVE_ROOM))
	case "running":
		ids = append(ids, uint32(businesspb.MsgId_C2S_MOLE_HIT))
	}
	return ids
}

// Admission 返回可写入相应业务 Response 的拒绝结果。
func Admission(session *businesspb.SessionRecord, pending string, id uint32) string {
	if slices.Contains(Allowed(session, pending), id) {
		return "ok"
	}
	if pending != "" {
		return "operation_pending"
	}
	if session.GetIdentity().GetUserId() == "" {
		return "not_authenticated"
	}
	return "state_mismatch"
}

// View 只暴露客户端需要的位置与准入，不泄漏内部 Service 地址或绑定令牌。
func View(session *businesspb.SessionRecord, pending string) *businesspb.SessionView {
	view := &businesspb.SessionView{Location: "guest", PendingOperationId: pending, AllowedMsgIds: Allowed(session, pending)}
	if session.GetIdentity().GetUserId() == "" {
		return view
	}
	view.UserId = session.Identity.UserId
	view.LocationVersion = session.Identity.LocationVersion
	view.Location = "lobby"
	if session.GetRoom() == nil {
		return view
	}
	view.Location = "room"
	view.GameKind = "mole"
	view.RoomId = session.Room.RoomId
	view.RoundId = session.GetSnapshot().GetRoundId()
	view.RoomPhase = session.GetSnapshot().GetPhase()
	view.RoomStateVersion = session.GetSnapshot().GetStateVersion()
	return view
}
