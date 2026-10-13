package access

import (
	"testing"

	"github.com/cnooljaw/gsrdemo/common/model"
)

func moleState(room, phase string, version uint64) map[string]any {
	return map[string]any{"room_id": room, "round_id": "", "phase": phase, "state_version": version, "members": []any{}, "ends_at_ms": int64(0)}
}

// TestAgentRejectsUnconfirmedAndOldRoomState 保护推送不能切换路由、同位置版本身份一致及房间版本不倒退。
func TestAgentRejectsUnconfirmedAndOldRoomState(t *testing.T) {
	identity := model.Identity{UserID: "u", LoginID: "l", AccessNode: "access", AgentName: "a", LocationVersion: 1}
	service := &agent{identity: identity}
	room := model.SessionRecord{
		Identity: identity,
		Room:     model.RoomBinding{Identity: identity, RoomID: "r", BindingToken: "t"},
		GameKind: "mole", RoomPhase: "running", RoomStateVersion: 10,
	}
	snapshot := moleState("r", "running", 10)
	if service.observeMole(snapshot) {
		t.Fatal("push installed unconfirmed route")
	}
	if !service.installPosition(room) {
		t.Fatal("confirmed route refused")
	}
	if !service.observeMole(snapshot) {
		t.Fatal("game snapshot refused at confirmed version")
	}
	if service.observeMole(moleState("other", "running", 100)) || service.observeMole(moleState("r", "running", 9)) {
		t.Fatal("old or foreign state installed")
	}
	conflict := room
	conflict.Room.RoomID = "other"
	if service.installPosition(conflict) {
		t.Fatal("same version changed route")
	}
	old := room
	old.Identity.LocationVersion = 0
	if service.installPosition(old) {
		t.Fatal("old location restored")
	}
	old = room
	old.RoomStateVersion = 2
	old.RoomPhase = "filling"
	if !service.installPosition(old) || service.positionResult("ok", "").Session.RoomPhase != "running" || service.positionResult("ok", "").Snapshot.StateVersion != 10 {
		t.Fatal("query rolled snapshot back")
	}
	conflict = room
	conflict.GameKind = "other"
	if service.installPosition(conflict) {
		t.Fatal("same position changed game admission")
	}
	lobby := model.SessionRecord{Identity: identity}
	lobby.Identity.LocationVersion++
	if !service.installPosition(lobby) || service.positionResult("ok", "").Snapshot != nil {
		t.Fatal("left room still exposed game snapshot")
	}
}

// TestNativeMoleStateRejectsBadFieldsAndOwnsClientView 保护规范数字类型、字段边界及客户端视图独占。
func TestNativeMoleStateRejectsBadFieldsAndOwnsClientView(t *testing.T) {
	identity := model.Identity{UserID: "u", LoginID: "l", LocationVersion: 1}
	service := &agent{session: model.SessionRecord{Identity: identity, Room: model.RoomBinding{RoomID: "r"}, GameKind: "mole"}}
	fields := moleState("r", "running", 1)
	member := map[string]any{"user_id": "u", "seat": uint64(0), "connected": true, "score": int64(10)}
	fields["members"] = []any{member}
	if !service.observeMole(fields) {
		t.Fatal("valid state refused")
	}
	member["score"] = int64(999)
	if service.moleSnapshot.Members[0].Score != 10 {
		t.Fatal("client view aliases received map")
	}
	fields["state_version"] = uint64(2)
	member["seat"] = uint64(1 << 32)
	if service.observeMole(fields) {
		t.Fatal("seat overflow accepted")
	}
	member["seat"] = uint64(0)
	fields["state_version"] = int64(2)
	if service.observeMole(fields) || service.session.RoomStateVersion != 1 {
		t.Fatal("signed version changed admission")
	}
}
