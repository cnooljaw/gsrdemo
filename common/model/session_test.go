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
	session := &businesspb.SessionRecord{Identity: &businesspb.Identity{UserId: "u", LoginId: "l"}}
	if !slices.Contains(Allowed(nil, ""), login) || slices.Contains(Allowed(nil, ""), hit) {
		t.Fatal("guest admission")
	}
	if !slices.Contains(Allowed(session, ""), enter) || slices.Contains(Allowed(session, ""), login) {
		t.Fatal("lobby admission")
	}
	session.Room = &businesspb.RoomBinding{RoomId: "r"}
	if slices.Contains(Allowed(session, ""), hit) {
		t.Fatal("unknown phase admitted hit")
	}
	session.Snapshot = &businesspb.MoleSnapshot{Phase: "running"}
	if !slices.Contains(Allowed(session, ""), hit) || slices.Contains(Allowed(session, ""), enter) {
		t.Fatal("room admission")
	}
	if slices.Contains(Allowed(session, "pending"), hit) {
		t.Fatal("unconfirmed operation admitted hit")
	}
	session.Snapshot.Phase = "ended"
	if slices.Contains(Allowed(session, ""), hit) {
		t.Fatal("ended admitted hit")
	}
}
