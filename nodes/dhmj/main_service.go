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

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, newDirectory())
	if err != nil {
		return err
	}
	if err := self.Node().RegisterService(directoryName, id); err != nil {
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

func (*mainService) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (*mainService) OnStop(context.Context) error { return nil }
