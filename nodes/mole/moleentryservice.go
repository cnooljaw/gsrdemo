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

// Entry 只协调身份核对、缺失余额查询和完整入场，不持有业务状态。
//
//rpc:service moleentry public name=moleEntryName
const (
	moleEntryName                      = "mole-entry"
	moleEnterCommand runtime.CommandID = 1
	// 原 3 号查询与 10 号补偿唤醒已停用，不复用。
)

type moleEntryService struct {
	settlementNode cluster.NodeID
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*moleEntryService)(nil)

func newMoleEntryService(settlementNode cluster.NodeID) *moleEntryService {
	service := &moleEntryService{settlementNode: settlementNode}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{moleEnterCommand: service.handleEnter})
	return service
}
func (*moleEntryService) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*moleEntryService) OnStop(context.Context) error                      { return nil }
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
	var seed map[string]any
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, callErr := self.CallName(ctx, moleServiceName, moleJoinCommand, userID, loginID, accessNode, agentName, seed)
		cancel()
		if callErr == nil {
			snapshot, ok := value.(map[string]any)
			if !ok {
				return nil, errAccountingUnavailable
			}
			return snapshot, nil
		}
		if !errors.Is(callErr, errMoleBalanceRequired) || attempt != 0 {
			if errors.Is(callErr, context.DeadlineExceeded) {
				return nil, runtime.ErrCallTimeout
			}
			return nil, callErr
		}
		// 首次 Enter 缺余额时没有修改任何成员、组或账户；查询失败无需回滚。
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, err = self.Cluster().Call(ctx, service.settlementNode, settlementread.ServiceName, settlementread.GetBalanceCommand, userID)
		cancel()
		if err != nil {
			return nil, errAccountingUnavailable
		}
		balance, err := decodeBalance(value)
		if err != nil {
			return nil, err
		}
		seed = balance.snapshot()
	}
	return nil, errAccountingUnavailable
}
