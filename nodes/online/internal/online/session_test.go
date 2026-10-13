package online

import (
	"testing"

	"github.com/cnooljaw/gsrdemo/common/model"
)

// TestPositionStaysConfirmedUntilRoomAcknowledges 保护待确认迁移不切路由、一次只保留一条操作及重试冲突。
func TestPositionStaysConfirmedUntilRoomAcknowledges(t *testing.T) {
	service := newSessionService()
	identity := model.Identity{UserID: "u", LoginID: "l", AccessNode: "access", AgentName: "agent"}
	bind := model.PositionCommand{OperationID: "login", Kind: "bind", Identity: identity}
	user, reply := service.begin(bind)
	if reply != nil {
		t.Fatal(reply)
	}
	service.complete(user, model.RoomReply{Code: "ok"})
	confirmed := user.confirmed
	enter := model.PositionCommand{OperationID: "enter", Kind: "enter", Identity: confirmed.Identity}
	_, reply = service.begin(enter)
	if reply != nil || user.confirmed != confirmed || user.pending == nil {
		t.Fatal("unconfirmed route installed")
	}
	_, reply = service.begin(model.PositionCommand{OperationID: "other", Kind: "enter", Identity: confirmed.Identity})
	if reply["code"] != "operation_pending" {
		t.Fatal(reply)
	}
	changed := enter
	changed.Kind = "leave"
	_, reply = service.begin(changed)
	if reply["code"] != "operation_conflict" {
		t.Fatal(reply)
	}
	binding := model.RoomBinding{Identity: user.pending.target.Identity, RoomID: "r", BindingToken: "token"}
	reply = service.complete(user, model.RoomReply{Code: "ok", Binding: binding, Snapshot: map[string]any{"room_id": "r", "round_id": "", "phase": "filling", "state_version": uint64(1)}})
	if user.confirmed.Room.RoomID != "r" || user.pending != nil || user.confirmed.Identity.LocationVersion <= confirmed.Identity.LocationVersion {
		t.Fatal("room not confirmed")
	}
	if user.confirmed.RoomPhase != "filling" || reply["snapshot"].(map[string]any)["room_id"] != "r" {
		t.Fatal("confirmed position or forwarded game state lost")
	}
}

// TestRoomPhaseFiltersOldBindingsAndVersions 保护原生阶段通知只能推进当前房间，迟到通知不能恢复旧阶段。
func TestRoomPhaseFiltersOldBindingsAndVersions(t *testing.T) {
	service := newSessionService()
	identity := model.Identity{UserID: "u", LoginID: "l", AccessNode: "access", AgentName: "a", LocationVersion: 1}
	binding := model.RoomBinding{Identity: identity, RoomID: "r", BindingToken: "current"}
	user := &onlineUser{confirmed: model.SessionRecord{Identity: identity, Room: binding, GameKind: "mole", RoomPhase: "filling", RoomStateVersion: 1}}
	service.users["u"] = user
	if err := service.handlePhase(nil, binding.Values(), "round", "running", 10); err != nil {
		t.Fatal(err)
	}
	if err := service.handlePhase(nil, binding.Values(), "old", "filling", 2); err != nil {
		t.Fatal(err)
	}
	old := binding
	old.BindingToken = "old"
	if err := service.handlePhase(nil, old.Values(), "other", "ended", 20); err != nil {
		t.Fatal(err)
	}
	if user.confirmed.RoomPhase != "running" || user.confirmed.RoundID != "round" || user.confirmed.RoomStateVersion != 10 {
		t.Fatal("old notice changed confirmed phase")
	}
}

// TestRejectedPositionDoesNotReuseVersion 保护明确拒绝保留原位置，并且不复用已分配的目标代次。
func TestRejectedPositionDoesNotReuseVersion(t *testing.T) {
	service := newSessionService()
	user, _ := service.begin(model.PositionCommand{OperationID: "login", Kind: "bind", Identity: model.Identity{UserID: "u", LoginID: "l", AccessNode: "access", AgentName: "agent"}})
	service.complete(user, model.RoomReply{Code: "ok"})
	request := model.PositionCommand{OperationID: "enter", Kind: "enter", Identity: user.confirmed.Identity}
	service.begin(request)
	rejectedVersion := user.pending.target.Identity.LocationVersion
	service.complete(user, model.RoomReply{Code: "capacity"})
	if user.confirmed.Room.RoomID != "" {
		t.Fatal("rejected room installed")
	}
	request.OperationID = "enter2"
	service.begin(request)
	if user.pending.target.Identity.LocationVersion <= rejectedVersion {
		t.Fatal("reused version")
	}
}

// TestOldDisconnectCannotMarkPendingReplacementOffline 保护连接替换期间旧断线只更新旧身份，不能污染新连接。
func TestOldDisconnectCannotMarkPendingReplacementOffline(t *testing.T) {
	service := newSessionService()
	old := model.Identity{UserID: "u", LoginID: "old", AccessNode: "access", AgentName: "old"}
	user, _ := service.begin(model.PositionCommand{OperationID: "first", Kind: "bind", Identity: old})
	service.complete(user, model.RoomReply{Code: "ok"})
	old = user.confirmed.Identity
	next := old
	next.LoginID = "new"
	next.AgentName = "new"
	service.begin(model.PositionCommand{OperationID: "second", Kind: "bind", Identity: next})
	service.connectionClosed(old)
	if !user.pending.connected {
		t.Fatal("old disconnect marked new candidate offline")
	}
	service.complete(user, model.RoomReply{Code: "ok"})
	service.connectionClosed(old)
	if !user.connected || user.confirmed.Identity.LoginID != "new" {
		t.Fatal("new connection offline")
	}
}

// TestUnknownBindKeepsOriginalPayloadAfterDisconnect 保护结果未知时同操作重试内容不变，断线另行条件清理。
func TestUnknownBindKeepsOriginalPayloadAfterDisconnect(t *testing.T) {
	service := newSessionService()
	user, _ := service.begin(model.PositionCommand{OperationID: "login", Kind: "bind", Identity: model.Identity{UserID: "u", LoginID: "l", AccessNode: "access", AgentName: "a"}})
	reservation := model.RoomBinding{Identity: user.pending.target.Identity, RoomID: "r", BindingToken: "reserved"}
	original := user.pending.bindingRequest(reservation)
	service.connectionClosed(user.pending.target.Identity)
	if user.pending.connected || original != user.pending.bindingRequest(reservation) {
		t.Fatal("retry changed the unknown operation")
	}
}
