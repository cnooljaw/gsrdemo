package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
)

type mainService struct {
	node cluster.NodeID
	db   *sql.DB
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 创建只负责节点启动的 MainService。
func NewMain(node cluster.NodeID, db *sql.DB) gsr.Service {
	return &mainService{node: node, db: db}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	writes, reads := routing.Services()
	targets := make([]settlementTarget, 0, len(writes)+len(reads))
	for _, group := range []struct {
		names    []string
		readOnly bool
	}{{writes, false}, {reads, true}} {
		for _, name := range group.names {
			target := settlementTarget{name: cluster.ServiceName(name), readOnly: group.readOnly}
			id, err := self.CreateService(ctx, target.service(service.db))
			if err != nil {
				return err
			}
			target.id = id
			targets = append(targets, target)
		}
	}
	if _, err := self.CreateService(ctx, &settlementSupervisor{db: service.db, targets: targets}); err != nil {
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
