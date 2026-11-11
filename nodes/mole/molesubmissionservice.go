package main

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
)

const (
	moleSubmissionName                       = "mole-submission"
	submissionBatchCommand runtime.CommandID = 1
	submissionTickCommand  runtime.CommandID = 10
	submissionStateCommand runtime.CommandID = 11
	moleSettlementTimeout                    = 3 * time.Second
)

// moleSubmissionService 独占 Redis 检查点和结算调用；只推进一个在途批次，不持有钱包。
type moleSubmissionService struct {
	store          moleCheckpoint
	settlementNode cluster.NodeID
	batch          *moleBatch
	receipt        *batchReceipt
	ready          bool
	fault          error
	retryAt        time.Time
	retryDelay     time.Duration
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*moleSubmissionService)(nil)

func newMoleSubmissionService(store moleCheckpoint, settlementNode cluster.NodeID) *moleSubmissionService {
	service := &moleSubmissionService{store: store, settlementNode: settlementNode}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		submissionBatchCommand: service.handleSubmit,
		submissionTickCommand:  service.handleTick,
		submissionStateCommand: service.handleState,
	})
	return service
}

func (service *moleSubmissionService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	_, err := self.CreateService(ctx, &molePulse{target: self.Self(), command: submissionTickCommand, period: accountingPeriod, pauseAccounting: true})
	return err
}

func (service *moleSubmissionService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleSubmissionService) OnStop(_ context.Context) error {
	return nil
}

func (service *moleSubmissionService) handleSubmit(self gsr.CommandContext, batch moleBatch) error {
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
	// 保存原批次后 Call 结算；等待仅占用 Submission，Accounting 继续处理命中。
	if err := service.submitBatch(self); err != nil {
		service.retryOrPause(self, err)
		return err
	}
	return nil
}

func (service *moleSubmissionService) handleTick(self gsr.CommandContext) error {
	if service.fault != nil {
		// 通知满队列时下次继续，原始批次和 Redis 记录始终保留。
		return self.SendName(moleAccountingName, accountingPauseCommand)
	}
	if time.Now().Before(service.retryAt) {
		return nil
	}
	var err error
	switch {
	case !service.ready && service.batch == nil:
		err = service.restoreCheckpoint(self)
	case service.receipt != nil:
		err = service.completeBatch(self)
	case service.batch != nil:
		err = service.submitBatch(self)
	}
	if err != nil {
		service.retryOrPause(self, err)
	}
	return err
}

func (service *moleSubmissionService) retryOrPause(self gsr.ServiceContext, err error) {
	var status *runtime.StatusError
	if errors.As(err, &status) && (status.Class == runtime.ErrorClassDomain && status.Code != "settlement.store_unavailable" && status.Code != "mole.accounting_unavailable") {
		service.fault = err
		_ = self.SendName(moleAccountingName, accountingPauseCommand)
		logMoleError(self, "结算提交暂停，保留原批次，修复后重启恢复", err)
		return
	}
	service.retryDelay = min(max(service.retryDelay*2, accountingPeriod), 2*time.Second)
	service.retryAt = time.Now().Add(service.retryDelay)
	logMoleError(self, "结算提交等待原批次重试", err)
}

func (service *moleSubmissionService) handleState(_ gsr.CommandContext) (map[string]any, error) {
	view := map[string]any{"ready": service.ready && service.fault == nil, "blocked": service.fault != nil}
	if service.batch != nil {
		view["batch_id"], view["sequence"] = service.batch.ID, service.batch.Sequence
	}
	return view, nil
}

// restoreCheckpoint 在开放新记账前恢复统计；有待批次时立即按原请求补交。
func (service *moleSubmissionService) restoreCheckpoint(self gsr.ServiceContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	state, err := service.store.load(ctx)
	cancel()
	if err != nil {
		return err
	}
	if state.Pending != nil {
		copied := state.Pending.clone()
		service.batch = &copied
		return service.submitBatch(self)
	}
	if err := self.SendName(moleAccountingName, accountingInitPoolCommand, state.Pool, state.Sequence); err != nil {
		return err
	}
	service.ready = true
	service.retryDelay, service.retryAt = 0, time.Time{}
	return nil
}

// submitBatch 先保存原批次，再 Call 结算并校验完整回执；结果未知时保留原请求。
func (service *moleSubmissionService) submitBatch(self gsr.ServiceContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	err := service.store.savePendingBatch(ctx, *service.batch)
	cancel()
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), moleSettlementTimeout)
	value, err := self.Cluster().Call(ctx, service.settlementNode, settlementwrite.ServiceName, settlementwrite.SettleBatchCommand, service.batch.wire())
	cancel()
	if err != nil {
		return err
	}
	receipt, err := decodeBatchReceipt(value, *service.batch)
	if err != nil {
		return err
	}
	service.receipt = &receipt
	return service.completeBatch(self)
}

// completeBatch 先持久化确认统计并清除待批次，再通知 Accounting；通知未接纳时保留回执重试。
func (service *moleSubmissionService) completeBatch(self gsr.ServiceContext) error {
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	state, err := service.store.completeBatch(ctx, *service.batch, *service.receipt)
	cancel()
	if err != nil {
		return err
	}
	if !service.ready {
		// 恢复批次只交付已确认统计，尚未初始化的 Accounting 不接收旧回执。
		if err := self.SendName(moleAccountingName, accountingInitPoolCommand, state.Pool, state.Sequence); err != nil {
			return err
		}
		service.ready = true
	} else if err := self.SendName(moleAccountingName, accountingSettlementReceiptCommand, service.receipt.clone()); err != nil {
		return err
	}
	service.batch, service.receipt = nil, nil
	service.retryDelay, service.retryAt = 0, time.Time{}
	return nil
}
