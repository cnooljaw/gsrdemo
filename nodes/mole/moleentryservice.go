package main

import (
	"context"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
)

const (
	moleEntryName                      = "mole-entry"
	moleEnterCommand runtime.CommandID = 1
)

// moleEntryService 只协调低频进入，不拥有目录、余额或房间状态。
type moleEntryService struct {
	settlementNode cluster.NodeID
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*moleEntryService)(nil)

func newMoleEntryService(settlementNode cluster.NodeID) *moleEntryService {
	service := &moleEntryService{settlementNode: settlementNode}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		moleEnterCommand: service.handleEnter,
	})
	return service
}

func (service *moleEntryService) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (service *moleEntryService) OnStop(context.Context) error                      { return nil }
func (service *moleEntryService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleEntryService) handleEnter(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName string) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) {
		return nil, errInvalidMoleInput
	}
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	location, err := self.Cluster().Call(ctx, "online", onlinerpc.ServiceName, onlinerpc.LookupCommand, userID)
	cancel()
	if err != nil {
		return nil, errMoleOnlineUnavailable
	}
	current, ok := location.(map[string]any)
	if !ok || current["login_id"] != loginID || current["access_node"] != accessNode || current["agent_name"] != agentName {
		return nil, errMoleStaleLogin
	}
	// 同一成员身份贯穿余额缓存与组目录，旧连接的迟到回收不能作用于本次进入。
	membership, err := randomInstance("member-")
	if err != nil {
		return nil, err
	}
	balance, err := service.entryBalance(self, userID, membership)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, err := self.CallName(ctx, moleServiceName, moleReserveCommand, userID, loginID, membership)
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
func (service *moleEntryService) entryBalance(self gsr.ServiceContext, userID uint64, membership string) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	cached, err := self.CallName(ctx, moleAccountingName, accountingResumeCommand, userID, membership)
	cancel()
	if err != nil || cached != nil {
		return cached, err
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	value, err := self.Cluster().Call(ctx, service.settlementNode, settlementread.ServiceName, settlementread.GetBalanceCommand, userID)
	cancel()
	if err != nil {
		return nil, errAccountingUnavailable
	}
	confirmed, err := decodeBalance(value)
	if err != nil {
		return nil, err
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	return self.CallName(ctx, moleAccountingName, accountingSetBalanceCommand, userID, membership, confirmed.snapshot())
}
