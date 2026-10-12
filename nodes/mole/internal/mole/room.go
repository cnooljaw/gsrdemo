package mole

import (
	"fmt"
	"sort"
	"time"

	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
	"google.golang.org/protobuf/proto"
)

type member struct {
	userID    string
	seat      uint32
	token     string
	binding   *businesspb.RoomBinding
	connected bool
	score     int64
}

type roomOperation struct {
	request proto.Message
	reply   *businesspb.RoomReply
}
type hitOperation struct {
	request *businesspb.HitCommand
	code    string
}

// room 仅由一个 RoomService 的串行 Handler 修改，时间由调用边界提供。
type room struct {
	id          string
	cfg         model.MoleConfig
	phase       string
	version     uint64
	members     map[string]*member
	operations  map[string]roomOperation
	hits        map[string]hitOperation
	started     time.Time
	deadline    time.Time
	nextSpawn   time.Time
	targetClose time.Time
	target      *businesspb.MoleTarget
	lastHit     uint64
	spawnSeq    uint64
	result      *businesspb.RoundResult
}

func newRoom(id string, cfg model.MoleConfig) *room {
	return &room{id: id, cfg: cfg, phase: "filling", version: 1, members: make(map[string]*member), operations: make(map[string]roomOperation), hits: make(map[string]hitOperation)}
}

func (room *room) reserve(request *businesspb.AllocateRequest) *businesspb.RoomReply {
	identity := request.GetIdentity()
	if !model.ValidOperationID(request.OperationId) || identity.GetUserId() == "" || identity.GetLoginId() == "" || identity.GetLocationVersion() == 0 {
		return &businesspb.RoomReply{Code: "invalid"}
	}
	key := identity.UserId + "/reserve/" + request.OperationId
	if previous, ok := room.operations[key]; ok {
		if !proto.Equal(previous.request, request) {
			return &businesspb.RoomReply{Code: "operation_conflict"}
		}
		return proto.Clone(previous.reply).(*businesspb.RoomReply)
	}
	if room.phase != "filling" {
		return &businesspb.RoomReply{Code: "room_full"}
	}
	if _, exists := room.members[identity.UserId]; exists {
		return &businesspb.RoomReply{Code: "room_full"}
	}
	occupied := make(map[uint32]bool)
	for _, member := range room.members {
		if member.token != "" {
			occupied[member.seat] = true
		}
	}
	seat := uint32(0)
	for occupied[seat] {
		seat++
	}
	if int(seat) >= room.cfg.GroupSize {
		return &businesspb.RoomReply{Code: "room_full"}
	}
	if len(room.operations) >= 512 {
		return &businesspb.RoomReply{Code: "capacity"}
	}
	token := fmt.Sprintf("%s/reserve/%s/%d", room.id, identity.UserId, identity.LocationVersion)
	room.members[identity.UserId] = &member{userID: identity.UserId, seat: seat, token: token}
	reply := &businesspb.RoomReply{Code: "ok", Binding: &businesspb.RoomBinding{Identity: proto.Clone(identity).(*businesspb.Identity), RoomId: room.id, BindingToken: token}}
	room.operations[key] = roomOperation{request: proto.Clone(request), reply: proto.Clone(reply).(*businesspb.RoomReply)}
	return reply
}

func (room *room) bind(request *businesspb.RoomRequest, now time.Time) *businesspb.RoomReply {
	room.advance(now)
	target := request.GetTarget()
	binding := request.GetBinding()
	if !model.ValidOperationID(request.OperationId) || binding.GetRoomId() != room.id || target.GetUserId() == "" || target.GetLoginId() == "" || target.GetAccessNode() == "" || target.GetAgentName() == "" || target.GetLocationVersion() == 0 || target.UserId != binding.GetIdentity().GetUserId() {
		return &businesspb.RoomReply{Code: "invalid"}
	}
	key := target.UserId + "/bind/" + request.OperationId
	if previous, ok := room.operations[key]; ok {
		if !proto.Equal(previous.request, request) {
			return &businesspb.RoomReply{Code: "operation_conflict"}
		}
		member := room.members[target.UserId]
		if member == nil || !model.SameBinding(member.binding, previous.reply.Binding) {
			return &businesspb.RoomReply{Code: "binding_replaced"}
		}
		reply := proto.Clone(previous.reply).(*businesspb.RoomReply)
		reply.Snapshot = room.snapshot()
		return reply
	}
	member := room.members[target.UserId]
	if member == nil || member.token == "" || member.token != binding.BindingToken {
		return &businesspb.RoomReply{Code: "binding_replaced"}
	}
	if member.binding != nil && (!model.SameBinding(member.binding, binding) || target.LocationVersion <= member.binding.Identity.LocationVersion) {
		return &businesspb.RoomReply{Code: "binding_replaced"}
	}
	if len(room.operations) >= 512 {
		return &businesspb.RoomReply{Code: "capacity"}
	}
	member.token = fmt.Sprintf("%s/bound/%s/%d", room.id, target.LoginId, target.LocationVersion)
	member.binding = &businesspb.RoomBinding{RoomId: room.id, BindingToken: member.token, Identity: proto.Clone(target).(*businesspb.Identity)}
	member.connected = request.Connected
	room.version++
	if room.phase == "filling" && room.boundCount() == room.cfg.GroupSize {
		room.phase = "running"
		room.started = now
		room.deadline = now.Add(time.Duration(room.cfg.RoundMS) * time.Millisecond)
		room.nextSpawn = now
		room.advance(now)
	}
	reply := &businesspb.RoomReply{Code: "ok", Binding: proto.Clone(member.binding).(*businesspb.RoomBinding), Snapshot: room.snapshot()}
	room.operations[key] = roomOperation{request: proto.Clone(request), reply: proto.Clone(reply).(*businesspb.RoomReply)}
	return reply
}

func (room *room) boundCount() int {
	count := 0
	for _, member := range room.members {
		if member.binding != nil {
			count++
		}
	}
	return count
}

func (room *room) check(binding *businesspb.RoomBinding) string {
	member := room.members[binding.GetIdentity().GetUserId()]
	if member == nil || !model.SameBinding(member.binding, binding) {
		return "binding_replaced"
	}
	return "ok"
}

func (room *room) remove(request *businesspb.RoomRequest, now time.Time) *businesspb.RoomReply {
	room.advance(now)
	key := request.GetBinding().GetIdentity().GetUserId() + "/leave/" + request.OperationId
	if previous, ok := room.operations[key]; ok {
		if !proto.Equal(previous.request, request) {
			return &businesspb.RoomReply{Code: "operation_conflict"}
		}
		return proto.Clone(previous.reply).(*businesspb.RoomReply)
	}
	if code := room.check(request.Binding); code != "ok" {
		return &businesspb.RoomReply{Code: code}
	}
	if room.phase == "running" {
		return &businesspb.RoomReply{Code: "state_mismatch", Snapshot: room.snapshot()}
	}
	if room.phase == "ended" {
		return &businesspb.RoomReply{Code: "settlement_pending", Snapshot: room.snapshot()}
	}
	if !model.ValidOperationID(request.OperationId) {
		return &businesspb.RoomReply{Code: "invalid"}
	}
	if len(room.operations) >= 512 {
		return &businesspb.RoomReply{Code: "capacity"}
	}
	member := room.members[request.Binding.Identity.UserId]
	member.binding = nil
	member.token = "" // 保留成员墓碑，不能让迟到的初次绑定复活。
	member.connected = false
	room.version++
	reply := &businesspb.RoomReply{Code: "ok", Snapshot: room.snapshot()}
	room.operations[key] = roomOperation{request: proto.Clone(request), reply: proto.Clone(reply).(*businesspb.RoomReply)}
	return reply
}

func (room *room) disconnect(binding *businesspb.RoomBinding, now time.Time) {
	room.advance(now)
	if room.check(binding) != "ok" {
		return
	}
	member := room.members[binding.Identity.UserId]
	if member.connected {
		member.connected = false
		room.version++
	}
}

func (room *room) hit(request *businesspb.HitCommand, now time.Time) *businesspb.MoleHitResponse {
	room.advance(now)
	response := &businesspb.MoleHitResponse{OperationId: request.GetHit().GetOperationId(), Snapshot: room.snapshot()}
	if code := room.check(request.Binding); code != "ok" {
		response.Code = code
		return response
	}
	hit := request.GetHit()
	if !model.ValidOperationID(hit.GetOperationId()) {
		response.Code = "invalid"
		return response
	}
	key := request.Binding.Identity.UserId + "/" + hit.OperationId
	if previous, ok := room.hits[key]; ok {
		response.Code = previous.code
		if !proto.Equal(previous.request, request) {
			response.Code = "operation_conflict"
		}
		return response
	}
	if len(room.hits) >= 512 {
		response.Code = "capacity"
		return response
	}
	switch {
	case hit.RoomId != room.id || hit.RoundId != room.id+"/1":
		response.Code = "state_mismatch"
	case room.phase != "running":
		response.Code = "state_mismatch"
	case hit.InstanceId == room.lastHit && room.lastHit != 0:
		response.Code = "already_hit"
	case room.target == nil || room.target.InstanceId != hit.InstanceId:
		response.Code = "target_expired"
	default:
		member := room.members[request.Binding.Identity.UserId]
		member.score += 10
		room.lastHit = hit.InstanceId
		room.target = nil
		room.nextSpawn = now.Add(time.Duration(room.cfg.GapMS) * time.Millisecond)
		room.version++
		response.Code = "ok"
	}
	room.hits[key] = hitOperation{request: proto.Clone(request).(*businesspb.HitCommand), code: response.Code}
	response.Snapshot = room.snapshot()
	return response
}

func (room *room) advance(now time.Time) {
	if room.phase != "running" {
		return
	}
	if !now.Before(room.deadline) {
		room.phase = "ended"
		room.target = nil
		room.result = &businesspb.RoundResult{SettlementId: room.id + "/settlement", Progress: "unpersisted"}
		for _, member := range room.orderedMembers() {
			room.result.Deltas = append(room.result.Deltas, &businesspb.PlayerDelta{UserId: member.userID, Delta: member.score})
		}
		room.version++
		return
	}
	if room.target != nil && !now.Before(room.targetClose) {
		room.target = nil
		room.nextSpawn = room.targetClose.Add(time.Duration(room.cfg.GapMS) * time.Millisecond)
		room.version++
	}
	if room.target == nil && !now.Before(room.nextSpawn) {
		// 直接定位当前周期，避免巡检延迟后补跑大量已经过期的生成步骤。
		period := time.Duration(room.cfg.WindowMS+room.cfg.GapMS) * time.Millisecond
		skipped := now.Sub(room.nextSpawn) / period
		opened := room.nextSpawn.Add(skipped * period)
		room.spawnSeq += uint64(skipped) + 1
		room.targetClose = opened.Add(time.Duration(room.cfg.WindowMS) * time.Millisecond)
		room.nextSpawn = opened.Add(period)
		if now.Before(room.targetClose) {
			room.target = &businesspb.MoleTarget{InstanceId: room.spawnSeq, Hole: uint32((room.spawnSeq - 1) % 9), OpensAtMs: opened.UnixMilli(), ClosesAtMs: room.targetClose.UnixMilli()}
		}
		room.version++
	}
}

func (room *room) orderedMembers() []*member {
	members := make([]*member, 0, len(room.members))
	for _, member := range room.members {
		if member.binding != nil {
			members = append(members, member)
		}
	}
	sort.Slice(members, func(first, second int) bool { return members[first].seat < members[second].seat })
	return members
}

func (room *room) snapshot() *businesspb.MoleSnapshot {
	snapshot := &businesspb.MoleSnapshot{RoomId: room.id, Phase: room.phase, StateVersion: room.version}
	if !room.started.IsZero() {
		snapshot.RoundId = room.id + "/1"
		snapshot.EndsAtMs = room.deadline.UnixMilli()
	}
	for _, member := range room.orderedMembers() {
		snapshot.Members = append(snapshot.Members, &businesspb.MemberView{UserId: member.userID, Seat: member.seat, Connected: member.connected, Score: member.score})
	}
	if room.target != nil {
		snapshot.Target = proto.Clone(room.target).(*businesspb.MoleTarget)
	}
	if room.result != nil {
		snapshot.Result = proto.Clone(room.result).(*businesspb.RoundResult)
	}
	return snapshot
}
