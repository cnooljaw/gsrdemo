package main

import (
	"context"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

type mainService struct {
	node cluster.NodeID
	gate gateOptions
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 组装 access 的 Cluster、Gate 与连接私有 Agent。
func NewMain(node cluster.NodeID, options gateOptions) gsr.Service {
	return &mainService{node: node, gate: options}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if err := self.Node().OpenCluster(service.node); err != nil {
		return err
	}
	registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
	if err != nil {
		return err
	}
	address, err := self.Node().CreateGate(ctx, gsr.GateConfig{
		SecurityMode:          gate.SecurityModeMAC,
		Address:               service.gate.Address,
		MaxConnections:        service.gate.MaxConnections,
		MaxFrameBytes:         service.gate.MaxFrameBytes,
		MaxOutboundQueueBytes: service.gate.MaxOutboundQueueBytes,
		NewAgent: func(client gate.ClientSession) gsr.Service {
			agent := newAgent(client, registry)
			agent.node = service.node
			return agent
		},
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Printf("%s 就绪：Cluster=%s Gate=ws://%s/ws\n", service.node, self.Node().ClusterListenAddress(), address); err != nil {
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
