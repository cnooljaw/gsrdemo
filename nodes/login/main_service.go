package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type mainService struct {
	node  cluster.NodeID
	db    *sql.DB
	store tokenStore
	ttl   time.Duration
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 创建只负责节点启动的 MainService。
func NewMain(node cluster.NodeID, db *sql.DB, store tokenStore, ttl time.Duration) gsr.Service {
	return &mainService{node: node, db: db, store: store, ttl: ttl}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for _, entry := range []struct {
		name    string
		service gsr.Service
	}{
		{tokenServiceName, newTokenService(service.store, service.ttl)},
		{authServiceName, newAuthService(service.db)},
	} {
		id, err := self.CreateService(ctx, entry.service)
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(cluster.ServiceName(entry.name), id); err != nil {
			return err
		}
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
