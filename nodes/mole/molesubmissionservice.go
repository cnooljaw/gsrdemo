package main

import (
	"context"
	"reflect"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	moleSubmissionName                           = "mole-submission"
	submissionBatchCommand     runtime.CommandID = 1
	submissionBalanceCommand   runtime.CommandID = 2
	submissionOperationCommand runtime.CommandID = 3
	submissionTickCommand      runtime.CommandID = 10
	settlementSubmitCommand    runtime.CommandID = 1
	settlementBatchCommand     runtime.CommandID = 2
	settlementOperationCommand runtime.CommandID = 3
	settlementBalanceCommand   runtime.CommandID = 4
)

// moleSubmissionService 独占 Redis 检查点和结算调用；只推进一个在途批次，不持有钱包。
type moleSubmissionService struct {
	store      moleCheckpoint
	batch      *moleBatch
	receipt    *batchReceipt
	ready      bool
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*moleSubmissionService)(nil)

func newMoleSubmissionService(store moleCheckpoint) *moleSubmissionService {
	service := &moleSubmissionService{store: store}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		submissionBatchCommand: service.submit, submissionBalanceCommand: service.balance,
		submissionOperationCommand: service.operation, submissionTickCommand: service.tick,
	})
	return service
}

func (service *moleSubmissionService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(time.Millisecond, submissionTickCommand)
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
		logMoleError(self, "结算提交未完成，保留同一内存批次重试", err)
		return err
	}
	return nil
}

func (service *moleSubmissionService) tick(self gsr.CommandContext) error {
	err := service.advance(self)
	if err != nil {
		logMoleError(self, "结算未完成，等待原批次结果", err)
	}
	if _, timerErr := self.After(accountingPeriod, submissionTickCommand); timerErr != nil {
		return timerErr
	}
	return err
}

func (service *moleSubmissionService) advance(self gsr.ServiceContext) error {
	if !service.ready {
		ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
		state, err := service.store.load(ctx)
		cancel()
		if err != nil {
			return err
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
		if err := self.SendName(moleAccountingName, accountingInitPoolCommand, state.Pool, state.Sequence); err != nil {
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
	if err := self.SendName(moleAccountingName, accountingSettledCommand, service.receipt.clone()); err != nil {
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
			return moleCheckpointState{}, err
		}
		service.receipt = &receipt
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	return service.store.finish(ctx, *service.batch, *service.receipt)
}

func (service *moleSubmissionService) balance(self gsr.CommandContext, userID uint64) (map[string]any, error) {
	if !service.ready {
		return nil, errAccountingUnavailable
	}
	if userID == 0 {
		return nil, errInvalidMoleInput
	}
	value, err := callSettlement(self, settlementBalanceCommand, userID)
	if err != nil {
		return nil, errAccountingUnavailable
	}
	balance, err := decodeBalance(value)
	if err != nil {
		return nil, err
	}
	return balance.snapshot(), nil
}

func (service *moleSubmissionService) operation(self gsr.CommandContext, userID uint64, hitID string) (any, error) {
	if userID == 0 || !validMoleIdentifier(hitID) {
		return nil, errInvalidMoleInput
	}
	return callSettlement(self, settlementOperationCommand, userID, hitID)
}

func callSettlement(self gsr.ServiceContext, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	defer cancel()
	return self.Cluster().Call(ctx, "settlement", "settlement", command, args...)
}
