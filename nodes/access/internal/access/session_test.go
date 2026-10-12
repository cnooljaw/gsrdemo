package access

import (
	"testing"

	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"google.golang.org/protobuf/proto"
)

// TestAgentRejectsUnconfirmedAndOldRoomState 保护推送不能切换路由、同位置版本身份一致及房间版本不倒退。
func TestAgentRejectsUnconfirmedAndOldRoomState(t *testing.T) {
	identity := &businesspb.Identity{UserId: "u", LoginId: "l", AccessNode: "access", AgentName: "a", LocationVersion: 1}
	service := &agent{identity: identity}
	room := &businesspb.SessionRecord{Identity: proto.Clone(identity).(*businesspb.Identity), Room: &businesspb.RoomBinding{Identity: identity, RoomId: "r", BindingToken: "t"}, Snapshot: &businesspb.MoleSnapshot{RoomId: "r", Phase: "running", StateVersion: 10}}
	if service.observe(room.Snapshot) {
		t.Fatal("push installed unconfirmed route")
	}
	if !service.install(room) {
		t.Fatal("confirmed route refused")
	}
	if service.observe(&businesspb.MoleSnapshot{RoomId: "other", StateVersion: 100}) || service.observe(&businesspb.MoleSnapshot{RoomId: "r", StateVersion: 9}) {
		t.Fatal("old or foreign state installed")
	}
	conflict := proto.Clone(room).(*businesspb.SessionRecord)
	conflict.Room.RoomId = "other"
	if service.install(conflict) {
		t.Fatal("same version changed route")
	}
	old := proto.Clone(room).(*businesspb.SessionRecord)
	old.Identity.LocationVersion = 0
	if service.install(old) {
		t.Fatal("old location restored")
	}
	old = proto.Clone(room).(*businesspb.SessionRecord)
	old.Snapshot.StateVersion = 2
	if !service.install(old) || service.session.Snapshot.StateVersion != 10 {
		t.Fatal("query rolled snapshot back")
	}
}
