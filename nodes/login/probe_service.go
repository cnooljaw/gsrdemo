package main

import (
	"context"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
)

// probeService 只验证原生参数和跨进程调用，不提供 login 业务。
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

func (service *probeService) handleProbe(_ gsr.CommandContext, operationID, text, _, _, _ string) (map[string]any, error) {
	if operationID == "" || len(operationID) > 64 || text == "" || len(text) > 256 {
		return nil, errors.New("invalid probe")
	}
	return map[string]any{"operation_id": operationID, "text": text, "node": "login"}, nil
}

func (service *probeService) OnStop(_ context.Context) error {
	return nil
}
