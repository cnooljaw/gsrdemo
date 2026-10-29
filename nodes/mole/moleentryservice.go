package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	moleEntryCount                         = 8
	moleEnterCommand     runtime.CommandID = 1
	moleOperationCommand runtime.CommandID = 3
)

// moleEntryService 仅协调进入，不拥有目录、余额或房间状态。慢请求限制在一个进入通道。
type moleEntryService struct {
	lane       uint64
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*moleEntryService)(nil)

func moleEntryName(userID uint64) string {
	return fmt.Sprintf("mole-entry-%02d", userID%moleEntryCount)
}

func newMoleEntryService(lane uint64) *moleEntryService {
	service := &moleEntryService{lane: lane}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{moleEnterCommand: service.enter, moleOperationCommand: service.operation})
	return service
}

func (service *moleEntryService) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (service *moleEntryService) OnStop(context.Context) error                      { return nil }
func (service *moleEntryService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleEntryService) enter(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName string) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) || userID%moleEntryCount != service.lane {
		return nil, errInvalidMoleInput
	}
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
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, err := self.CallName(ctx, moleServiceName, moleReserveCommand, userID, loginID)
		cancel()
		if err != nil {
			return nil, err
		}
		route, ok := value.(moleRoute)
		if !ok {
			return nil, errAccountingUnavailable
		}
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, err = self.CallName(ctx, cluster.ServiceName(route.Attack), attackJoinCommand, userID, loginID, accessNode, agentName, route.Membership)
		cancel()
		if err == nil {
			snapshot, ok := value.(map[string]any)
			if !ok {
				return nil, errAccountingUnavailable
			}
			snapshot["silver"] = balance
			return snapshot, nil
		}
		// 超时可能已经加入，只能保留原路由，不能改分组或撤销座位。
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, runtime.ErrCallTimeout
		}
		if !errors.Is(err, runtime.ErrServiceNotFound) && !errors.Is(err, runtime.ErrServiceClosed) && !errors.Is(err, runtime.ErrServiceStopping) && !errors.Is(err, runtime.ErrServicePanicked) {
			return nil, err
		}
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		_, err = self.CallName(ctx, moleServiceName, moleDiscardGroupCommand, route.Attack)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	return nil, errMoleBusy
}

// entryBalance 只在缓存缺失时直接查询 settlement，不与批次提交共享等待队列。
func (service *moleEntryService) entryBalance(self gsr.ServiceContext, userID uint64) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	cached, err := self.CallName(ctx, moleAccountingName, accountingResumeCommand, userID)
	cancel()
	if err != nil || cached != nil {
		return cached, err
	}
	value, err := callSettlement(self, settlementBalanceCommand, userID)
	if err != nil {
		return nil, errAccountingUnavailable
	}
	confirmed, err := decodeBalance(value)
	if err != nil {
		return nil, err
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	return self.CallName(ctx, moleAccountingName, accountingSetBalanceCommand, userID, confirmed.snapshot())
}

// operation 的只读远端等待只占用进入通道，不与归批提交共享 Mailbox。
func (service *moleEntryService) operation(self gsr.CommandContext, userID uint64, hitID string) (any, error) {
	if userID == 0 || userID%moleEntryCount != service.lane || !validMoleIdentifier(hitID) {
		return nil, errInvalidMoleInput
	}
	return callSettlement(self, settlementOperationCommand, userID, hitID)
}
