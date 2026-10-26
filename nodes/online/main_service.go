package main

import (
	"context"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type mainService struct {
	node cluster.NodeID
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 创建只负责节点启动的 MainService。
func NewMain(node cluster.NodeID) gsr.Service {
	return &mainService{node: node}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, newOnlineService())
	if err != nil {
		return err
	}
	if err := self.Node().RegisterService(onlineServiceName, id); err != nil {
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
