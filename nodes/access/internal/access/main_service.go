package access

import (
	"context"
	"encoding/json"
	"io"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	diagnosticpb "github.com/cnooljaw/gsrdemo/common/gen/demo/diagnostic/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
)

type mainService struct {
	cfg    model.NodeConfig
	output io.Writer
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 组装诊断 Gate 和连接 Agent，不提供认证或游戏行为。
func NewMain(cfg model.NodeConfig, output io.Writer) gsr.Service {
	return &mainService{cfg: cfg, output: output}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	registry, err := protocol.NewRegistry(diagnosticpb.MessageCatalogDemoDiagnosticV1Diagnostic)
	if err != nil {
		return err
	}
	if err := self.Cluster().Open(cluster.NodeID(service.cfg.Listen)); err != nil {
		return err
	}
	address, err := self.OpenGate(ctx, gsr.GateConfig{
		Protocol: gate.ProtocolPlain, Address: service.cfg.GateAddress,
		MaxConnections: 128, MaxFrameBytes: 4096, MaxOutboundQueueBytes: 64 << 10,
		NewAgent: func(client gate.ClientSession) gsr.Service {
			return newAgent(client, registry)
		},
	})
	if err != nil {
		return err
	}
	if err := json.NewEncoder(service.output).Encode(model.NodeReady{
		Event: "ready", Instance: service.cfg.Instance,
		ListenAddress: self.Cluster().ListenAddress(), GateAddress: address,
	}); err != nil {
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
