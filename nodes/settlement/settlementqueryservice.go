package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// settlementQueryService 只读，不与写入入口共享 Mailbox，也不拥有余额缓存。
type settlementQueryService struct {
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementQueryService)(nil)

func newSettlementQueryService(db *sql.DB) *settlementQueryService {
	service := &settlementQueryService{store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		getBatchCommand:     service.handleGetBatch,
		getOperationCommand: service.handleGetOperation,
		getBalanceCommand:   service.handleGetBalance,
	})
	return service
}
func (service *settlementQueryService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	if service.store.db == nil {
		return errors.New("settlement requires a database")
	}
	return self.Node().RegisterService(settlementReadName, self.Self())
}
func (service *settlementQueryService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}
func (service *settlementQueryService) OnStop(context.Context) error { return nil }

func (service *settlementQueryService) handleGetBatch(self gsr.CommandContext, id string) (any, error) {
	if !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBatch(ctx, id)
	return value, settlementError(self, err)
}

func (service *settlementQueryService) handleGetOperation(self gsr.CommandContext, userID uint64, id string) (any, error) {
	if userID == 0 || !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getOperation(ctx, userID, id)
	return value, settlementError(self, err)
}

func (service *settlementQueryService) handleGetBalance(self gsr.CommandContext, userID uint64) (map[string]any, error) {
	if userID == 0 {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBalance(ctx, userID)
	return value, settlementError(self, err)
}
