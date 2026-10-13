package mole

import (
	"context"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
)

// probeService 只验证原生参数和跨进程调用，不提供 mole 业务。
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

func (service *probeService) handleProbe(self gsr.CommandContext, operationID, text, accessNode, agentName, agentToken string) (map[string]any, error) {
	if operationID == "" || len(operationID) > 64 || text == "" || len(text) > 256 {
		return nil, errors.New("invalid probe")
	}
	if accessNode != "access" || len(agentToken) != 32 || agentName != "agent/"+agentToken {
		return nil, errors.New("invalid probe callback")
	}
	// Send 成功只代表本地接纳，最终送达由客户端看到 Push 验证。
	if err := self.Cluster().Send(cluster.NodeID(accessNode), cluster.ServiceName(agentName), command.ProbePush, agentToken, operationID, text, "mole"); err != nil {
		return nil, err
	}
	return map[string]any{"operation_id": operationID, "text": text, "node": "mole"}, nil
}

func (service *probeService) OnStop(_ context.Context) error {
	return nil
}
