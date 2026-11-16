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

// 进入协调提供固定逻辑入口，客户端经 Agent 调用。
//
//rpc:service moleentry public name=moleEntryName
const (
	moleEntryName                             = "mole-entry"
	moleEnterCommand        runtime.CommandID = 1
	entryCleanupTickCommand runtime.CommandID = 10
)

// moleEntryService 协调低频进入及失败准备的补偿，不拥有目录、余额或房间状态。
type moleEntryService struct {
	settlementNode cluster.NodeID
	pendingCleanup map[uint64]string // 每个用户至多一条，先交付旧补偿再开启新准备。
	onStopped      func(error)
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*moleEntryService)(nil)

func newMoleEntryService(settlementNode cluster.NodeID) *moleEntryService {
	service := &moleEntryService{settlementNode: settlementNode, pendingCleanup: make(map[uint64]string)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		moleEnterCommand:        service.handleEnter,
		entryCleanupTickCommand: service.handleCleanupTick,
	})
	return service
}

func (service *moleEntryService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return startMolePulse(ctx, self, accountingPeriod, entryCleanupTickCommand, service.onStopped)
}

func (service *moleEntryService) OnStop(context.Context) error {
	return nil
}

func (service *moleEntryService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleEntryService) handleEnter(self gsr.CommandContext, userID uint64, loginID, accessNode, agentName string) (map[string]any, error) {
	if !validMember(userID, loginID, accessNode, agentName) {
		return nil, errInvalidMoleInput
	}
	if _, exists := service.pendingCleanup[userID]; !exists && len(service.pendingCleanup) >= maxMoleUsers {
		return nil, errMoleBusy
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
	if _, pending := service.pendingCleanup[userID]; pending {
		// 新准备的结果也可能未知，不能先覆盖仍需清理的旧 Membership。
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		err = service.deliverPreparedCleanup(ctx, self, userID)
		cancel()
		if err != nil {
			return nil, errMoleBusy
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		// 每次换组重新建立成员身份和余额视图，原组的迟到回收不能作用于新组。
		membership, err := randomInstance("member-")
		if err != nil {
			return nil, err
		}
		balance, err := service.entryBalance(self, userID, membership)
		if err != nil {
			// 本地准备可能已执行；此时尚未发起 Reserve，按当前目录修正缓存归属。
			service.pendingCleanup[userID] = membership
			service.retryPreparedCleanup(self)
			return nil, err
		}
		ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
		value, err := self.CallName(ctx, moleServiceName, moleReserveCommand, userID, loginID, membership)
		cancel()
		if err != nil {
			if reserveNotAccepted(err) {
				service.pendingCleanup[userID] = membership
				service.retryPreparedCleanup(self)
			}
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

// reserveNotAccepted 只识别明确未接纳的失败；超时、panic 或未知 Handler 错误不撤销预留。
func reserveNotAccepted(err error) bool {
	return errors.Is(err, runtime.ErrMailboxFull) || errors.Is(err, runtime.ErrServiceNotFound) ||
		errors.Is(err, runtime.ErrServiceClosed) || errors.Is(err, runtime.ErrServiceStarting) || errors.Is(err, runtime.ErrServiceStopping)
}

func (service *moleEntryService) handleCleanupTick(self gsr.CommandContext) error {
	if self.Source() == 0 {
		return errInvalidMoleInput
	}
	service.retryPreparedCleanup(self)
	return nil
}

// retryPreparedCleanup 的数量及整轮等待都有上限；目录只需确认 Accounting 已接纳修正。
func (service *moleEntryService) retryPreparedCleanup(self gsr.ServiceContext) {
	if len(service.pendingCleanup) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	attempted := 0
	for userID := range service.pendingCleanup {
		if attempted >= maxForgetPerFlush {
			break
		}
		attempted++
		if err := service.deliverPreparedCleanup(ctx, self, userID); err != nil {
			logMoleError(self, "进入准备补偿未接纳，保留重试", err)
			return
		}
	}
}

func (service *moleEntryService) deliverPreparedCleanup(ctx context.Context, self gsr.ServiceContext, userID uint64) error {
	membership := service.pendingCleanup[userID]
	if _, err := self.CallName(ctx, moleServiceName, moleReconcilePreparationCommand, userID, membership); err != nil {
		return err
	}
	delete(service.pendingCleanup, userID)
	return nil
}
