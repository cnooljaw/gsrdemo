package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

const (
	attackSeats = 3
	attackHoles = 9
	spawnPeriod = time.Second
	targetLife  = 2 * time.Second
	maxRequests = 1024
)

type moleTarget struct {
	Sequence uint64
	Deadline time.Time
	Claim    string
}

// hitEvent 只保存记账事实和组来源，不携带登录身份或连接地址。
type hitEvent struct {
	HitID      string
	UserID     uint64
	ActionID   string
	AttackName string
	Sequence   uint64
	Hole       int64
	Debit      int64
	Credit     int64
	Rule       string
}

type hitRequest struct {
	UserID  uint64
	LoginID string
	Action  string
}

type hitRecord struct {
	Result   memoryHitResult
	Status   string
	Deadline time.Time
}

// attackGame 是 AttackService 私有的规则状态；不访问数据库、网络或钱包。
type attackGame struct {
	name       string
	sequence   uint64
	hitCounter uint64
	targets    [attackHoles]moleTarget
	requests   map[hitRequest]*hitRecord
}

func newAttackGame(name string) attackGame {
	return attackGame{name: name, requests: make(map[hitRequest]*hitRecord)}
}

func (game *attackGame) spawn(now time.Time) {
	if game.sequence == math.MaxUint64 {
		return
	}
	game.sequence++
	hole := (game.sequence - 1) % attackHoles
	game.targets[hole] = moleTarget{Sequence: game.sequence, Deadline: now.Add(targetLife)}
	for key, record := range game.requests {
		if record.Status != "pending" && !now.Before(record.Deadline) {
			delete(game.requests, key)
		}
	}
}

func (game *attackGame) hit(now time.Time, userID uint64, loginID, action string, sequence uint64, hole int64) (*hitRecord, bool, error) {
	key := hitRequest{UserID: userID, LoginID: loginID, Action: action}
	if record, ok := game.requests[key]; ok {
		if record.Result.Event.Sequence != sequence || record.Result.Event.Hole != hole {
			return nil, false, errMoleConflict
		}
		return record, true, nil
	}
	if hole < 0 || hole >= attackHoles || sequence == 0 {
		return nil, false, errInvalidMoleInput
	}
	target := &game.targets[hole]
	if target.Sequence != sequence || !now.Before(target.Deadline) || target.Claim != "" {
		return nil, false, errTargetUnavailable
	}
	if len(game.requests) >= maxRequests || game.hitCounter == math.MaxUint64 {
		return nil, false, errMoleBusy
	}
	game.hitCounter++
	event := hitEvent{
		HitID: fmt.Sprintf("%s-%d", game.name, game.hitCounter), UserID: userID,
		ActionID: action, AttackName: game.name,
		Sequence: sequence, Hole: hole, Debit: hammerCost, Rule: moleRuleVersion,
	}
	if rand.IntN(2) == 1 {
		event.Credit = hammerReward
	}
	record := &hitRecord{Result: memoryHitResult{Event: event}, Status: "pending", Deadline: target.Deadline}
	game.requests[key] = record
	target.Claim = event.HitID
	return record, false, nil
}

// finish 只处理本组已取得的记录，不再按 HitID 反查全部请求。
func (game *attackGame) finish(record *hitRecord, status string, now time.Time) {
	if record.Status != "pending" {
		return
	}
	record.Status = status
	event := record.Result.Event
	target := &game.targets[event.Hole]
	if status == "rejected" && target.Claim == event.HitID && target.Sequence == event.Sequence && now.Before(target.Deadline) {
		target.Claim = ""
	}
}

func (event hitEvent) snapshot(status string) map[string]any {
	return map[string]any{
		"hit_id": event.HitID, "action_id": event.ActionID, "status": status,
		"debit": event.Debit, "credit": event.Credit, "spawn_seq": event.Sequence, "hole": event.Hole,
	}
}
