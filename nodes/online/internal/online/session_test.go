package online

import (
	"testing"

	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"google.golang.org/protobuf/proto"
)

// TestPositionStaysConfirmedUntilRoomAcknowledges 保护待确认迁移不切路由、一次只保留一条操作及重试冲突。
func TestPositionStaysConfirmedUntilRoomAcknowledges(t *testing.T) {
	service := newSessionService()
	identity := &businesspb.Identity{UserId: "u", LoginId: "l", AccessNode: "access", AgentName: "agent"}
	bind := &businesspb.PositionCommand{OperationId: "login", Kind: "bind", Identity: identity}
	user, reply := service.begin(bind)
	if reply != nil {
		t.Fatal(reply)
	}
	service.complete(user, &businesspb.RoomReply{Code: "ok"})
	confirmed := proto.Clone(user.confirmed).(*businesspb.SessionRecord)
	enter := &businesspb.PositionCommand{OperationId: "enter", Kind: "enter", Identity: confirmed.Identity}
	_, reply = service.begin(enter)
	if reply != nil || !proto.Equal(user.confirmed, confirmed) || user.pending == nil {
		t.Fatal("unconfirmed route installed")
	}
	_, reply = service.begin(&businesspb.PositionCommand{OperationId: "other", Kind: "enter", Identity: confirmed.Identity})
	if reply.Code != "operation_pending" {
		t.Fatal(reply)
	}
	changed := proto.Clone(enter).(*businesspb.PositionCommand)
	changed.Kind = "leave"
	_, reply = service.begin(changed)
	if reply.Code != "operation_conflict" {
		t.Fatal(reply)
	}
	binding := &businesspb.RoomBinding{Identity: user.pending.target.Identity, RoomId: "r", BindingToken: "token"}
	service.complete(user, &businesspb.RoomReply{Code: "ok", Binding: binding, Snapshot: &businesspb.MoleSnapshot{RoomId: "r", Phase: "filling", StateVersion: 1}})
	if user.confirmed.Room.RoomId != "r" || user.pending != nil || user.confirmed.Identity.LocationVersion <= confirmed.Identity.LocationVersion {
		t.Fatal("room not confirmed")
	}
}

// TestRejectedPositionDoesNotReuseVersion 保护明确拒绝保留原位置，并且不复用已分配的目标代次。
func TestRejectedPositionDoesNotReuseVersion(t *testing.T) {
	service := newSessionService()
	user, _ := service.begin(&businesspb.PositionCommand{OperationId: "login", Kind: "bind", Identity: &businesspb.Identity{UserId: "u", LoginId: "l", AccessNode: "access", AgentName: "agent"}})
	service.complete(user, &businesspb.RoomReply{Code: "ok"})
	request := &businesspb.PositionCommand{OperationId: "enter", Kind: "enter", Identity: user.confirmed.Identity}
	service.begin(request)
	rejectedVersion := user.pending.target.Identity.LocationVersion
	service.complete(user, &businesspb.RoomReply{Code: "capacity"})
	if user.confirmed.Room != nil {
		t.Fatal("rejected room installed")
	}
	request.OperationId = "enter2"
	service.begin(request)
	if user.pending.target.Identity.LocationVersion <= rejectedVersion {
		t.Fatal("reused version")
	}
}

// TestOldDisconnectCannotMarkPendingReplacementOffline 保护连接替换期间旧断线只更新旧身份，不能污染新连接。
func TestOldDisconnectCannotMarkPendingReplacementOffline(t *testing.T) {
	service := newSessionService()
	old := &businesspb.Identity{UserId: "u", LoginId: "old", AccessNode: "access", AgentName: "old"}
	user, _ := service.begin(&businesspb.PositionCommand{OperationId: "first", Kind: "bind", Identity: old})
	service.complete(user, &businesspb.RoomReply{Code: "ok"})
	old = proto.Clone(user.confirmed.Identity).(*businesspb.Identity)
	next := proto.Clone(old).(*businesspb.Identity)
	next.LoginId = "new"
	next.AgentName = "new"
	service.begin(&businesspb.PositionCommand{OperationId: "second", Kind: "bind", Identity: next})
	service.connectionClosed(old)
	if !user.pending.connected {
		t.Fatal("old disconnect marked new candidate offline")
	}
	service.complete(user, &businesspb.RoomReply{Code: "ok"})
	service.connectionClosed(old)
	if !user.connected || user.confirmed.Identity.LoginId != "new" {
		t.Fatal("new connection offline")
	}
}

// TestUnknownBindKeepsOriginalPayloadAfterDisconnect 保护结果未知时同操作重试内容不变，断线另行条件清理。
func TestUnknownBindKeepsOriginalPayloadAfterDisconnect(t *testing.T) {
	service := newSessionService()
	user, _ := service.begin(&businesspb.PositionCommand{OperationId: "login", Kind: "bind", Identity: &businesspb.Identity{UserId: "u", LoginId: "l", AccessNode: "access", AgentName: "a"}})
	reservation := &businesspb.RoomBinding{Identity: user.pending.target.Identity, RoomId: "r", BindingToken: "reserved"}
	original := proto.Clone(user.pending.bindingRequest(reservation))
	service.connectionClosed(user.pending.target.Identity)
	if user.pending.connected || !proto.Equal(original, user.pending.bindingRequest(reservation)) {
		t.Fatal("retry changed the unknown operation")
	}
}
