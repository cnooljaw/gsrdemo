package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	settlementSubmitName                       = "settlement-submit"
	settlementResultCommand  runtime.CommandID = 20
	settlementExecuteCommand runtime.CommandID = 1
	defaultSettlementWorkers                   = 8
	settlementQueueWait                        = 2 * time.Second
)

var errSettlementBusy = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.busy", "settlement workers are busy")

// settlementSubmitService 只分配任务，不等待 SQL；执行队列就是各 Service 的有界 Mailbox。
type settlementSubmitService struct {
	db         *sql.DB
	count      int
	workers    []runtime.ServiceID
	next       int
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementSubmitService)(nil)

func newSettlementSubmitService(db *sql.DB, count int) *settlementSubmitService {
	service := &settlementSubmitService{db: db, count: count}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		settleBatchCommand:  service.handleBatch,
		settleSingleCommand: service.handleSingle,
		settleGroupCommand:  service.handleGroup,
	})
	return service
}

func (service *settlementSubmitService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if service.db == nil || service.count < 1 || service.count > 64 {
		return errors.New("settlement requires MySQL and 1..64 workers")
	}
	for range service.count {
		id, err := self.CreateService(ctx, newSettlementExecutor(service.db))
		if err != nil {
			return err
		}
		service.workers = append(service.workers, id)
	}
	return self.Node().RegisterService(settlementSubmitName, self.Self())
}

func (service *settlementSubmitService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *settlementSubmitService) OnStop(context.Context) error {
	return nil
}

func (service *settlementSubmitService) handleBatch(self gsr.CommandContext, request map[string]any, replyNode, replyService string) error {
	batch, err := decodeSettlementBatch(request)
	return service.dispatch(self, batch, requestString(request, "batch_id"), replyNode, replyService, err)
}

func (service *settlementSubmitService) handleSingle(self gsr.CommandContext, request map[string]any, replyNode, replyService string) error {
	batch, err := decodeSettlementSingle(request)
	return service.dispatch(self, batch, requestString(request, "operation_id"), replyNode, replyService, err)
}

func (service *settlementSubmitService) handleGroup(self gsr.CommandContext, request map[string]any, replyNode, replyService string) error {
	batch, err := decodeSettlementGroup(request)
	return service.dispatch(self, batch, requestString(request, "operation_id"), replyNode, replyService, err)
}

func requestString(request map[string]any, key string) string {
	value, _ := request[key].(string)
	return value
}

func (service *settlementSubmitService) dispatch(self gsr.ServiceContext, batch settlementBatch, id, replyNode, replyService string, decodeErr error) error {
	if !validSettlementID(replyNode) || !validSettlementID(replyService) {
		return errInvalidSettlement
	}
	if decodeErr != nil {
		return sendSettlementResult(self, replyNode, replyService, id, nil, decodeErr)
	}
	task := settlementTask{batch: batch, id: id, replyNode: replyNode, replyService: replyService, deadline: time.Now().Add(settlementQueueWait)}
	// 每次从不同执行者开始，正常路径一次投递即可；过载最多检查 N 个有界邮箱，不等待空位。
	for range len(service.workers) {
		worker := service.workers[service.next]
		service.next = (service.next + 1) % len(service.workers)
		if err := self.Send(worker, settlementExecuteCommand, task); err == nil {
			return nil
		} else if !errors.Is(err, runtime.ErrMailboxFull) {
			_ = self.Logger().Error("结算执行 Service 未接纳", "service", worker, "error", err.Error())
		}
	}
	return sendSettlementResult(self, replyNode, replyService, id, nil, errSettlementBusy)
}

type settlementTask struct {
	batch        settlementBatch
	id           string
	replyNode    string
	replyService string
	deadline     time.Time
}

// settlementExecutor 只拥有当前事务，钱包与终局幂等记录归 MySQL。
type settlementExecutor struct {
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementExecutor)(nil)

func newSettlementExecutor(db *sql.DB) *settlementExecutor {
	service := &settlementExecutor{store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		settlementExecuteCommand: service.handleExecute,
	})
	return service
}

func (service *settlementExecutor) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (service *settlementExecutor) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *settlementExecutor) handleExecute(self gsr.CommandContext, task settlementTask) error {
	if !time.Now().Before(task.deadline) {
		return sendSettlementResult(self, task.replyNode, task.replyService, task.id, nil, errSettlementBusy)
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	receipt, err := service.store.settle(ctx, task.batch)
	cancel()
	if err != nil {
		return sendSettlementResult(self, task.replyNode, task.replyService, task.id, nil, settlementError(self, err))
	}
	return sendSettlementResult(self, task.replyNode, task.replyService, task.id, receipt.wire(), nil)
}

func (service *settlementExecutor) OnStop(context.Context) error {
	return nil
}

func sendSettlementResult(self gsr.ServiceContext, node, name, id string, receipt map[string]any, err error) error {
	code := ""
	if err != nil {
		code = "settlement.store_unavailable"
		var status *runtime.StatusError
		if errors.As(err, &status) {
			code = string(status.Code)
		}
	}
	if receipt == nil {
		receipt = map[string]any{}
	}
	return self.Cluster().Send(cluster.NodeID(node), cluster.ServiceName(name), settlementResultCommand, id, receipt, code)
}
