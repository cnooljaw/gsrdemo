package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

const (
	regroupBarrierCommand runtime.CommandID = 998
	regroupReleaseCommand runtime.CommandID = 999
)

// regroupAccountingFixture 只控制旧回收通知的执行时点，状态变化仍交给真实 Accounting。
type regroupAccountingFixture struct {
	owner   gsr.Service
	delayed bool
	forgets uint64
	held    [][]any
}

var _ gsr.Service = (*regroupAccountingFixture)(nil)

func (probe *regroupAccountingFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return probe.owner.OnStart(ctx, self)
}

func (probe *regroupAccountingFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	switch command {
	case accountingForgetCommand:
		probe.forgets++
		if probe.delayed {
			probe.held = append(probe.held, append([]any(nil), args...))
			return nil, nil
		}
	case regroupBarrierCommand:
		return probe.forgets, nil
	case regroupReleaseCommand:
		count := uint64(len(probe.held))
		for _, notice := range probe.held {
			if _, err := probe.owner.Handle(self, accountingForgetCommand, notice...); err != nil {
				return nil, err
			}
		}
		probe.held = nil
		return count, nil
	}
	return probe.owner.Handle(self, command, args...)
}

func (probe *regroupAccountingFixture) OnStop(ctx context.Context) error {
	return probe.owner.OnStop(ctx)
}

// regroupEntryContext 在清理原组返回前确认旧回收已到达 Accounting，消除测试对调度顺序的依赖。
type regroupEntryContext struct{ gsr.CommandContext }

func (self *regroupEntryContext) CallName(ctx context.Context, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	value, err := self.CommandContext.CallName(ctx, name, command, args...)
	if err == nil && name == moleServiceName && command == moleDiscardGroupCommand {
		seen, barrierErr := self.CommandContext.CallName(ctx, moleAccountingName, regroupBarrierCommand)
		if barrierErr != nil {
			return nil, barrierErr
		}
		if seen != uint64(1) {
			return nil, errors.New("旧组回收通知未到达测试屏障")
		}
	}
	return value, err
}

type regroupEntryFixture struct{ gsr.Service }

var _ gsr.Service = (*regroupEntryFixture)(nil)

func (probe *regroupEntryFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return probe.Service.Handle(&regroupEntryContext{self}, command, args...)
}

type regroupMainContext struct {
	gsr.ServiceContext
	delayed bool
}

func (self *regroupMainContext) CreateService(ctx context.Context, child gsr.Service) (runtime.ServiceID, error) {
	switch service := child.(type) {
	case *moleService:
		// 目录仍指向已不存在的组，让真实 Entry 进入已有的自动换组分支。
		service.routes[1] = moleRoute{Attack: "attack-stopped", LoginID: "login", Membership: "old"}
		service.groups["attack-stopped"] = 1
	case *moleAccountingService:
		child = &regroupAccountingFixture{owner: child, delayed: self.delayed}
	case *moleEntryService:
		child = &regroupEntryFixture{Service: child}
	}
	return self.ServiceContext.CreateService(ctx, child)
}

type regroupMainFixture struct {
	gsr.Service
	delayed bool
}

var _ gsr.Service = (*regroupMainFixture)(nil)

func (probe *regroupMainFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return probe.Service.OnStart(ctx, &regroupMainContext{ServiceContext: self, delayed: probe.delayed})
}

func (probe *regroupMainFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return probe.Service.Handle(self, command, args...)
}

// TestMoleRegroupRestoresAccountBeforeJoining 保护原组回收早于或晚于换组时，新组都能实际命中并完成结算。
func TestMoleRegroupRestoresAccountBeforeJoining(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "cleanup before retry"
		if delayed {
			name = "cleanup after join"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSettlementFixture()
			settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
			online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
			routes := map[cluster.NodeID]string{"settlement": settlement, "online": online}
			main := &regroupMainFixture{Service: newMoleTestMain(t, fixture), delayed: delayed}
			address, _ := startMoleTestNode(t, "mole", routes, main)
			routes["mole"] = address
			snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
			if snapshot["attack_name"] == "attack-stopped" {
				t.Fatal("进入未切换到可用组")
			}
			released, err := moleRPC(t, routes, "mole", moleAccountingName, regroupReleaseCommand)
			want := uint64(0)
			if delayed {
				want = 1
			}
			if err != nil || released != want {
				t.Fatalf("回收时序注入失败: released=%v want=%d err=%v", released, want, err)
			}
			hit := testHitTarget(t, routes, snapshot, 1, "login", "after-regroup")
			waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
		})
	}
}
