package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsrdemo/common/model"
)

func join(t *testing.T, room *room, user, login string, now time.Time) model.RoomBinding {
	t.Helper()
	identity := model.Identity{UserID: user, LoginID: login, AccessNode: "access", AgentName: login, LocationVersion: 1}
	reserved := room.reserve(allocateRequest{OperationID: "enter-" + user, Identity: identity})
	if reserved.Code != "ok" {
		t.Fatal(reserved)
	}
	bound := room.bind(model.RoomRequest{OperationID: "enter-" + user, Binding: reserved.Binding, Target: identity, Connected: true}, now)
	if bound.Code != "ok" {
		t.Fatal(bound)
	}
	return bound.Binding
}

// TestRoomStartsOnlyWhenFullAndRewardsOneWinner 保护三人共享目标、竞争命中、重试和全组冻结结果。
func TestRoomStartsOnlyWhenFullAndRewardsOneWinner(t *testing.T) {
	now := time.Now()
	room := newRoom("room", model.DefaultMoleConfig())
	first := join(t, room, "u1", "l1", now)
	second := join(t, room, "u2", "l2", now)
	if room.snapshot()["phase"] != "filling" || room.snapshot()["target"] != nil {
		t.Fatal("started before full")
	}
	third := join(t, room, "u3", "l3", now)
	snapshot := room.snapshot()
	if snapshot["phase"] != "running" || snapshot["target"] == nil {
		t.Fatal(snapshot)
	}
	target := snapshot["target"].(map[string]any)
	hit := hitRequest{Binding: first, OperationID: "hit1", RoomID: "room", RoundID: snapshot["round_id"].(string), InstanceID: target["instance_id"].(uint64)}
	if room.hit(hit, now).Code != "ok" || room.hit(hit, now).Code != "ok" {
		t.Fatal("hit retry")
	}
	competitor := hit
	competitor.Binding = second
	competitor.OperationID = "hit2"
	if room.hit(competitor, now).Code != "already_hit" {
		t.Fatal("two winners")
	}
	conflict := hit
	conflict.InstanceID++
	if room.hit(conflict, now).Code != "operation_conflict" {
		t.Fatal("changed retry")
	}
	room.disconnect(third, now)
	room.advance(now.Add(40 * time.Second))
	ended := room.snapshot()
	result := ended["result"].(map[string]any)
	deltas := result["deltas"].([]any)
	if ended["phase"] != "ended" || len(deltas) != 3 || result["progress"] != "unpersisted" {
		t.Fatal(ended)
	}
	for _, value := range deltas {
		delta := value.(map[string]any)
		expected := int64(0)
		if delta["user_id"] == "u1" {
			expected = 10
		}
		if delta["delta"] != expected {
			t.Fatal(delta)
		}
	}
	if room.remove(model.RoomRequest{OperationID: "leave", Binding: first}, now.Add(40*time.Second)).Code != "settlement_pending" {
		t.Fatal("unsettled member removed")
	}
}

// TestRoomRejectsOldBindingAndExpiredTarget 保护条件换绑、旧断线和处理时刻的窗口校验。
func TestRoomRejectsOldBindingAndExpiredTarget(t *testing.T) {
	now := time.Now()
	room := newRoom("r", model.DefaultMoleConfig())
	old := join(t, room, "u1", "l1", now)
	request := model.RoomRequest{OperationID: "reconnect", Binding: old, Target: model.Identity{UserID: "u1", LoginID: "l2", AccessNode: "access", AgentName: "l2", LocationVersion: 2}, Connected: true}
	fresh := room.bind(request, now)
	if fresh.Code != "ok" {
		t.Fatal(fresh)
	}
	delayed := request
	delayed.OperationID = "late"
	delayed.Target.LoginID = "l3"
	delayed.Target.LocationVersion = 3
	if room.bind(delayed, now).Code != "binding_replaced" {
		t.Fatal("late bind replaced fresh")
	}
	room.disconnect(old, now)
	if !room.snapshot()["members"].([]any)[0].(map[string]any)["connected"].(bool) {
		t.Fatal("old disconnect changed new binding")
	}
	join(t, room, "u2", "l4", now)
	join(t, room, "u3", "l5", now)
	snapshot := room.snapshot()
	target := snapshot["target"].(map[string]any)
	hit := hitRequest{Binding: fresh.Binding, OperationID: "late-hit", RoomID: "r", RoundID: snapshot["round_id"].(string), InstanceID: target["instance_id"].(uint64)}
	if room.hit(hit, now.Add(1100*time.Millisecond)).Code == "ok" {
		t.Fatal("expired target rewarded")
	}
	before := room.snapshot()
	hit.Binding = old
	if room.hit(hit, now.Add(1100*time.Millisecond)).Code != "binding_replaced" || !reflect.DeepEqual(before, room.snapshot()) {
		t.Fatal("stale binding changed room")
	}
}

// TestFillingRoomReusesSeatWithoutRevivingOldBinding 保护等待阶段离开和座位复用的迟到命令边界。
func TestFillingRoomReusesSeatWithoutRevivingOldBinding(t *testing.T) {
	now := time.Now()
	room := newRoom("r", model.DefaultMoleConfig())
	old := join(t, room, "u1", "l1", now)
	if room.remove(model.RoomRequest{OperationID: "leave", Binding: old}, now).Code != "ok" {
		t.Fatal("filling leave")
	}
	join(t, room, "u2", "l2", now)
	if room.snapshot()["members"].([]any)[0].(map[string]any)["seat"] != uint64(0) {
		t.Fatal("seat not reused")
	}
	if room.bind(model.RoomRequest{OperationID: "late", Binding: old, Target: old.Identity, Connected: true}, now).Code != "binding_replaced" {
		t.Fatal("removed binding revived")
	}
}

// TestSnapshotDoesNotExposeRoomState 保护本地调用和重试得到的快照无法修改 Room 持有的数据。
func TestSnapshotDoesNotExposeRoomState(t *testing.T) {
	now := time.Now()
	room := newRoom("r", model.DefaultMoleConfig())
	join(t, room, "u1", "l1", now)
	join(t, room, "u2", "l2", now)
	join(t, room, "u3", "l3", now)
	before := room.snapshot()
	borrowed := room.snapshot()
	borrowed["members"].([]any)[0].(map[string]any)["score"] = int64(999)
	borrowed["target"].(map[string]any)["instance_id"] = uint64(999)
	if !reflect.DeepEqual(before, room.snapshot()) {
		t.Fatal("snapshot mutation changed room")
	}
	room.advance(now.Add(40 * time.Second))
	before = room.snapshot()
	borrowed = room.snapshot()
	borrowed["result"].(map[string]any)["deltas"].([]any)[0].(map[string]any)["delta"] = int64(999)
	if !reflect.DeepEqual(before, room.snapshot()) {
		t.Fatal("snapshot mutation changed frozen result")
	}
}
