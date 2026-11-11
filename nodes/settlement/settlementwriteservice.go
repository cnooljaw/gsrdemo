package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	settlementWriteName                          = "settlement-write"
	settlementReadName                           = "settlement-read"
	settleBatchCommand         runtime.CommandID = 1
	getBatchCommand            runtime.CommandID = 2
	getOperationCommand        runtime.CommandID = 3
	getBalanceCommand          runtime.CommandID = 4
	settleSingleCommand        runtime.CommandID = 6
	settleEqualGroupCommand    runtime.CommandID = 7
	settleTransferGroupCommand runtime.CommandID = 8
	settlementSQLTimeout                         = 2 * time.Second
)

var (
	errInvalidSettlement     = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.invalid_request", "invalid settlement request")
	errSettlementConflict    = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.identity_conflict", "settlement identity conflicts with stored content")
	errSettlementUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "settlement storage is unavailable; query or retry the original identity")
	errWalletNotFound        = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.wallet_not_found", "wallet must be explicitly initialized")
	errBalanceOverflow       = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.balance_overflow", "wallet balance would overflow")
	errVersionOverflow       = runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.version_overflow", "wallet version would overflow")
)

// settlementWriteService 是写入池的独立成员；写命令共享事务规则，不持有钱包副本。
type settlementWriteService struct {
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementWriteService)(nil)

func newSettlementWriteService(db *sql.DB) *settlementWriteService {
	service := &settlementWriteService{store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		settleBatchCommand:         service.handleSettleBatch,
		settleSingleCommand:        service.handleSettleSingle,
		settleEqualGroupCommand:    service.handleSettleEqualGroup,
		settleTransferGroupCommand: service.handleSettleTransferGroup,
	})
	return service
}

func (service *settlementWriteService) OnStart(context.Context, gsr.ServiceContext) error {
	if service.store.db == nil {
		return errors.New("settlement requires a database")
	}
	return nil
}

func (service *settlementWriteService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *settlementWriteService) OnStop(_ context.Context) error {
	return nil
}

func (service *settlementWriteService) handleSettleBatch(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeSettlementBatch(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementWriteService) handleSettleSingle(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeSettlementSingle(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementWriteService) handleSettleEqualGroup(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeEqualGroup(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementWriteService) handleSettleTransferGroup(self gsr.CommandContext, request map[string]any) (map[string]any, error) {
	batch, err := decodeTransferGroup(request)
	if err != nil {
		return nil, err
	}
	return service.commit(self, batch)
}

func (service *settlementWriteService) commit(self gsr.ServiceContext, batch settlementBatch) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	receipt, err := service.store.settle(ctx, batch)
	if err != nil {
		return nil, settlementError(self, err)
	}
	return receipt.wire(), nil
}

func settlementError(self gsr.ServiceContext, err error) error {
	if err == nil || errors.Is(err, errInvalidSettlement) || errors.Is(err, errSettlementConflict) || errors.Is(err, errWalletNotFound) || errors.Is(err, errBalanceOverflow) || errors.Is(err, errVersionOverflow) {
		return err
	}
	_ = self.Logger().Error("结算数据库操作未确认完成", "error", err.Error())
	return errSettlementUnavailable
}
