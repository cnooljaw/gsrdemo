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

// NewMain 组装纯内存游戏与远端提交 Service，不打开数据库。
func NewMain(node cluster.NodeID) gsr.Service {
	return &mainService{node: node}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for _, child := range []struct {
		name    cluster.ServiceName
		service gsr.Service
	}{
		{moleAccountingName, newMoleAccountingService()},
		{moleSubmissionName, newMoleSubmissionService()},
		{moleServiceName, newMoleService()},
	} {
		id, err := self.CreateService(ctx, child.service)
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(child.name, id); err != nil {
			return err
		}
	}
	if err := self.Node().OpenCluster(service.node); err != nil {
		return err
	}
	if _, err := fmt.Printf("%s 监听就绪：%s（结算初始化完成后接纳进入）\n", service.node, self.Node().ClusterListenAddress()); err != nil {
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
