package mole

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	diagnosticpb "github.com/cnooljaw/gsrdemo/common/gen/demo/diagnostic/v1"
	"google.golang.org/protobuf/proto"
)

// probeService 只验证字节协议和跨进程调用，不提供 mole 业务。
type probeService struct {
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*probeService)(nil)

func newProbeService() *probeService {
	service := &probeService{}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{command.Probe: service.handleProbe})
	return service
}

func (service *probeService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

func (service *probeService) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, id, args...)
}

func (service *probeService) handleProbe(self gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &diagnosticpb.NodeProbeRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, fmt.Errorf("decode mole probe: %w", err)
	}
	probe := request.GetProbe()
	if probe.GetOperationId() == "" || len(probe.GetOperationId()) > 64 || probe.GetText() == "" || len(probe.GetText()) > 256 {
		return nil, errors.New("invalid probe")
	}
	if request.GetAccessNode() != "access" || len(request.GetAgentToken()) != 32 || request.GetAgentName() != "probe-agent/"+request.GetAgentToken() {
		return nil, errors.New("invalid probe callback")
	}
	push, err := proto.Marshal(&diagnosticpb.AgentProbePush{
		Push:       &diagnosticpb.ProbePush{OperationId: probe.GetOperationId(), Text: probe.GetText(), Node: "mole"},
		AgentToken: request.GetAgentToken(),
	})
	if err != nil {
		return nil, err
	}
	// Send 成功只代表本地接纳，最终送达由客户端看到 Push 验证。
	if err := self.Cluster().Send(cluster.NodeID(request.GetAccessNode()), cluster.ServiceName(request.GetAgentName()), command.ProbePush, push); err != nil {
		return nil, err
	}
	return proto.Marshal(&diagnosticpb.NodeProbeResponse{OperationId: probe.GetOperationId(), Text: probe.GetText(), Node: "mole"})
}

func (service *probeService) OnStop(_ context.Context) error {
	return nil
}
