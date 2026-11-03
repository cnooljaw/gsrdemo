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
	// 接入 Agent 按独立 CommandID 接收快照与余额校正。
	moleSnapshotPushCommand          runtime.CommandID = 100
	moleBalanceCorrectionPushCommand runtime.CommandID = 102
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
		attackJoinCommand:     service.handleJoin,
		attackHitCommand:      service.handleHit,
		attackSnapshotCommand: service.handleSnapshot,
		attackLeaveCommand:    service.handleLeave,
		attackTickCommand:     service.handleTick,
		attackBalanceCommand:  service.handleBalanceCorrection,
	})
	return service
}

func (service *attackService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if err := self.Node().RegisterService(cluster.ServiceName(service.game.name), self.Self()); err != nil {
		return err
	}
	service.game.spawn(time.Now())
	return startMolePulse(ctx, self, spawnPeriod, attackTickCommand, nil)
}

func (service *attackService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *attackService) OnStop(_ context.Context) error {
	return nil
}

func (service *attackService) handleJoin(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName, membership string) (map[string]any, error) {
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
	service.broadcastSnapshot(self)
	return service.view(userID), nil
}

func (service *attackService) handleHit(self gsr.CommandContext, userID uint64, loginID, actionID string, sequence uint64, hole int64) (map[string]any, error) {
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
	if !ok {
		return nil, errMoleOperationUnknown
	}
	if err := service.game.acceptHit(record, reply, time.Now()); err != nil {
		return nil, err
	}
	service.broadcastSnapshot(self)
	return reply.snapshot(), nil
}

func knownMemoryRejection(err error) bool {
	return errors.Is(err, errInvalidMoleInput) || errors.Is(err, errMoleConflict) || errors.Is(err, errMoleBusy) ||
		errors.Is(err, errAccountingUnavailable) || errors.Is(err, errInsufficientSilver) ||
		errors.Is(err, runtime.ErrMailboxFull) || errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed)
}

func (service *attackService) handleSnapshot(_ gsr.CommandContext, userID uint64, loginID string) (map[string]any, error) {
	if _, err := service.member(userID, loginID); err != nil {
		return nil, err
	}
	return service.view(userID), nil
}

func (service *attackService) handleLeave(self gsr.CommandContext, userID uint64, loginID string) (bool, error) {
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
	service.broadcastSnapshot(self)
	return true, nil
}

func (service *attackService) handleTick(self gsr.CommandContext) error {
	service.game.spawn(time.Now())
	service.broadcastSnapshot(self)
	return nil
}

func (service *attackService) handleBalanceCorrection(self gsr.CommandContext, userID uint64, loginID string, view map[string]any) error {
	member, err := service.member(userID, loginID)
	if err == nil {
		service.push(self, member, moleBalanceCorrectionPushCommand, view)
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
	view := service.game.view(time.Now(), userID)
	members := make([]any, 0, len(service.members))
	for _, member := range service.members {
		members = append(members, map[string]any{"user_id": member.UserID, "seat": member.Seat})
	}
	view["members"], view["seat"] = members, service.members[userID].Seat
	return view
}

func (service *attackService) broadcastSnapshot(self gsr.ServiceContext) {
	for _, member := range service.members {
		service.push(self, member, moleSnapshotPushCommand, service.view(member.UserID))
	}
}

func (service *attackService) push(self gsr.ServiceContext, member attackMember, command runtime.CommandID, payload map[string]any) {
	if err := self.Cluster().Send(cluster.NodeID(member.AccessNode), cluster.ServiceName(member.AgentName),
		command, member.LoginID, payload); err != nil {
		logMoleError(self, "地鼠推送未接纳，客户端需取快照", err)
	}
}
