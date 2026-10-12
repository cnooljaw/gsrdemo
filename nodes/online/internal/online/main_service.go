package online

import (
	"context"
	"encoding/json"
	"io"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/model"
)

type mainService struct {
	cfg    model.NodeConfig
	output io.Writer
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 显式组装 online 节点；当前只提供通信探测，不表示业务已实现。
func NewMain(cfg model.NodeConfig, output io.Writer) gsr.Service {
	return &mainService{cfg: cfg, output: output}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, newProbeService())
	if err != nil {
		return err
	}
	if err := self.Register("online", id); err != nil {
		return err
	}
	if err := self.Cluster().Open(cluster.NodeID(service.cfg.Listen)); err != nil {
		return err
	}
	if err := json.NewEncoder(service.output).Encode(model.NodeReady{
		Event: "ready", Instance: service.cfg.Instance, ListenAddress: self.Cluster().ListenAddress(),
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
