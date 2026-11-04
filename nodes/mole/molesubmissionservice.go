package main

import (
	"context"
	"crypto/rand"
	"errors"
	jitter "math/rand/v2"
	"reflect"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	moleSubmissionName                        = "mole-submission"
	submissionBatchCommand  runtime.CommandID = 1
	submissionTickCommand   runtime.CommandID = 10
	submissionStateCommand  runtime.CommandID = 11
	submissionResultCommand runtime.CommandID = 20
	settlementReplyWait                       = 5 * time.Second
)

// moleSubmissionService 独占 Redis 检查点和结算调用；只推进一个在途批次，不持有钱包。
type moleSubmissionService struct {
	store          moleCheckpoint
	settlementNode cluster.NodeID
	replyNode      cluster.NodeID
	replyName      cluster.ServiceName
	batch          *moleBatch
	receipt        *batchReceipt
	ready          bool
	fault          error
	retryAt        time.Time
	retryDelay     time.Duration
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*moleSubmissionService)(nil)

func newMoleSubmissionService(store moleCheckpoint, settlementNode, replyNode cluster.NodeID) *moleSubmissionService {
	service := &moleSubmissionService{store: store, settlementNode: settlementNode, replyNode: replyNode}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		submissionBatchCommand:  service.handleSubmit,
		submissionTickCommand:   service.handleTick,
		submissionStateCommand:  service.handleState,
		submissionResultCommand: service.handleResult,
	})
	return service
}

func (service *moleSubmissionService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	service.replyName = cluster.ServiceName("mole-settle-" + rand.Text())
	if err := self.Node().RegisterService(service.replyName, self.Self()); err != nil {
		return err
	}
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
	// 保存原批次后异步提交；等待回执期间仍可处理状态查询和重复通知。
	if err := service.advance(self); err != nil {
		service.failed(self, err)
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
	err := service.advance(self)
	if err != nil {
		service.failed(self, err)
	}
	return err
}

func (service *moleSubmissionService) failed(self gsr.ServiceContext, err error) {
	var status *runtime.StatusError
	if errors.As(err, &status) && (status.Class == runtime.ErrorClassDomain && status.Code != "settlement.store_unavailable" && status.Code != "mole.accounting_unavailable" && status.Code != "settlement.busy") {
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

func (service *moleSubmissionService) advance(self gsr.ServiceContext) error {
	if !service.ready && service.batch == nil {
		ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
		state, err := service.store.load(ctx)
		cancel()
		if err != nil {
			return err
		}
		if state.Pending == nil {
			if err := self.SendName(moleAccountingName, accountingInitPoolCommand, state.Pool, state.Sequence); err != nil {
				return err
			}
			service.ready = true
			service.retryDelay, service.retryAt = 0, time.Time{}
			return nil
		}
		copied := state.Pending.clone()
		service.batch = &copied
	}
	if service.batch == nil {
		return nil
	}
	if service.receipt == nil {
		ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
		err := service.store.prepare(ctx, *service.batch)
		cancel()
		if err != nil {
			return err
		}
		if err := self.Cluster().Send(service.settlementNode, "settlement-submit", 1, service.batch.wire(), string(service.replyNode), string(service.replyName)); err != nil {
			return err
		}
		service.retryAt = time.Now().Add(settlementReplyWait + jitter.N(500*time.Millisecond))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), moleCallTimeout)
	state, err := service.store.finish(ctx, *service.batch, *service.receipt)
	cancel()
	if err != nil {
		return err
	}
	if !service.ready {
		// 先恢复确认统计，才开放新记账；恢复批次不再向尚未初始化的 Accounting 交付旧回执。
		if err := self.SendName(moleAccountingName, accountingInitPoolCommand, state.Pool, state.Sequence); err != nil {
			return err
		}
		service.ready = true
	} else if err := self.SendName(moleAccountingName, accountingSettledCommand, service.receipt.clone()); err != nil {
		return err
	}
	service.batch, service.receipt = nil, nil
	service.retryDelay, service.retryAt = 0, time.Time{}
	return nil
}

func (service *moleSubmissionService) handleResult(self gsr.CommandContext, id string, fields map[string]any, code string) error {
	if service.batch == nil || service.batch.ID != id || service.receipt != nil || service.fault != nil {
		return nil
	}
	if code != "" {
		err := runtime.NewStatusError(runtime.ErrorClassDomain, runtime.ErrorCode(code), "settlement request did not complete")
		service.failed(self, err)
		return nil
	}
	receipt, err := decodeReceipt(fields, *service.batch)
	if err != nil {
		// 不接受格式错误的结果，原请求继续保留；不能把坏回执当成拒绝入账。
		logMoleError(self, "忽略非法结算回执", err)
		return nil
	}
	service.receipt = &receipt
	service.retryAt = time.Time{}
	if err := service.advance(self); err != nil {
		service.failed(self, err)
		return err
	}
	return nil
}
