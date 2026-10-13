package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/cnooljaw/gsrdemo/common/model"
)

type member struct {
	userID    string
	seat      uint64
	token     string
	binding   model.RoomBinding
	connected bool
	score     int64
}

type roomOperation struct {
	request any // 仅保存可比较的 allocateRequest 或 model.RoomRequest 值。
	reply   model.RoomReply
}
type hitOperation struct {
	request hitRequest
	code    string
}

type allocateRequest struct {
	OperationID string
	Identity    model.Identity
}
type hitRequest struct {
	Binding                      model.RoomBinding
	OperationID, RoomID, RoundID string
	InstanceID                   uint64
}
type target struct {
	instanceID, hole      uint64
	opensAtMS, closesAtMS int64
}
type playerDelta struct {
	userID string
	delta  int64
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
	target      *target
	lastHit     uint64
	spawnSeq    uint64
	result      []playerDelta
}

func newRoom(id string, cfg model.MoleConfig) *room {
	return &room{id: id, cfg: cfg, phase: "filling", version: 1, members: make(map[string]*member), operations: make(map[string]roomOperation), hits: make(map[string]hitOperation)}
}

func (room *room) reserve(request allocateRequest) model.RoomReply {
	identity := request.Identity
	if !model.ValidOperationID(request.OperationID) || identity.UserID == "" || identity.LoginID == "" || identity.LocationVersion == 0 {
		return model.RoomReply{Code: "invalid"}
	}
	key := identity.UserID + "/reserve/" + request.OperationID
	if previous, ok := room.operations[key]; ok {
		if previous.request != request {
			return model.RoomReply{Code: "operation_conflict"}
		}
		return previous.reply
	}
	if room.phase != "filling" {
		return model.RoomReply{Code: "room_full"}
	}
	if _, exists := room.members[identity.UserID]; exists {
		return model.RoomReply{Code: "room_full"}
	}
	occupied := make(map[uint64]bool)
	for _, member := range room.members {
		if member.token != "" {
			occupied[member.seat] = true
		}
	}
	seat := uint64(0)
	for occupied[seat] {
		seat++
	}
	if int(seat) >= room.cfg.GroupSize {
		return model.RoomReply{Code: "room_full"}
	}
	if len(room.operations) >= 512 {
		return model.RoomReply{Code: "capacity"}
	}
	token := fmt.Sprintf("%s/reserve/%s/%d", room.id, identity.UserID, identity.LocationVersion)
	room.members[identity.UserID] = &member{userID: identity.UserID, seat: seat, token: token}
	reply := model.RoomReply{Code: "ok", Binding: model.RoomBinding{Identity: identity, RoomID: room.id, BindingToken: token}}
	room.operations[key] = roomOperation{request: request, reply: model.RoomReply{Code: reply.Code, Binding: reply.Binding}}
	return reply
}

func (room *room) bind(request model.RoomRequest, now time.Time) model.RoomReply {
	room.advance(now)
	target := request.Target
	binding := request.Binding
	if !model.ValidOperationID(request.OperationID) || binding.RoomID != room.id || target.UserID == "" || target.LoginID == "" || target.AccessNode == "" || target.AgentName == "" || target.LocationVersion == 0 || target.UserID != binding.Identity.UserID {
		return model.RoomReply{Code: "invalid"}
	}
	key := target.UserID + "/bind/" + request.OperationID
	if previous, ok := room.operations[key]; ok {
		if previous.request != request {
			return model.RoomReply{Code: "operation_conflict"}
		}
		member := room.members[target.UserID]
		if member == nil || !model.SameBinding(member.binding, previous.reply.Binding) {
			return model.RoomReply{Code: "binding_replaced"}
		}
		reply := previous.reply
		reply.Snapshot = room.snapshot()
		return reply
	}
	member := room.members[target.UserID]
	if member == nil || member.token == "" || member.token != binding.BindingToken {
		return model.RoomReply{Code: "binding_replaced"}
	}
	if member.binding.RoomID != "" && (!model.SameBinding(member.binding, binding) || target.LocationVersion <= member.binding.Identity.LocationVersion) {
		return model.RoomReply{Code: "binding_replaced"}
	}
	if len(room.operations) >= 512 {
		return model.RoomReply{Code: "capacity"}
	}
	member.token = fmt.Sprintf("%s/bound/%s/%d", room.id, target.LoginID, target.LocationVersion)
	member.binding = model.RoomBinding{RoomID: room.id, BindingToken: member.token, Identity: target}
	member.connected = request.Connected
	room.version++
	if room.phase == "filling" && room.boundCount() == room.cfg.GroupSize {
		room.phase = "running"
		room.started = now
		room.deadline = now.Add(time.Duration(room.cfg.RoundMS) * time.Millisecond)
		room.nextSpawn = now
		room.advance(now)
	}
	reply := model.RoomReply{Code: "ok", Binding: member.binding, Snapshot: room.snapshot()}
	room.operations[key] = roomOperation{request: request, reply: model.RoomReply{Code: reply.Code, Binding: reply.Binding}}
	return reply
}

func (room *room) boundCount() int {
	count := 0
	for _, member := range room.members {
		if member.binding.RoomID != "" {
			count++
		}
	}
	return count
}

func (room *room) check(binding model.RoomBinding) string {
	member := room.members[binding.Identity.UserID]
	if member == nil || !model.SameBinding(member.binding, binding) {
		return "binding_replaced"
	}
	return "ok"
}

func (room *room) remove(request model.RoomRequest, now time.Time) model.RoomReply {
	room.advance(now)
	key := request.Binding.Identity.UserID + "/leave/" + request.OperationID
	if previous, ok := room.operations[key]; ok {
		if previous.request != request {
			return model.RoomReply{Code: "operation_conflict"}
		}
		return previous.reply
	}
	if code := room.check(request.Binding); code != "ok" {
		return model.RoomReply{Code: code}
	}
	if room.phase == "running" {
		return model.RoomReply{Code: "state_mismatch", Snapshot: room.snapshot()}
	}
	if room.phase == "ended" {
		return model.RoomReply{Code: "settlement_pending", Snapshot: room.snapshot()}
	}
	if !model.ValidOperationID(request.OperationID) {
		return model.RoomReply{Code: "invalid"}
	}
	if len(room.operations) >= 512 {
		return model.RoomReply{Code: "capacity"}
	}
	member := room.members[request.Binding.Identity.UserID]
	member.binding = model.RoomBinding{}
	member.token = "" // 保留成员墓碑，不能让迟到的初次绑定复活。
	member.connected = false
	room.version++
	reply := model.RoomReply{Code: "ok", Snapshot: room.snapshot()}
	room.operations[key] = roomOperation{request: request, reply: model.RoomReply{Code: reply.Code, Binding: reply.Binding}}
	return reply
}

func (room *room) disconnect(binding model.RoomBinding, now time.Time) {
	room.advance(now)
	if room.check(binding) != "ok" {
		return
	}
	member := room.members[binding.Identity.UserID]
	if member.connected {
		member.connected = false
		room.version++
	}
}

func (room *room) hit(request hitRequest, now time.Time) model.RoomReply {
	room.advance(now)
	response := model.RoomReply{Snapshot: room.snapshot()}
	if code := room.check(request.Binding); code != "ok" {
		response.Code = code
		return response
	}
	hit := request
	if !model.ValidOperationID(hit.OperationID) {
		response.Code = "invalid"
		return response
	}
	key := request.Binding.Identity.UserID + "/" + hit.OperationID
	if previous, ok := room.hits[key]; ok {
		response.Code = previous.code
		if previous.request != request {
			response.Code = "operation_conflict"
		}
		return response
	}
	if len(room.hits) >= 512 {
		response.Code = "capacity"
		return response
	}
	switch {
	case hit.RoomID != room.id || hit.RoundID != room.id+"/1":
		response.Code = "state_mismatch"
	case room.phase != "running":
		response.Code = "state_mismatch"
	case hit.InstanceID == room.lastHit && room.lastHit != 0:
		response.Code = "already_hit"
	case room.target == nil || room.target.instanceID != hit.InstanceID:
		response.Code = "target_expired"
	default:
		member := room.members[request.Binding.Identity.UserID]
		member.score += 10
		room.lastHit = hit.InstanceID
		room.target = nil
		room.nextSpawn = now.Add(time.Duration(room.cfg.GapMS) * time.Millisecond)
		room.version++
		response.Code = "ok"
	}
	room.hits[key] = hitOperation{request: request, code: response.Code}
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
		room.result = make([]playerDelta, 0, len(room.members))
		for _, member := range room.orderedMembers() {
			room.result = append(room.result, playerDelta{userID: member.userID, delta: member.score})
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
			room.target = &target{instanceID: room.spawnSeq, hole: (room.spawnSeq - 1) % 9, opensAtMS: opened.UnixMilli(), closesAtMS: room.targetClose.UnixMilli()}
		}
		room.version++
	}
}

func (room *room) orderedMembers() []*member {
	members := make([]*member, 0, len(room.members))
	for _, member := range room.members {
		if member.binding.RoomID != "" {
			members = append(members, member)
		}
	}
	sort.Slice(members, func(first, second int) bool { return members[first].seat < members[second].seat })
	return members
}

// snapshot 每次新建所有 map/list，不把房间内的可变数据交给其它 Service。
func (room *room) snapshot() map[string]any {
	members := make([]any, 0, room.boundCount())
	for _, member := range room.orderedMembers() {
		members = append(members, map[string]any{"user_id": member.userID, "seat": member.seat, "connected": member.connected, "score": member.score})
	}
	snapshot := map[string]any{
		"room_id": room.id, "round_id": "", "phase": room.phase, "state_version": room.version,
		"members": members, "ends_at_ms": int64(0),
	}
	if !room.started.IsZero() {
		snapshot["round_id"] = room.id + "/1"
		snapshot["ends_at_ms"] = room.deadline.UnixMilli()
	}
	if room.target != nil {
		snapshot["target"] = map[string]any{
			"instance_id": room.target.instanceID, "hole": room.target.hole,
			"opens_at_ms": room.target.opensAtMS, "closes_at_ms": room.target.closesAtMS,
		}
	}
	if room.result != nil {
		deltas := make([]any, 0, len(room.result))
		for _, delta := range room.result {
			deltas = append(deltas, map[string]any{"user_id": delta.userID, "delta": delta.delta})
		}
		snapshot["result"] = map[string]any{"settlement_id": room.id + "/settlement", "progress": "unpersisted", "deltas": deltas}
	}
	return snapshot
}
