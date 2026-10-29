package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	settlementServiceName                   = "settlement"
	settleBatchCommand    runtime.CommandID = 1
	getBatchCommand       runtime.CommandID = 2
	getOperationCommand   runtime.CommandID = 3
	getBalanceCommand     runtime.CommandID = 4
	settleSingleCommand   runtime.CommandID = 6
	settleGroupCommand    runtime.CommandID = 7
	settlementSQLTimeout                    = 2 * time.Second
)

var (
	errInvalidSettlement     = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.invalid_request", "invalid settlement request")
	errSettlementConflict    = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.identity_conflict", "settlement identity conflicts with stored content")
	errSettlementUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "settlement storage is unavailable; query or retry the original identity")
	errWalletNotFound        = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.wallet_not_found", "wallet must be explicitly initialized")
)

// settlementService 唯一管理持久银子；三个写入口共享事务，不持有游戏、连接或内存钱包副本。
type settlementService struct {
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementService)(nil)

func newSettlementService(db *sql.DB) *settlementService {
	service := &settlementService{store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		settleBatchCommand: service.settleBatch, settleSingleCommand: service.settleSingle, settleGroupCommand: service.settleGroup,
		getBatchCommand: service.getBatch, getOperationCommand: service.getOperation, getBalanceCommand: service.getBalance,
	})
	return service
}

func (service *settlementService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	if service.store.db == nil {
		return errors.New("settlement requires a database")
	}
	return nil
}

func (service *settlementService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
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

func (service *settlementService) getBatch(self gsr.CommandContext, id string) (any, error) {
	if !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBatch(ctx, id)
	return value, settlementError(self, err)
}

func (service *settlementService) getOperation(self gsr.CommandContext, userID uint64, id string) (any, error) {
	if userID == 0 || !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getOperation(ctx, userID, id)
	return value, settlementError(self, err)
}

func (service *settlementService) getBalance(self gsr.CommandContext, userID uint64) (map[string]any, error) {
	if userID == 0 {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBalance(ctx, userID)
	return value, settlementError(self, err)
}

func settlementError(self gsr.ServiceContext, err error) error {
	if err == nil || errors.Is(err, errInvalidSettlement) || errors.Is(err, errSettlementConflict) || errors.Is(err, errWalletNotFound) {
		return err
	}
	if logErr := self.Logger().Error("结算数据库操作未确认完成", "error", err.Error()); logErr != nil {
		_, _ = fmt.Fprintln(os.Stderr, "结算数据库操作未确认完成", err, "log:", logErr)
	}
	return errSettlementUnavailable
}
