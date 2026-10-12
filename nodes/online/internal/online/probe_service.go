package online

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	diagnosticpb "github.com/cnooljaw/gsrdemo/common/gen/demo/diagnostic/v1"
	"google.golang.org/protobuf/proto"
)

// probeService 只验证字节协议和跨进程调用，不提供 online 业务。
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

func (service *probeService) handleProbe(_ gsr.CommandContext, payload []byte) ([]byte, error) {
	request := &diagnosticpb.NodeProbeRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return nil, fmt.Errorf("decode online probe: %w", err)
	}
	probe := request.GetProbe()
	if probe.GetOperationId() == "" || len(probe.GetOperationId()) > 64 || probe.GetText() == "" || len(probe.GetText()) > 256 {
		return nil, errors.New("invalid probe")
	}

	return proto.Marshal(&diagnosticpb.NodeProbeResponse{OperationId: probe.GetOperationId(), Text: probe.GetText(), Node: "online"})
}

func (service *probeService) OnStop(_ context.Context) error {
	return nil
}
