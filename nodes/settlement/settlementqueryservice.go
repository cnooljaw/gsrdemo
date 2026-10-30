package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// settlementQueryService 只读，不与写入入口共享 Mailbox，也不拥有余额缓存。
type settlementQueryService struct {
	name       cluster.ServiceName
	store      settlementStore
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*settlementQueryService)(nil)

func newSettlementQueryService(db *sql.DB, name cluster.ServiceName) *settlementQueryService {
	service := &settlementQueryService{name: name, store: settlementStore{db: db}}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		getBatchCommand: service.getBatch, getOperationCommand: service.getOperation, getBalanceCommand: service.getBalance,
	})
	return service
}
func (service *settlementQueryService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	if service.store.db == nil {
		return errors.New("settlement requires a database")
	}
	return self.Node().RegisterService(service.name, self.Self())
}
func (service *settlementQueryService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == settlementHealthCommand && self.Source() != 0 {
		return nil, nil
	}
	return service.dispatcher.Handle(self, command, args...)
}
func (service *settlementQueryService) OnStop(context.Context) error { return nil }

func (service *settlementQueryService) getBatch(self gsr.CommandContext, id string) (any, error) {
	if !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBatch(ctx, id)
	return value, settlementError(self, err)
}

func (service *settlementQueryService) getOperation(self gsr.CommandContext, userID uint64, id string) (any, error) {
	if userID == 0 || !validSettlementID(id) {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getOperation(ctx, userID, id)
	return value, settlementError(self, err)
}

func (service *settlementQueryService) getBalance(self gsr.CommandContext, userID uint64) (map[string]any, error) {
	if userID == 0 {
		return nil, errInvalidSettlement
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementSQLTimeout)
	defer cancel()
	value, err := service.store.getBalance(ctx, userID)
	return value, settlementError(self, err)
}
