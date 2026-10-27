package main

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	attackJoinCommand     runtime.CommandID = 1
	attackHitCommand      runtime.CommandID = 2
	attackSnapshotCommand runtime.CommandID = 3
	attackLeaveCommand    runtime.CommandID = 4
	attackTickCommand     runtime.CommandID = 10
	attackBalanceCommand  runtime.CommandID = 11
	// molePushCommand 是未来接入 Agent 的业务推送入口，本轮只由测试 Agent 消费。
	molePushCommand runtime.CommandID = 100
)

type attackMember struct {
	UserID     uint64
	LoginID    string
	AccessNode string
	AgentName  string
	Seat       int64
	Membership string
}

// attackService 独占一个组；本地 Call 内存记账，不等待数据库或远端结算。
type attackService struct {
	game       attackGame
	members    map[uint64]attackMember
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*attackService)(nil)

func newAttackService(name string) *attackService {
	service := &attackService{game: newAttackGame(name), members: make(map[uint64]attackMember)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		attackJoinCommand: service.join, attackHitCommand: service.hit,
		attackSnapshotCommand: service.snapshot, attackLeaveCommand: service.leave,
		attackTickCommand: service.tick, attackBalanceCommand: service.balanceCorrection,
	})
	return service
}

func (service *attackService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	if err := self.Node().RegisterService(cluster.ServiceName(service.game.name), self.Self()); err != nil {
		return err
	}
	service.game.spawn(time.Now())
	_, err := self.After(spawnPeriod, attackTickCommand)
	return err
}

func (service *attackService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *attackService) OnStop(_ context.Context) error {
	return nil
}

func (service *attackService) join(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName, membership string) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) || !validMoleIdentifier(membership) {
		return nil, errInvalidMoleInput
	}
	member, exists := service.members[userID]
	if exists && member.LoginID == loginID && (member.AccessNode != accessNode || member.AgentName != agentName) {
		return nil, errMoleConflict
	}
	if !exists {
		if len(service.members) >= attackSeats {
			return nil, errMoleBusy
		}
		used := [attackSeats]bool{}
		for _, current := range service.members {
			used[current.Seat] = true
		}
		for seat, occupied := range used {
			if !occupied {
				member.Seat = int64(seat)
				break
			}
		}
	}
	member.UserID, member.LoginID = userID, loginID
	member.AccessNode, member.AgentName = accessNode, agentName
	member.Membership = membership
	service.members[userID] = member
	service.broadcast(self, "snapshot")
	return service.view(userID), nil
}

func (service *attackService) hit(self gsr.CommandContext, userID uint64, loginID, actionID string, sequence uint64, hole int64) (map[string]any, error) {
	_, err := service.member(userID, loginID)
	if err != nil {
		return nil, err
	}
	if !validMoleIdentifier(actionID) {
		return nil, errInvalidMoleInput
	}
	record, repeated, err := service.game.hit(time.Now(), userID, loginID, actionID, sequence, hole)
	if err != nil {
		return nil, err
	}
	event := record.Result.Event
	if record.Status == "accepted" {
		return record.Result.snapshot(), nil
	}
	if record.Status == "rejected" {
		return event.snapshot("rejected"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	value, err := self.CallName(ctx, moleAccountingName, accountingHitCommand, event, repeated)
	cancel()
	if err != nil {
		if knownMemoryRejection(err) {
			service.game.finish(record, "rejected", time.Now())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, runtime.ErrCallTimeout
		}
		return nil, err
	}
	reply, ok := value.(memoryHitResult)
	if !ok || reply.Event != event {
		return nil, errMoleOperationUnknown
	}
	record.Result = reply
	service.game.finish(record, "accepted", time.Now())
	service.broadcast(self, "snapshot")
	return reply.snapshot(), nil
}

func knownMemoryRejection(err error) bool {
	return errors.Is(err, errInvalidMoleInput) || errors.Is(err, errMoleConflict) || errors.Is(err, errMoleBusy) ||
		errors.Is(err, errAccountingUnavailable) || errors.Is(err, errInsufficientSilver) ||
		errors.Is(err, runtime.ErrMailboxFull) || errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed)
}

func (service *attackService) snapshot(_ gsr.CommandContext, userID uint64, loginID string) (map[string]any, error) {
	if _, err := service.member(userID, loginID); err != nil {
		return nil, err
	}
	return service.view(userID), nil
}

func (service *attackService) leave(self gsr.CommandContext, userID uint64, loginID string) (bool, error) {
	if _, err := service.member(userID, loginID); err != nil {
		return false, err
	}
	// 先让目录接纳清理通知；失败则保留成员，调用方可重试。
	if err := self.SendName(moleServiceName, moleDepartedCommand, userID, loginID, service.game.name, service.members[userID].Membership); err != nil {
		return false, errMoleBusy
	}
	delete(service.members, userID)
	if len(service.members) == 0 {
		self.Exit()
		return true, nil
	}
	service.broadcast(self, "snapshot")
	return true, nil
}

func (service *attackService) tick(self gsr.CommandContext) error {
	service.game.spawn(time.Now())
	service.broadcast(self, "snapshot")
	_, err := self.After(spawnPeriod, attackTickCommand)
	if err != nil {
		logMoleError(self, "地鼠 Timer 登记失败，停止当前组", err)
		self.Exit()
		return err
	}
	return nil
}

func (service *attackService) balanceCorrection(self gsr.CommandContext, userID uint64, loginID string, view map[string]any) error {
	member, err := service.member(userID, loginID)
	if err == nil {
		service.push(self, member, "balance_correction", view)
	}
	return nil
}

func (service *attackService) member(userID uint64, loginID string) (attackMember, error) {
	member, exists := service.members[userID]
	if !exists || member.LoginID != loginID {
		return attackMember{}, errMoleStaleLogin
	}
	return member, nil
}

func (service *attackService) view(userID uint64) map[string]any {
	now := time.Now()
	targets, members, hits := make([]any, 0), make([]any, 0), make([]any, 0)
	for hole, target := range service.game.targets {
		if target.Sequence != 0 && now.Before(target.Deadline) {
			targets = append(targets, map[string]any{
				"hole": int64(hole), "spawn_seq": target.Sequence,
				"deadline_ms": target.Deadline.UnixMilli(), "claimed": target.Claim != "",
			})
		}
	}
	for _, member := range service.members {
		members = append(members, map[string]any{"user_id": member.UserID, "seat": member.Seat})
	}
	for _, record := range service.game.requests {
		if record.Result.Event.UserID == userID {
			hits = append(hits, record.Result.Event.snapshot(record.Status))
		}
	}
	return map[string]any{
		"attack_id": service.game.name, "attack_name": service.game.name, "server_ms": now.UnixMilli(),
		"members": members, "targets": targets, "hits": hits, "seat": service.members[userID].Seat,
	}
}

func (service *attackService) broadcast(self gsr.ServiceContext, kind string) {
	for _, member := range service.members {
		service.push(self, member, kind, service.view(member.UserID))
	}
}

func (service *attackService) push(self gsr.ServiceContext, member attackMember, kind string, payload map[string]any) {
	if err := self.Cluster().Send(cluster.NodeID(member.AccessNode), cluster.ServiceName(member.AgentName),
		molePushCommand, member.LoginID, kind, payload); err != nil {
		logMoleError(self, "地鼠推送未接纳，客户端需取快照", err)
	}
}
