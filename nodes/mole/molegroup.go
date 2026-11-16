package main

import (
	"container/list"
	"errors"
	"time"
)

type moleMember struct {
	UserID     uint64
	LoginID    string
	AccessNode string
	AgentName  string
	Seat       int64
}

// moleGroup 是 MoleService 私有组对象，没有独立邮箱或生命周期。
type moleGroup struct {
	game         attackGame
	members      map[uint64]moleMember
	nextSpawnAt  time.Time
	spawnElement *list.Element
}

func newMoleGroup(name string, now time.Time) *moleGroup {
	group := &moleGroup{game: newAttackGame(name), members: make(map[uint64]moleMember), nextSpawnAt: now.Add(spawnPeriod)}
	group.game.spawn(now)
	return group
}
func (group *moleGroup) view(userID uint64) map[string]any {
	view := group.game.view(time.Now(), userID)
	members := make([]any, 0, len(group.members))
	for _, member := range group.members {
		members = append(members, map[string]any{"user_id": member.UserID, "seat": member.Seat})
	}
	view["members"], view["seat"] = members, group.members[userID].Seat
	return view
}
func (group *moleGroup) hit(accounting *moleAccounting, userID uint64, loginID, actionID string, sequence uint64, hole int64) (map[string]any, error) {
	if !validMoleIdentifier(actionID) {
		return nil, errInvalidMoleInput
	}
	record, repeated, err := group.game.hit(time.Now(), userID, loginID, actionID, sequence, hole)
	if err != nil {
		return nil, err
	}
	if record.Status == "accepted" {
		return record.Result.snapshot(), nil
	}
	if record.Status == "rejected" {
		return record.Result.Event.snapshot("rejected"), nil
	}
	result, err := accounting.accept(record.Result.Event, repeated)
	if err != nil {
		if errors.Is(err, errInvalidMoleInput) || errors.Is(err, errMoleConflict) || errors.Is(err, errMoleBusy) || errors.Is(err, errAccountingUnavailable) || errors.Is(err, errInsufficientSilver) {
			group.game.finish(record, "rejected", time.Now())
		}
		return nil, err
	}
	if err := group.game.acceptHit(record, result, time.Now()); err != nil {
		return nil, err
	}
	return result.snapshot(), nil
}
