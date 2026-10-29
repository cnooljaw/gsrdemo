package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	moleServiceName                           = "mole"
	moleResolveEntryCommand runtime.CommandID = 1
	moleDepartedCommand     runtime.CommandID = 10
	moleBalanceCommand      runtime.CommandID = 11
	moleReserveCommand      runtime.CommandID = 12
	moleDiscardGroupCommand runtime.CommandID = 13
	moleHealthCommand       runtime.CommandID = 14
	maxMoleUsers                              = 100000
	moleCallTimeout                           = 500 * time.Millisecond
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
	errMoleOperationUnknown  = runtime.NewStatusError(runtime.ErrorClassDomain, "mole.operation_unknown", "query the original operation; do not resubmit as new")
)

type moleRoute struct {
	Attack     string
	LoginID    string
	Membership string
}

// moleService 只持有冷路径目录，地鼠和记账分别属于自己的 Service。
type moleService struct {
	routes     map[uint64]moleRoute
	groups     map[string]int
	dispatcher *gsr.Dispatcher
	onStopped  func(error)
}

var _ gsr.Service = (*moleService)(nil)

func newMoleService() *moleService {
	service := &moleService{routes: make(map[uint64]moleRoute), groups: make(map[string]int)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		moleHealthCommand: service.health, moleResolveEntryCommand: service.entryAddress, moleReserveCommand: service.reserve, moleDiscardGroupCommand: service.discardGroup, moleDepartedCommand: service.departed, moleBalanceCommand: service.balanceCorrection,
	})
	return service
}

func (service *moleService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return startMolePulse(ctx, self, accountingPeriod, moleHealthCommand, service.onStopped)
}

func (service *moleService) health(gsr.CommandContext) error { return nil }

func (service *moleService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleService) OnStop(_ context.Context) error {
	return nil
}

// entryAddress 仅返回稳定的进入通道，不等待 Online、余额或 Join。
func (service *moleService) entryAddress(_ gsr.CommandContext, userID uint64, loginID, accessNode, agentName string) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) {
		return nil, errInvalidMoleInput
	}
	return map[string]any{"entry_name": moleEntryName(userID)}, nil
}

// reserve 在 Join 前保留座位；未知结果的重试始终回到原组。
func (service *moleService) reserve(self gsr.CommandContext, userID uint64, loginID string) (moleRoute, error) {
	if self.Source() == 0 || userID == 0 || !validMoleIdentifier(loginID) {
		return moleRoute{}, errInvalidMoleInput
	}
	if _, exists := service.routes[userID]; !exists && len(service.routes) >= maxMoleUsers {
		return moleRoute{}, errMoleBusy
	}
	name := service.routes[userID].Attack
	membership, err := randomInstance("member-")
	if err != nil {
		return moleRoute{}, err
	}
	if name == "" {
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

func (service *moleService) discardGroup(self gsr.CommandContext, name string) error {
	if self.Source() == 0 || !validMoleIdentifier(name) {
		return errInvalidMoleInput
	}
	service.removeGroup(name)
	return nil
}

func (service *moleService) balanceCorrection(self gsr.CommandContext, userID uint64, view map[string]any) error {
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

func (service *moleService) departed(self gsr.CommandContext, userID uint64, loginID, attack, membership string) error {
	if !service.removeMember(userID, loginID, attack, membership) {
		return nil
	}
	if err := self.SendName(moleAccountingName, accountingForgetCommand, userID); err != nil {
		logMoleError(self, "余额缓存回收通知未接纳", err)
	}
	return nil
}

func (service *moleService) removeMember(userID uint64, loginID, attack, membership string) bool {
	route, exists := service.routes[userID]
	if !exists || route.LoginID != loginID || route.Attack != attack || route.Membership != membership {
		return false
	}
	delete(service.routes, userID)
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
