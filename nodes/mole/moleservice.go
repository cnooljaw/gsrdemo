package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	moleServiceName                       = "mole"
	moleEnterCommand    runtime.CommandID = 1
	moleDepartedCommand runtime.CommandID = 10
	moleBalanceCommand  runtime.CommandID = 11
	maxMoleUsers                          = 100000
	moleCallTimeout                       = 500 * time.Millisecond
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
}

var _ gsr.Service = (*moleService)(nil)

func newMoleService() *moleService {
	service := &moleService{routes: make(map[uint64]moleRoute), groups: make(map[string]int)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		moleEnterCommand: service.enter, moleDepartedCommand: service.departed, moleBalanceCommand: service.balanceCorrection,
	})
	return service
}

func (service *moleService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

func (service *moleService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleService) OnStop(_ context.Context) error {
	return nil
}

func (service *moleService) enter(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName string) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) {
		return nil, errInvalidMoleInput
	}
	// Enter 也可能迟到，不能只依赖客户端曾经完成过绑定。
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	location, err := self.Cluster().Call(ctx, "online", "online", 2, userID)
	cancel()
	if err != nil {
		return nil, errMoleOnlineUnavailable
	}
	current, ok := location.(map[string]any)
	if !ok || current["login_id"] != loginID || current["access_node"] != accessNode || current["agent_name"] != agentName {
		return nil, errMoleStaleLogin
	}
	balance, err := service.entryBalance(self, userID)
	if err != nil {
		return nil, err
	}
	if _, exists := service.routes[userID]; !exists && len(service.routes) >= maxMoleUsers {
		return nil, errMoleBusy
	}
	name := service.routes[userID].Attack
	membership, err := randomInstance("member-")
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if name == "" {
			name, err = service.chooseGroup(self)
			if err != nil {
				return nil, err
			}
		}
		// Join 超时可能已经执行。先保留该用户的组路由，重试只能回到同一组。
		if _, exists := service.routes[userID]; !exists {
			service.groups[name]++
		}
		service.routes[userID] = moleRoute{Attack: name, LoginID: loginID, Membership: membership}
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, joinErr := self.CallName(ctx, cluster.ServiceName(name), attackJoinCommand, userID, loginID, accessNode, agentName, membership)
		cancel()
		if joinErr == nil {
			snapshot, ok := value.(map[string]any)
			if !ok {
				return nil, errAccountingUnavailable
			}
			snapshot["silver"] = balance
			return snapshot, nil
		}
		if errors.Is(joinErr, context.DeadlineExceeded) {
			// Handler 返回裸 context 错误会被归为 handler_failed，保留可辨识的超时语义。
			return nil, runtime.ErrCallTimeout
		}
		if !errors.Is(joinErr, runtime.ErrServiceNotFound) && !errors.Is(joinErr, runtime.ErrServiceClosed) &&
			!errors.Is(joinErr, runtime.ErrServiceStopping) && !errors.Is(joinErr, runtime.ErrServicePanicked) {
			return nil, joinErr
		}
		service.removeGroup(name)
		name = ""
	}
	return nil, errMoleBusy
}

// entryBalance 只在缓存缺失时查询远端；已有本地账由回执推进，不用裸余额覆盖。
func (service *moleService) entryBalance(self gsr.ServiceContext, userID uint64) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	cached, err := self.CallName(ctx, moleAccountingName, accountingResumeCommand, userID)
	cancel()
	if err != nil || cached != nil {
		return cached, err
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	value, err := self.CallName(ctx, moleSubmissionName, submissionBalanceCommand, userID)
	cancel()
	if err != nil {
		return nil, errAccountingUnavailable
	}
	confirmed, ok := value.(map[string]any)
	if !ok {
		return nil, errAccountingUnavailable
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	return self.CallName(ctx, moleAccountingName, accountingSetBalanceCommand, userID, confirmed)
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
	if err := self.Logger().Error(message, "error", cause.Error()); err != nil {
		// 日志设施不可用时保留两种错误，不递归写同一个日志器。
		_, _ = fmt.Fprintln(os.Stderr, message, cause, "log:", err)
	}
}
