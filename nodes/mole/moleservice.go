package main

import (
	"container/list"
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

// MoleService 是本节点组、成员和内存账务的唯一 owner。
//
//rpc:service mole public name=moleServiceName
const (
	moleServiceName                                = "mole"
	moleJoinCommand              runtime.CommandID = 1
	moleHitCommand               runtime.CommandID = 2
	moleSnapshotCommand          runtime.CommandID = 3
	moleLeaveCommand             runtime.CommandID = 4
	moleTickCommand              runtime.CommandID = 5
	moleInitAccountingCommand    runtime.CommandID = 6
	moleSettlementReceiptCommand runtime.CommandID = 7
	molePauseCommand             runtime.CommandID = 8
	moleBalanceCommand           runtime.CommandID = 9
	molePoolCommand              runtime.CommandID = 10
	moleOperationStatusCommand   runtime.CommandID = 11
	moleRefreshCommand           runtime.CommandID = 12
)
const (
	maxMoleUsers    = 100000
	moleCallTimeout = 500 * time.Millisecond
)

var (
	errInvalidMoleInput      = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.invalid_request", "invalid mole request")
	errMoleStaleLogin        = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.stale_login", "login is not current")
	errMoleOnlineUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.online_unavailable", "online directory is unavailable")
	errMoleConflict          = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.conflict", "request identity conflict")
	errMoleBusy              = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.busy", "mole is busy")
	errTargetUnavailable     = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.target_unavailable", "target is unavailable")
	errAccountingUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.accounting_unavailable", "accounting is unavailable")
	errInsufficientSilver    = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.insufficient_silver", "insufficient silver")
	errMoleOperationUnknown  = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.operation_unknown", "operation outcome unknown; do not resubmit as new")
	errMoleBalanceRequired   = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.balance_required", "balance is required")
)

type moleService struct {
	groups          map[string]*moleGroup
	availableGroups [attackSeats]map[string]*moleGroup // 按实际人数索引可补位组，0 人和满组不入索引。
	users           map[uint64]string                  // 用户到实际组的索引；与组内成员在同一回调更新。
	accounting      moleAccounting
	spawnOrder      list.List // 固定周期下按下次刷新时间排列，入组和刷新均追加到尾部。
	refreshQueued   bool      // 同一时刻最多一条自身续批消息。
	dispatcher      *gsr.Dispatcher
	onStopped       func(error)
}

var _ gsr.Service = (*moleService)(nil)

func newMoleService() *moleService {
	service := &moleService{groups: make(map[string]*moleGroup), users: make(map[uint64]string), accounting: newMoleAccounting()}
	for size := 1; size < attackSeats; size++ {
		service.availableGroups[size] = make(map[string]*moleGroup)
	}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		moleJoinCommand:              service.handleEnter,
		moleHitCommand:               service.handleHit,
		moleSnapshotCommand:          service.handleSnapshot,
		moleLeaveCommand:             service.handleLeave,
		moleTickCommand:              service.handleTick,
		moleInitAccountingCommand:    service.handleInitAccounting,
		moleSettlementReceiptCommand: service.handleSettlementReceipt,
		molePauseCommand:             service.handlePause,
		moleBalanceCommand:           service.handleBalance,
		molePoolCommand:              service.handlePool,
		moleOperationStatusCommand:   service.handleOperationStatus,
		moleRefreshCommand:           service.handleRefresh,
	})
	return service
}
func (service *moleService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return startMolePulse(ctx, self, accountingPeriod, moleTickCommand, service.onStopped)
}
func (*moleService) OnStop(context.Context) error { return nil }
func (service *moleService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleService) handleEnter(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName string, seed map[string]any) (map[string]any, error) {
	if self.Source() == 0 {
		return nil, errInvalidMoleInput
	}
	view, err := service.enter(userID, loginID, accessNode, agentName, seed)
	if err == nil {
		service.broadcast(self, service.groups[service.users[userID]])
	}
	return view, err
}

// enter 先检查所有可拒绝条件，再一次更新组、成员和账户；整个过程没有消息等待或外部 I/O。
func (service *moleService) enter(userID uint64, loginID, accessNode, agentName string, seed map[string]any) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) {
		return nil, errInvalidMoleInput
	}
	if !service.accounting.ready {
		return nil, errAccountingUnavailable
	}
	group := service.groups[service.users[userID]]
	if group == nil && len(service.users) >= maxMoleUsers {
		return nil, errMoleBusy
	}
	account := service.accounting.accounts[userID]
	var confirmed silverBalance
	var balance map[string]any
	if account == nil {
		if len(service.accounting.accounts) >= maxMoleUsers {
			return nil, errMoleBusy
		}
		if seed == nil {
			return nil, errMoleBalanceRequired
		}
		var err error
		confirmed, err = decodeBalance(seed)
		if err != nil {
			return nil, err
		}
		balance = map[string]any{"balance": confirmed.Amount, "confirmed_balance": confirmed.Amount, "available": confirmed.Amount, "version": confirmed.Version, "revision": uint64(1)}
	} else {
		var err error
		balance, err = service.accounting.balanceSnapshot(userID)
		if err != nil {
			return nil, err
		}
	}
	member := moleMember{UserID: userID, LoginID: loginID, AccessNode: accessNode, AgentName: agentName}
	if group != nil {
		old := group.members[userID]
		if old.LoginID == loginID && (old.AccessNode != accessNode || old.AgentName != agentName) {
			return nil, errMoleConflict
		}
		member.Seat = old.Seat
	} else {
		group = service.availableGroup()
		if group == nil {
			name, err := randomInstance("attack-")
			if err != nil {
				return nil, err
			}
			group = newMoleGroup(name, time.Now())
		}
		used := [attackSeats]bool{}
		for _, current := range group.members {
			used[current.Seat] = true
		}
		for seat, occupied := range used {
			if !occupied {
				member.Seat = int64(seat)
				break
			}
		}
	}
	// 此后只提交已验证的内存状态；快照和余额来自同一次回调。
	previousSize := len(group.members)
	if account == nil {
		service.accounting.updateBalance(userID, confirmed)
	}
	group.members[userID] = member
	service.updateAvailableGroup(group, previousSize)
	if group.spawnElement == nil {
		service.addGroup(group)
	}
	service.users[userID] = group.game.name
	view := group.view(userID)
	view["silver"] = balance
	return view, nil
}

// availableGroup 优先补人数较多的组；同人数任选，不遍历满组或按名称排序。
func (service *moleService) availableGroup() *moleGroup {
	for size := attackSeats - 1; size > 0; size-- {
		for _, group := range service.availableGroups[size] {
			return group
		}
	}
	return nil
}

// updateAvailableGroup 与成员修改在同一 owner 回调执行；重进不改变人数或索引。
func (service *moleService) updateAvailableGroup(group *moleGroup, previousSize int) {
	size := len(group.members)
	if size == previousSize {
		return
	}
	if previousSize > 0 && previousSize < attackSeats {
		delete(service.availableGroups[previousSize], group.game.name)
	}
	if size > 0 && size < attackSeats {
		service.availableGroups[size][group.game.name] = group
	}
}

func (service *moleService) memberGroup(userID uint64, loginID, groupName string) (*moleGroup, error) {
	group := service.groups[service.users[userID]]
	if group == nil || group.game.name != groupName || group.members[userID].LoginID != loginID {
		return nil, errMoleStaleLogin
	}
	return group, nil
}
func (service *moleService) handleHit(self gsr.CommandContext, userID uint64, loginID, groupName, actionID string, sequence uint64, hole int64) (map[string]any, error) {
	group, err := service.memberGroup(userID, loginID, groupName)
	if err != nil {
		return nil, err
	}
	result, err := group.hit(&service.accounting, userID, loginID, actionID, sequence, hole)
	if err != nil {
		return nil, err
	}
	if len(service.accounting.buffer) >= maxBatchOperations || service.accounting.bufferBytes >= maxBatchBytes-128 {
		if err := service.accounting.flush(self); err != nil {
			logMoleError(self, "提交队列未接纳，保留内存操作", err)
		}
	}
	service.broadcast(self, group)
	return result, nil
}
func (service *moleService) handleSnapshot(_ gsr.CommandContext, userID uint64, loginID, groupName string) (map[string]any, error) {
	group, err := service.memberGroup(userID, loginID, groupName)
	if err != nil {
		return nil, err
	}
	return group.view(userID), nil
}
func (service *moleService) handleLeave(self gsr.CommandContext, userID uint64, loginID string) (bool, error) {
	group, err := service.leave(userID, loginID)
	if err != nil {
		return false, err
	}
	if group != nil && len(group.members) > 0 {
		service.broadcast(self, group)
	}
	return true, nil
}
func (service *moleService) leave(userID uint64, loginID string) (*moleGroup, error) {
	if userID == 0 || !validMoleIdentifier(loginID) {
		return nil, errInvalidMoleInput
	}
	group := service.groups[service.users[userID]]
	if group == nil {
		return nil, nil
	}
	if group.members[userID].LoginID != loginID {
		return nil, errMoleStaleLogin
	}
	previousSize := len(group.members)
	delete(group.members, userID)
	service.updateAvailableGroup(group, previousSize)
	delete(service.users, userID)
	if len(group.members) == 0 {
		service.removeGroup(group)
	}
	service.releaseAccount(userID)
	return group, nil
}
func (service *moleService) releaseAccount(userID uint64) {
	if service.users[userID] == "" && !service.accounting.hasPending(userID) {
		delete(service.accounting.accounts, userID)
	}
}
func (service *moleService) handleTick(self gsr.CommandContext) error {
	if self.Source() == 0 {
		return errInvalidMoleInput
	}
	err := service.accounting.flush(self)
	if err != nil {
		logMoleError(self, "地鼠归批未完成，保留内存操作", err)
	}
	if !service.refreshQueued {
		service.refreshGroups(self)
	}
	return err
}
func (service *moleService) handleInitAccounting(self gsr.CommandContext, pool molePool, sequence uint64) error {
	if self.Source() == 0 {
		return errInvalidMoleInput
	}
	service.accounting.initPool(pool, sequence)
	return nil
}
func (service *moleService) handlePause(self gsr.CommandContext) error {
	if self.Source() == 0 {
		return errInvalidMoleInput
	}
	service.accounting.ready = false
	return nil
}
func (service *moleService) handleSettlementReceipt(self gsr.CommandContext, receipt batchReceipt) error {
	if self.Source() == 0 {
		return errInvalidMoleInput
	}
	accounting := &service.accounting
	if accounting.inFlightID == "" || accounting.inFlightID != receipt.BatchID {
		return nil
	}
	before := make(map[uint64]int64, len(receipt.Balances))
	for userID := range receipt.Balances {
		before[userID], _, _ = accounting.balanceAmounts(userID)
	}
	accounting.applyReceipt(receipt)
	accounting.inFlightID = ""
	for userID := range receipt.Balances {
		group := service.groups[service.users[userID]]
		view, err := accounting.balanceSnapshot(userID)
		if group != nil && err == nil && view["balance"] != before[userID] {
			service.push(self, group.members[userID], accessagent.MoleBalanceCorrectionPushCommand, view)
		}
		service.releaseAccount(userID)
	}
	return accounting.flush(self)
}
func (service *moleService) handleBalance(_ gsr.CommandContext, userID uint64) (map[string]any, error) {
	return service.accounting.balanceSnapshot(userID)
}
func (service *moleService) handlePool(gsr.CommandContext) (map[string]any, error) {
	if !service.accounting.ready {
		return nil, errAccountingUnavailable
	}
	return service.accounting.pool.snapshot(), nil
}
func (service *moleService) handleOperationStatus(_ gsr.CommandContext, userID uint64, hitID string) (any, error) {
	return service.accounting.operationStatus(userID, hitID)
}
func (service *moleService) broadcast(self gsr.ServiceContext, group *moleGroup) {
	for userID, member := range group.members {
		service.push(self, member, accessagent.MoleSnapshotPushCommand, group.view(userID))
	}
}
func (*moleService) push(self gsr.ServiceContext, member moleMember, command runtime.CommandID, payload map[string]any) {
	if err := self.Cluster().Send(cluster.NodeID(member.AccessNode), cluster.ServiceName(member.AgentName), command, member.LoginID, payload); err != nil {
		logMoleError(self, "地鼠推送未接纳，客户端需取快照", err)
	}
}
func validMoleIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := range value {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}
func validMember(userID uint64, loginID, accessNode, agentName string) bool {
	return userID != 0 && validMoleIdentifier(loginID) && validMoleIdentifier(accessNode) && validMoleIdentifier(agentName)
}
func logMoleError(self gsr.ServiceContext, message string, cause error) {
	_ = self.Logger().Error(message, "error", cause.Error())
}
