package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type mainService struct {
	node    cluster.NodeID
	db      *sql.DB
	workers int
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 创建只负责节点启动的 MainService。
func NewMain(node cluster.NodeID, db *sql.DB, workers int) gsr.Service {
	return &mainService{node: node, db: db, workers: workers}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	// 池成员各自串行处理，邮箱容量沿用 Runtime 的 YAML 配置；共享数据库由宿主关闭。
	if err := self.Node().CreateServicePool(ctx, settlementWriteName, gsr.ServicePoolConfig{
		Size: service.workers,
		NewService: func() gsr.Service {
			return newSettlementService(service.db)
		},
	}); err != nil {
		return err
	}
	if _, err := self.CreateService(ctx, newSettlementQueryService(service.db)); err != nil {
		return err
	}
	if err := self.Node().OpenCluster(service.node); err != nil {
		return err
	}
	if _, err := fmt.Printf("%s 就绪：%s\n", service.node, self.Node().ClusterListenAddress()); err != nil {
		return err
	}
	self.Exit()
	return nil
}

func (service *mainService) Handle(_ gsr.CommandContext, _ runtime.CommandID, _ ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *mainService) OnStop(_ context.Context) error {
	return nil
}
