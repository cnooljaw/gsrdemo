package main

import (
	"context"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type mainService struct {
	node           cluster.NodeID
	settlementNode cluster.NodeID
	journal        landlordJournal
	pending        []*landlordOperation
}

var _ gsr.Service = (*mainService)(nil)

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, newLandlordService(service.journal, service.settlementNode))
	if err != nil {
		return err
	}
	if err := self.Node().RegisterService(landlordName, id); err != nil {
		return err
	}
	for _, result := range service.pending {
		if _, err := self.CreateService(ctx, newAttackService("", service.journal, result, service.settlementNode)); err != nil {
			return err
		}
	}
	if err := self.Node().OpenCluster(service.node); err != nil {
		return err
	}
	if _, err := fmt.Printf("%s 就绪：Cluster=%s（随机房，3 人一桌）\n", service.node, self.Node().ClusterListenAddress()); err != nil {
		return err
	}
	self.Exit()
	return nil
}

func (service *mainService) Handle(_ gsr.CommandContext, _ runtime.CommandID, _ ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *mainService) OnStop(context.Context) error {
	return nil
}
