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
	settlementSummaryCommand   runtime.CommandID = 5
)

// moleSubmissionService 是唯一远端结算调用者；只保留一个易失批次，不持有钱包或数据库。
type moleSubmissionService struct {
	batch      *moleBatch
	receipt    *batchReceipt
	ready      bool
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*moleSubmissionService)(nil)

func newMoleSubmissionService() *moleSubmissionService {
	service := &moleSubmissionService{}
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
	if !validMoleIdentifier(batch.ID) || len(batch.Events) == 0 || len(batch.Events) > maxBatchOperations || batchSize(batch) > maxBatchBytes {
		return errInvalidMoleInput
	}
	for _, event := range batch.Events {
		if !validHitEvent(event) {
			return errInvalidMoleInput
		}
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
		value, err := callSettlement(self, settlementSummaryCommand, "mole")
		if err != nil {
			return err
		}
		pool, err := decodePool(value)
		if err != nil {
			return err
		}
		if err := self.SendName(moleAccountingName, accountingInitPoolCommand, pool); err != nil {
			return err
		}
		service.ready = true
	}
	if service.batch == nil {
		return nil
	}
	if service.receipt == nil {
		// SubmitBatch 按原 ID 和内容幂等；超时后直接重提，不增加查询前置。
		value, err := callSettlement(self, settlementSubmitCommand, service.batch.wire())
		if err != nil {
			return err
		}
		receipt, err := decodeReceipt(value, *service.batch)
		if err != nil {
			return err
		}
		service.receipt = &receipt
	}
	if err := self.SendName(moleAccountingName, accountingSettledCommand, service.receipt.clone()); err != nil {
		return err
	}
	service.batch, service.receipt = nil, nil
	return nil
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
