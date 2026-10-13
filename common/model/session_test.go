package model

import (
	"slices"
	"testing"

	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
)

// TestProtocolAdmissionFollowsConfirmedLocation 保护最小位置状态和待确认操作的协议准入。
func TestProtocolAdmissionFollowsConfirmedLocation(t *testing.T) {
	hit := uint32(businesspb.MsgId_C2S_MOLE_HIT)
	enter := uint32(businesspb.MsgId_C2S_ENTER_MOLE)
	login := uint32(businesspb.MsgId_C2S_PASSWORD_LOGIN)
	session := SessionRecord{Identity: Identity{UserID: "u", LoginID: "l"}}
	if !slices.Contains(Allowed(SessionRecord{}, ""), login) || slices.Contains(Allowed(SessionRecord{}, ""), hit) {
		t.Fatal("guest admission")
	}
	if !slices.Contains(Allowed(session, ""), enter) || slices.Contains(Allowed(session, ""), login) {
		t.Fatal("lobby admission")
	}
	session.Room = RoomBinding{RoomID: "r"}
	session.GameKind = "mole"
	if slices.Contains(Allowed(session, ""), hit) {
		t.Fatal("unknown phase admitted hit")
	}
	session.RoomPhase = "running"
	if !slices.Contains(Allowed(session, ""), hit) || slices.Contains(Allowed(session, ""), enter) {
		t.Fatal("room admission")
	}
	if slices.Contains(Allowed(session, "pending"), hit) {
		t.Fatal("unconfirmed operation admitted hit")
	}
	session.RoomPhase = "ended"
	if slices.Contains(Allowed(session, ""), hit) {
		t.Fatal("ended admitted hit")
	}
	session.GameKind = "unknown"
	if slices.Contains(Allowed(session, ""), uint32(businesspb.MsgId_C2S_MOLE_SNAPSHOT)) || View(session, "").GameKind != "unknown" {
		t.Fatal("unknown game treated as mole")
	}
}
