package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// 地鼠目录以固定名称对外发布，命令编号均从属于此 Service。
//
//rpc:service mole public name=moleServiceName
const (
	moleServiceName                           = "mole"
	moleDepartedCommand     runtime.CommandID = 10
	moleBalanceCommand      runtime.CommandID = 11
	moleReserveCommand      runtime.CommandID = 12
	moleDiscardGroupCommand runtime.CommandID = 13
	moleHealthCommand       runtime.CommandID = 14
)

const (
	maxMoleUsers      = 100000
	moleCallTimeout   = 500 * time.Millisecond
	maxForgetPerFlush = 64
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
)

type moleRoute struct {
	Attack     string
	LoginID    string
	Membership string
}

// moleService 持有分组目录与尚未接纳的缓存回收通知，地鼠和记账分别属于自己的 Service。
type moleService struct {
	routes        map[uint64]moleRoute
	groups        map[string]int
	pendingForget map[uint64]string // UserID → Membership；同用户只保留最新一条回收通知。
	dispatcher    *gsr.Dispatcher
	onStopped     func(error)
}

var _ gsr.Service = (*moleService)(nil)

func newMoleService() *moleService {
	service := &moleService{
		routes: make(map[uint64]moleRoute), groups: make(map[string]int), pendingForget: make(map[uint64]string),
	}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		moleHealthCommand:       service.handleHealth,
		moleReserveCommand:      service.handleReserve,
		moleDiscardGroupCommand: service.handleDiscardGroup,
		moleDepartedCommand:     service.handleDeparted,
		moleBalanceCommand:      service.handleBalanceCorrection,
	})
	return service
}

func (service *moleService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return startMolePulse(ctx, self, accountingPeriod, moleHealthCommand, service.onStopped)
}

// handleHealth 复用目录的周期存活探测，补投缓存回收，不等待 Accounting 执行。
func (service *moleService) handleHealth(self gsr.CommandContext) error {
	service.flushForgets(self)
	return nil
}

func (service *moleService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleService) OnStop(_ context.Context) error {
	return nil
}

// reserve 在 Join 前保留座位；未知结果的重试始终回到原组。
func (service *moleService) handleReserve(self gsr.CommandContext, userID uint64, loginID, membership string) (moleRoute, error) {
	if self.Source() == 0 || userID == 0 || !validMoleIdentifier(loginID) || !validMoleIdentifier(membership) {
		return moleRoute{}, errInvalidMoleInput
	}
	if _, exists := service.routes[userID]; !exists && len(service.routes) >= maxMoleUsers {
		return moleRoute{}, errMoleBusy
	}
	name := service.routes[userID].Attack
	if name == "" {
		var err error
		name, err = service.chooseGroup(self)
		if err != nil {
			return moleRoute{}, err
		}
	}
	if _, exists := service.routes[userID]; !exists {
		service.groups[name]++
	}
	route := moleRoute{Attack: name, LoginID: loginID, Membership: membership}
	service.routes[userID] = route
	return route, nil
}

func (service *moleService) handleDiscardGroup(self gsr.CommandContext, name string) error {
	if self.Source() == 0 || !validMoleIdentifier(name) {
		return errInvalidMoleInput
	}
	service.removeGroup(name)
	service.flushForgets(self)
	return nil
}

func (service *moleService) handleBalanceCorrection(self gsr.CommandContext, userID uint64, view map[string]any) error {
	route, exists := service.routes[userID]
	if !exists {
		return nil
	}
	return self.SendName(cluster.ServiceName(route.Attack), attackBalanceCommand, userID, route.LoginID, view)
}

func (service *moleService) chooseGroup(self gsr.ServiceContext) (string, error) {
	available, largest := "", -1
	for name, count := range service.groups {
		if count < attackSeats && (count > largest || count == largest && name < available) {
			available, largest = name, count
		}
	}
	if available != "" {
		return available, nil
	}
	name, err := randomInstance("attack-")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	_, err = self.CreateService(ctx, newAttackService(name))
	if err != nil {
		return "", err
	}
	service.groups[name] = 0
	return name, nil
}

func (service *moleService) handleDeparted(self gsr.CommandContext, userID uint64, loginID, attack, membership string) error {
	if !service.removeMember(userID, loginID, attack, membership) {
		return nil
	}
	service.flushForgets(self)
	return nil
}

// flushForgets 只移除已被接纳的通知；背压时停下，交给下一次目录 Pulse 补投。
func (service *moleService) flushForgets(self gsr.ServiceContext) {
	attempts := 0
	for userID, membership := range service.pendingForget {
		if attempts >= maxForgetPerFlush {
			return
		}
		attempts++
		if err := self.SendName(moleAccountingName, accountingForgetCommand, userID, membership); err != nil {
			logMoleError(self, "余额缓存回收通知未接纳，保留待补投", err)
			return
		}
		delete(service.pendingForget, userID)
	}
}

func (service *moleService) removeMember(userID uint64, loginID, attack, membership string) bool {
	route, exists := service.routes[userID]
	if !exists || route.LoginID != loginID || route.Attack != attack || route.Membership != membership {
		return false
	}
	delete(service.routes, userID)
	service.pendingForget[userID] = membership
	service.groups[attack]--
	if service.groups[attack] == 0 {
		delete(service.groups, attack)
	}
	return true
}

func (service *moleService) removeGroup(name string) {
	delete(service.groups, name)
	for userID, route := range service.routes {
		if route.Attack == name {
			delete(service.routes, userID)
			service.pendingForget[userID] = route.Membership
		}
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
	// 日志队列满时保持业务有界，不在 Service 回调中退回同步 stderr I/O。
	_ = self.Logger().Error(message, "error", cause.Error())
}
