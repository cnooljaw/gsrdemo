package main

import (
	"context"
	"errors"
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"testing"
	"time"
)

func waitMoleReady(t *testing.T, routes map[cluster.NodeID]string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := moleRPC(t, routes, "mole", moleServiceName, molePoolCommand); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("内存状态未初始化")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMoleBalanceFailureDoesNotCreateAccount 保护余额查询失败不建立账户或入座，恢复后仍可正常进入和命中。
func TestMoleBalanceFailureDoesNotCreateAccount(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.failBalance.Store(true)
	routes, _ := prepareMoleCluster(t, fixture)
	waitMoleReady(t, routes)
	if _, err := moleRPC(t, routes, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uint64(1), "login", "access-01", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent"); !errors.Is(err, errAccountingUnavailable) {
		t.Fatal(err)
	}
	if _, err := moleRPC(t, routes, "mole", moleServiceName, moleBalanceCommand, uint64(1)); !errors.Is(err, errAccountingUnavailable) {
		t.Fatal("失败留下账户", err)
	}
	fixture.failBalance.Store(false)
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "after-failure")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}

type delayedMoleReply struct {
	gsr.Service
	delayed   bool
	committed chan map[string]any
}

func (owner *delayedMoleReply) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	value, err := owner.Service.Handle(self, command, args...)
	if command == moleJoinCommand && err == nil && !owner.delayed {
		owner.delayed = true
		owner.committed <- value.(map[string]any)
		time.Sleep(moleCallTimeout + 100*time.Millisecond)
	}
	return value, err
}

type atomicMoleMainContext struct {
	gsr.ServiceContext
	committed chan map[string]any
}

func (self *atomicMoleMainContext) CreateService(ctx context.Context, child gsr.Service) (runtime.ServiceID, error) {
	if _, ok := child.(*moleService); ok {
		child = &delayedMoleReply{Service: child, committed: self.committed}
	}
	return self.ServiceContext.CreateService(ctx, child)
}

type atomicMoleMain struct {
	gsr.Service
	committed chan map[string]any
}

func (main *atomicMoleMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return main.Service.OnStart(ctx, &atomicMoleMainContext{self, main.committed})
}

// TestMoleEnterTimeoutKeepsCompleteMembership 保护 Enter 回包超时只留下完整入座，固定入口离开后成员和账户一起回收。
func TestMoleEnterTimeoutKeepsCompleteMembership(t *testing.T) {
	fixture := newSettlementFixture()
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"settlement": settlement, "online": online}
	committed := make(chan map[string]any, 1)
	address, _ := startMoleTestNode(t, "mole", routes, &atomicMoleMain{newMoleTestMain(t, fixture), committed})
	routes["mole"] = address
	waitMoleReady(t, routes)
	if _, err := moleRPC(t, routes, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uint64(1), "login", "access-01", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent"); !errors.Is(err, runtime.ErrCallTimeout) {
		t.Fatal("未触发真实入座回包超时", err)
	}
	snapshot := <-committed
	again := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if again["attack_name"] != snapshot["attack_name"] || again["seat"] != snapshot["seat"] {
		t.Fatal("未知结果重试换组或换座")
	}
	if value, err := moleRPC(t, routes, "mole", moleServiceName, moleLeaveCommand, uint64(1), "login"); err != nil || value != true {
		t.Fatal("固定入口离开失败", value, err)
	}
	if _, err := moleRPC(t, routes, "mole", moleServiceName, moleBalanceCommand, uint64(1)); !errors.Is(err, errAccountingUnavailable) {
		t.Fatal("完整离开后账户残留", err)
	}
}
