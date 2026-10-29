package main

import (
	"context"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type mainService struct {
	node       cluster.NodeID
	checkpoint moleCheckpoint
	onStopped  func(error)
}

var _ gsr.Service = (*mainService)(nil)

// NewMain 组装内存游戏与持有 Redis 检查点的提交 Service，不访问 SQL。
func NewMain(node cluster.NodeID, checkpoint moleCheckpoint, onStopped func(error)) gsr.Service {
	return &mainService{node: node, checkpoint: checkpoint, onStopped: onStopped}
}

func (service *mainService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	accounting, directory := newMoleAccountingService(moleSubmissionCount), newMoleService()
	accounting.onStopped, directory.onStopped = service.onStopped, service.onStopped
	for _, child := range []struct {
		name    cluster.ServiceName
		service gsr.Service
	}{
		{moleAccountingName, accounting},
		{moleServiceName, directory},
	} {
		id, err := self.CreateService(ctx, child.service)
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(child.name, id); err != nil {
			return err
		}
	}
	for lane := 0; lane < moleSubmissionCount; lane++ {
		id, err := self.CreateService(ctx, newMoleSubmissionService(service.checkpoint.forLane(lane), uint64(lane)))
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(submissionName(lane), id); err != nil {
			return err
		}
	}
	if err := createMoleEntries(ctx, self); err != nil {
		return err
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

func createMoleEntries(ctx context.Context, self gsr.ServiceContext) error {
	for lane := uint64(0); lane < moleEntryCount; lane++ {
		id, err := self.CreateService(ctx, newMoleEntryService(lane))
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(cluster.ServiceName(moleEntryName(lane)), id); err != nil {
			return err
		}
	}
	return nil
}
