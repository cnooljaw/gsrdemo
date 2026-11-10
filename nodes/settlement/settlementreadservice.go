package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// settlementReadService 是读池的独立成员，只查询权威数据库，不持有余额缓存。
type settlementReadService struct {
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementReadService)(nil)

func newSettlementReadService(db *sql.DB) *settlementReadService {
	service := &settlementReadService{store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		getBatchCommand:     service.handleGetBatch,
		getOperationCommand: service.handleGetOperation,
		getBalanceCommand:   service.handleGetBalance,
	})
	return service
}

func (service *settlementReadService) OnStart(context.Context, gsr.ServiceContext) error {
	if service.store.db == nil {
		return errors.New("settlement requires a database")
	}
	return nil
}

func (service *settlementReadService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *settlementReadService) OnStop(context.Context) error {
	return nil
}

func (service *settlementReadService) handleGetBatch(self gsr.CommandContext, id string) (any, error) {
	if !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBatch(ctx, id)
	return value, settlementError(self, err)
}

func (service *settlementReadService) handleGetOperation(self gsr.CommandContext, userID uint64, id string) (any, error) {
	if userID == 0 || !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getOperation(ctx, userID, id)
	return value, settlementError(self, err)
}

func (service *settlementReadService) handleGetBalance(self gsr.CommandContext, userID uint64) (map[string]any, error) {
	if userID == 0 {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBalance(ctx, userID)
	return value, settlementError(self, err)
}
