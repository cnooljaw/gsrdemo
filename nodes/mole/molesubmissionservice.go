package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
)

const (
	moleSubmissionName                           = "mole-submission"
	moleSubmissionCount                          = 4
	submissionBatchCommand     runtime.CommandID = 1
	submissionTickCommand      runtime.CommandID = 10
	submissionStateCommand     runtime.CommandID = 11
	settlementSubmitCommand    runtime.CommandID = 1
	settlementBatchCommand     runtime.CommandID = 2
	settlementOperationCommand runtime.CommandID = 3
	settlementBalanceCommand   runtime.CommandID = 4
)

// moleSubmissionService 独占 Redis 检查点和结算调用；只推进一个在途批次，不持有钱包。
type moleSubmissionService struct {
	store      moleCheckpoint
	lane       uint64
	batch      *moleBatch
	receipt    *batchReceipt
	ready      bool
	fault      error
	retryAt    time.Time
	retryDelay time.Duration
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*moleSubmissionService)(nil)

func newMoleSubmissionService(store moleCheckpoint, lane uint64) *moleSubmissionService {
	service := &moleSubmissionService{store: store, lane: lane}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		submissionBatchCommand: service.submit, submissionTickCommand: service.tick, submissionStateCommand: service.state,
	})
	return service
}

func (service *moleSubmissionService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	_, err := self.CreateService(ctx, &molePulse{target: self.Self(), command: submissionTickCommand, period: accountingPeriod, failedLane: int(service.lane)})
	return err
}

func (service *moleSubmissionService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleSubmissionService) OnStop(_ context.Context) error {
	return nil
}

func (service *moleSubmissionService) submit(self gsr.CommandContext, batch moleBatch) error {
	if service.batch != nil {
		if !reflect.DeepEqual(*service.batch, batch) {
			return errMoleBusy
		}
		return nil
	}
	if !service.ready {
		return errAccountingUnavailable
	}
	if !validMoleBatch(batch) || batch.Sequence == 0 {
		return errInvalidMoleInput
	}
	copied := batch.clone()
	service.batch = &copied
	// 只阻塞本提交 Service；Accounting 与各组继续处理内存请求。
	if err := service.advance(self); err != nil {
		service.failed(self, err)
		return err
	}
	return nil
}

func (service *moleSubmissionService) tick(self gsr.CommandContext) error {
	if service.fault != nil {
		// 通知满队列时下次继续，原始批次和 Redis 记录始终保留。
		return self.SendName(moleAccountingName, accountingStopLaneCommand, service.lane)
	}
	if time.Now().Before(service.retryAt) {
		return nil
	}
	err := service.advance(self)
	if err != nil {
		service.failed(self, err)
	} else {
		service.retryDelay, service.retryAt = 0, time.Time{}
	}
	return err
}

func (service *moleSubmissionService) failed(self gsr.ServiceContext, err error) {
	var status *runtime.StatusError
	if errors.As(err, &status) && (status.Class == runtime.ErrorClassDomain && status.Code != "settlement.store_unavailable" && status.Code != "mole.accounting_unavailable") {
		service.fault = err
		_ = self.SendName(moleAccountingName, accountingStopLaneCommand, service.lane)
		logMoleError(self, "当前结算通道暂停，保留原批次，修复后重启恢复", err)
		return
	}
	service.retryDelay = min(max(service.retryDelay*2, accountingPeriod), 2*time.Second)
	service.retryAt = time.Now().Add(service.retryDelay)
	logMoleError(self, "当前结算通道等待原批次重试", err)
}

func (service *moleSubmissionService) state(_ gsr.CommandContext) (map[string]any, error) {
	view := map[string]any{"lane": service.lane, "ready": service.ready && service.fault == nil, "blocked": service.fault != nil}
	if service.batch != nil {
		view["batch_id"], view["sequence"] = service.batch.ID, service.batch.Sequence
	}
	return view, nil
}

func (service *moleSubmissionService) advance(self gsr.ServiceContext) error {
	if !service.ready {
		ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
		state, err := service.store.load(ctx)
		cancel()
		if err != nil {
			return err
		}
		if state.Routing != moleSubmissionCount {
			return fmt.Errorf("%w: checkpoint routing requires explicit -init-accounting", errInvalidMoleInput)
		}
		// 恢复完 Redis 中的旧批次后才开放 Accounting，避免重用序号或读到未确认基线。
		service.batch, service.receipt = nil, nil
		if state.Pending != nil {
			copied := state.Pending.clone()
			service.batch = &copied
			state, err = service.settlePending(self)
			if err != nil {
				return err
			}
			service.batch, service.receipt = nil, nil
		}
		if err := self.SendName(moleAccountingName, accountingInitPoolCommand, state.Pool, state.Sequence, service.lane); err != nil {
			return err
		}
		service.ready = true
	}
	if service.batch == nil {
		return nil
	}
	if _, err := service.settlePending(self); err != nil {
		return err
	}
	if err := self.SendName(moleAccountingName, accountingSettledCommand, service.receipt.clone(), service.lane); err != nil {
		return err
	}
	service.batch, service.receipt = nil, nil
	return nil
}

func (service *moleSubmissionService) settlePending(self gsr.ServiceContext) (moleCheckpointState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	err := service.store.prepare(ctx, *service.batch)
	cancel()
	if err != nil {
		return moleCheckpointState{}, err
	}
	if service.receipt == nil {
		// 先保存原批次，再直接幂等提交；超时不换 ID，也不增加 GetBatch 前置查询。
		value, err := callSettlement(self, settlementSubmitCommand, service.batch.wire())
		if err != nil {
			return moleCheckpointState{}, err
		}
		receipt, err := decodeReceipt(value, *service.batch)
		if err != nil {
			return moleCheckpointState{}, fmt.Errorf("%w: %v", errInvalidMoleInput, err)
		}
		service.receipt = &receipt
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	return service.store.finish(ctx, *service.batch, *service.receipt)
}

func callSettlement(self gsr.ServiceContext, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	name := ""
	if command == settlementSubmitCommand {
		request := args[0].(map[string]any)
		name = routing.WriteService(request["batch_id"].(string))
	} else {
		key := fmt.Sprint(args[0])
		if command == settlementOperationCommand {
			key = fmt.Sprint(args[1])
		}
		name = routing.ReadService(key)
	}
	return self.Cluster().Call(ctx, "settlement", cluster.ServiceName(name), command, args...)
}

func submissionName(lane int) cluster.ServiceName {
	if lane == 0 {
		return moleSubmissionName
	}
	return cluster.ServiceName(fmt.Sprintf("mole-submission-%02d", lane))
}
