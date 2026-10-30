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

type settlementTarget struct {
	name     cluster.ServiceName
	id       runtime.ServiceID
	readOnly bool
}

func (target settlementTarget) service(db *sql.DB) gsr.Service {
	if target.readOnly {
		return newSettlementQueryService(db, target.name)
	}
	return newSettlementService(db, target.name)
}

// settlementSupervisor 私有 Mailbox 只接收自身 Timer，不执行 SQL 或等待业务 Call。
// 执行入口无易失账务状态，退出后可重新发布同名实例；未知结果仍由调用方按原 ID 重试。
type settlementSupervisor struct {
	db      *sql.DB
	targets []settlementTarget
}

var _ gsr.Service = (*settlementSupervisor)(nil)

func (service *settlementSupervisor) OnStart(_ context.Context, self gsr.ServiceContext) error {
	service.targets = append([]settlementTarget(nil), service.targets...)
	_, err := self.After(250*time.Millisecond, 1)
	return err
}

func (service *settlementSupervisor) Handle(self gsr.CommandContext, command runtime.CommandID, _ ...any) (any, error) {
	if command != 1 || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	if _, err := self.After(250*time.Millisecond, 1); err != nil {
		self.Exit()
		return nil, err
	}
	for index, target := range service.targets {
		err := self.Send(target.id, settlementHealthCommand)
		// 满队列是局部背压，Stopping 先等待退役和名称清理，不并行创建第二个 owner。
		if !errors.Is(err, runtime.ErrServiceNotFound) && !errors.Is(err, runtime.ErrServiceClosed) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		id, err := self.CreateService(ctx, target.service(service.db))
		cancel()
		if err != nil {
			_ = self.Logger().Error("结算入口恢复失败，稍后重试", "service", target.name, "error", err.Error())
			continue
		}
		service.targets[index].id = id
		_ = self.Logger().Info("结算入口已独立恢复", "service", target.name)
	}
	return nil, nil
}

func (service *settlementSupervisor) OnStop(context.Context) error { return nil }
