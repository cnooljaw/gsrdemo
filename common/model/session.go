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

// SessionRecord 只记录身份、房间绑定和准入所需的阶段投影；空 RoomID 表示大厅。
type SessionRecord struct {
	Identity                     Identity
	Room                         RoomBinding
	GameKind, RoundID, RoomPhase string
	RoomStateVersion             uint64
}

// PositionCommand 是一次位置操作的不可变内容，用值比较判断同 ID 重试。
type PositionCommand struct {
	OperationID, Kind string
	Identity          Identity
	RoomID            string
}

// Values 新建已确认位置的业务字段集合，不包含游戏缓存。
func (session SessionRecord) Values() map[string]any {
	fields := map[string]any{
		"identity": session.Identity.Values(), "game_kind": session.GameKind,
		"round_id": session.RoundID, "room_phase": session.RoomPhase,
		"room_state_version": session.RoomStateVersion,
	}
	if session.Room.RoomID != "" {
		fields["room"] = session.Room.Values()
	}
	return fields
}

// ReadSession 校验已确认身份与房间身份一致，拒绝错误类型或未分配代次。
func ReadSession(value any) (SessionRecord, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return SessionRecord{}, false
	}
	session := SessionRecord{}
	session.Identity, ok = ReadIdentity(fields["identity"])
	if !ok || session.Identity.LocationVersion == 0 {
		return SessionRecord{}, false
	}
	session.GameKind, ok = fields["game_kind"].(string)
	if !ok {
		return SessionRecord{}, false
	}
	session.RoundID, ok = fields["round_id"].(string)
	if !ok {
		return SessionRecord{}, false
	}
	session.RoomPhase, ok = fields["room_phase"].(string)
	if !ok {
		return SessionRecord{}, false
	}
	session.RoomStateVersion, ok = fields["room_state_version"].(uint64)
	if !ok {
		return SessionRecord{}, false
	}
	if value, exists := fields["room"]; exists {
		session.Room, ok = ReadBinding(value)
		if !ok || session.Room.Identity != session.Identity || session.GameKind == "" {
			return SessionRecord{}, false
		}
	}
	return session, true
}

// Allowed 从已确认位置推导准入，不存第二份业务状态或动态重建 Dispatcher。
func Allowed(session SessionRecord, pending string) []uint32 {
	if pending != "" {
		return []uint32{uint32(businesspb.MsgId_C2S_QUERY_SESSION), uint32(businesspb.MsgId_C2S_QUERY_OPERATION)}
	}
	if session.Identity.UserID == "" {
		return []uint32{uint32(businesspb.MsgId_C2S_PASSWORD_LOGIN)}
	}
	ids := []uint32{uint32(businesspb.MsgId_C2S_QUERY_SESSION), uint32(businesspb.MsgId_C2S_QUERY_OPERATION)}
	if session.Room.RoomID == "" {
		return append(ids, uint32(businesspb.MsgId_C2S_ENTER_MOLE))
	}
	if session.GameKind != "mole" {
		return ids
	}
	ids = append(ids, uint32(businesspb.MsgId_C2S_MOLE_SNAPSHOT))
	switch session.RoomPhase {
	case "filling", "ended":
		ids = append(ids, uint32(businesspb.MsgId_C2S_LEAVE_ROOM))
	case "running":
		ids = append(ids, uint32(businesspb.MsgId_C2S_MOLE_HIT))
	}
	return ids
}

// Admission 返回可写入相应业务 Response 的拒绝结果。
func Admission(session SessionRecord, pending string, id uint32) string {
	if slices.Contains(Allowed(session, pending), id) {
		return "ok"
	}
	if pending != "" {
		return "operation_pending"
	}
	if session.Identity.UserID == "" {
		return "not_authenticated"
	}
	return "state_mismatch"
}

// View 只暴露客户端需要的位置与准入，不泄漏内部 Service 地址或绑定令牌。
func View(session SessionRecord, pending string) *businesspb.SessionView {
	view := &businesspb.SessionView{Location: "guest", PendingOperationId: pending, AllowedMsgIds: Allowed(session, pending)}
	if session.Identity.UserID == "" {
		return view
	}
	view.UserId = session.Identity.UserID
	view.LocationVersion = session.Identity.LocationVersion
	view.Location = "lobby"
	if session.Room.RoomID == "" {
		return view
	}
	view.Location = "room"
	view.GameKind = session.GameKind
	view.RoomId = session.Room.RoomID
	view.RoundId = session.RoundID
	view.RoomPhase = session.RoomPhase
	view.RoomStateVersion = session.RoomStateVersion
	return view
}

// ObservePhase 只记录当前房间较新的阶段信息，不保存游戏快照，也不改变路由。
func ObservePhase(session *SessionRecord, roomID, roundID, phase string, version uint64) bool {
	if session == nil || session.Room.RoomID == "" || roomID != session.Room.RoomID || version <= session.RoomStateVersion {
		return false
	}
	session.RoundID = roundID
	session.RoomPhase = phase
	session.RoomStateVersion = version
	return true
}
