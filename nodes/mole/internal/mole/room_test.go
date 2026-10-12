package mole

import (
	"testing"
	"time"

	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
	"google.golang.org/protobuf/proto"
)

func join(t *testing.T, room *room, user, login string, now time.Time) *businesspb.RoomBinding {
	t.Helper()
	identity := &businesspb.Identity{UserId: user, LoginId: login, AccessNode: "access", AgentName: login, LocationVersion: 1}
	reserved := room.reserve(&businesspb.AllocateRequest{OperationId: "enter-" + user, Identity: identity})
	if reserved.Code != "ok" {
		t.Fatal(reserved)
	}
	bound := room.bind(&businesspb.RoomRequest{OperationId: "enter-" + user, Binding: reserved.Binding, Target: identity, Connected: true}, now)
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
	if room.snapshot().Phase != "filling" || room.snapshot().Target != nil {
		t.Fatal("started before full")
	}
	third := join(t, room, "u3", "l3", now)
	snapshot := room.snapshot()
	if snapshot.Phase != "running" || snapshot.Target == nil {
		t.Fatal(snapshot)
	}
	hit := &businesspb.HitCommand{Binding: first, Hit: &businesspb.MoleHitRequest{OperationId: "hit1", RoomId: "room", RoundId: snapshot.RoundId, InstanceId: snapshot.Target.InstanceId}}
	if room.hit(hit, now).Code != "ok" || room.hit(hit, now).Code != "ok" {
		t.Fatal("hit retry")
	}
	competitor := proto.Clone(hit).(*businesspb.HitCommand)
	competitor.Binding = second
	competitor.Hit.OperationId = "hit2"
	if room.hit(competitor, now).Code != "already_hit" {
		t.Fatal("two winners")
	}
	conflict := proto.Clone(hit).(*businesspb.HitCommand)
	conflict.Hit.InstanceId++
	if room.hit(conflict, now).Code != "operation_conflict" {
		t.Fatal("changed retry")
	}
	room.disconnect(third, now)
	room.advance(now.Add(40 * time.Second))
	ended := room.snapshot()
	if ended.Phase != "ended" || len(ended.Result.Deltas) != 3 || ended.Result.Progress != "unpersisted" {
		t.Fatal(ended)
	}
	for _, delta := range ended.Result.Deltas {
		expected := int64(0)
		if delta.UserId == "u1" {
			expected = 10
		}
		if delta.Delta != expected {
			t.Fatal(delta)
		}
	}
	if room.remove(&businesspb.RoomRequest{OperationId: "leave", Binding: first}, now.Add(40*time.Second)).Code != "settlement_pending" {
		t.Fatal("unsettled member removed")
	}
}

// TestRoomRejectsOldBindingAndExpiredTarget 保护条件换绑、旧断线和处理时刻的窗口校验。
func TestRoomRejectsOldBindingAndExpiredTarget(t *testing.T) {
	now := time.Now()
	room := newRoom("r", model.DefaultMoleConfig())
	old := join(t, room, "u1", "l1", now)
	request := &businesspb.RoomRequest{OperationId: "reconnect", Binding: old, Target: &businesspb.Identity{UserId: "u1", LoginId: "l2", AccessNode: "access", AgentName: "l2", LocationVersion: 2}, Connected: true}
	fresh := room.bind(request, now)
	if fresh.Code != "ok" {
		t.Fatal(fresh)
	}
	delayed := proto.Clone(request).(*businesspb.RoomRequest)
	delayed.OperationId = "late"
	delayed.Target.LoginId = "l3"
	delayed.Target.LocationVersion = 3
	if room.bind(delayed, now).Code != "binding_replaced" {
		t.Fatal("late bind replaced fresh")
	}
	room.disconnect(old, now)
	if !room.snapshot().Members[0].Connected {
		t.Fatal("old disconnect changed new binding")
	}
	join(t, room, "u2", "l4", now)
	join(t, room, "u3", "l5", now)
	snapshot := room.snapshot()
	hit := &businesspb.HitCommand{Binding: fresh.Binding, Hit: &businesspb.MoleHitRequest{OperationId: "late-hit", RoomId: "r", RoundId: snapshot.RoundId, InstanceId: snapshot.Target.InstanceId}}
	if room.hit(hit, now.Add(1100*time.Millisecond)).Code == "ok" {
		t.Fatal("expired target rewarded")
	}
	before := room.snapshot()
	hit.Binding = old
	if room.hit(hit, now.Add(1100*time.Millisecond)).Code != "binding_replaced" || !proto.Equal(before, room.snapshot()) {
		t.Fatal("stale binding changed room")
	}
}

// TestFillingRoomReusesSeatWithoutRevivingOldBinding 保护等待阶段离开和座位复用的迟到命令边界。
func TestFillingRoomReusesSeatWithoutRevivingOldBinding(t *testing.T) {
	now := time.Now()
	room := newRoom("r", model.DefaultMoleConfig())
	old := join(t, room, "u1", "l1", now)
	if room.remove(&businesspb.RoomRequest{OperationId: "leave", Binding: old}, now).Code != "ok" {
		t.Fatal("filling leave")
	}
	join(t, room, "u2", "l2", now)
	if room.snapshot().Members[0].Seat != 0 {
		t.Fatal("seat not reused")
	}
	if room.bind(&businesspb.RoomRequest{OperationId: "late", Binding: old, Target: old.Identity, Connected: true}, now).Code != "binding_replaced" {
		t.Fatal("removed binding revived")
	}
}
