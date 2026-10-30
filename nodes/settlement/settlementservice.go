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
	settleBatchCommand      runtime.CommandID = 1
	getBatchCommand         runtime.CommandID = 2
	getOperationCommand     runtime.CommandID = 3
	getBalanceCommand       runtime.CommandID = 4
	settleSingleCommand     runtime.CommandID = 6
	settleGroupCommand      runtime.CommandID = 7
	settlementSQLTimeout                      = 2 * time.Second
	settlementHealthCommand runtime.CommandID = 9
	// 六个入口最多接纳 6*(4+1)=30 个排队/执行中的 Call，低于 v1.0.2 单连接的 32 个槽位。
	// 不以大队列掩盖慢请求，给同连接的其他目标留出接纳机会。
	settlementMailboxCapacity = 4
)

var (
	errInvalidSettlement     = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.invalid_request", "invalid settlement request")
	errSettlementConflict    = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.identity_conflict", "settlement identity conflicts with stored content")
	errSettlementUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "settlement storage is unavailable; query or retry the original identity")
	errWalletNotFound        = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.wallet_not_found", "wallet must be explicitly initialized")
)

// settlementService 是一个独立写入入口；三个命令共享事务规则，不持有钱包副本。
type settlementService struct {
	name       cluster.ServiceName
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementService)(nil)

func newSettlementService(db *sql.DB, name cluster.ServiceName) *settlementService {
	service := &settlementService{name: name, store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		settleBatchCommand: service.settleBatch, settleSingleCommand: service.settleSingle, settleGroupCommand: service.settleGroup,
	})
	return service
}

func (service *settlementService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	if service.store.db == nil {
		return errors.New("settlement requires a database")
	}
	return self.Node().RegisterService(service.name, self.Self())
}

func (service *settlementService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == settlementHealthCommand && self.Source() != 0 {
		return nil, nil
	}
	return service.dispatcher.Handle(self, command, args...)
}

func (service *settlementService) OnStop(_ context.Context) error {
	return nil
}

func (service *settlementService) settleBatch(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeSettlementBatch(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementService) settleSingle(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeSettlementSingle(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementService) settleGroup(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeSettlementGroup(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementService) commit(self gsr.ServiceContext, batch settlementBatch) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	receipt, err := service.store.settle(ctx, batch)
	if err != nil {
		return nil, settlementError(self, err)
	}
	return receipt.wire(), nil
}

func settlementError(self gsr.ServiceContext, err error) error {
	if err == nil || errors.Is(err, errInvalidSettlement) || errors.Is(err, errSettlementConflict) || errors.Is(err, errWalletNotFound) {
		return err
	}
	_ = self.Logger().Error("结算数据库操作未确认完成", "error", err.Error())
	return errSettlementUnavailable
}
