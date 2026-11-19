package main

import (
	"context"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type mainService struct {
	node    cluster.NodeID
	rules   roomRules
	journal settlementJournal
	pending []roundSettlement
	next    int
}

//rpc:service dhmjmain private
const recoverSettlementCommand runtime.CommandID = 1

var _ gsr.Service = (*mainService)(nil)

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, newDirectory(service.rules, service.journal))
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
	return service.scheduleRecovery(self)
}

func (service *mainService) scheduleRecovery(self gsr.ServiceContext) error {
	if len(service.pending) == 0 {
		self.Exit()
		return nil
	}
	_, err := self.After(pulsePeriod, recoverSettlementCommand)
	return err
}

func (service *mainService) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != recoverSettlementCommand || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	job := &service.pending[service.next]
	job.advance(self, service.journal)
	if job.cleared || job.paused {
		last := len(service.pending) - 1
		*job = service.pending[last]
		service.pending[last] = roundSettlement{}
		service.pending = service.pending[:last]
	} else {
		service.next++
	}
	if service.next >= len(service.pending) {
		service.next = 0
	}
	return nil, service.scheduleRecovery(self)
}

func (*mainService) OnStop(context.Context) error { return nil }
