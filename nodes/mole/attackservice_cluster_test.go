package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type moleFaultMain struct {
	main gsr.Service
}

var _ gsr.Service = (*moleFaultMain)(nil)

func (service *moleFaultMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	broken := newAttackService("attack-broken")
	// 仅测试注入非法座位，下一次加入触发真实 AttackService Handler panic。
	broken.members[20] = attackMember{UserID: 20, LoginID: "broken", Seat: 99}
	if _, err := self.CreateService(ctx, broken); err != nil {
		return err
	}
	return service.main.OnStart(ctx, self)
}

func (service *moleFaultMain) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.main.Handle(self, command, args...)
}

func (service *moleFaultMain) OnStop(ctx context.Context) error {
	return service.main.OnStop(ctx)
}

// TestMolePanickedGroupDoesNotStopOtherGroups 保护一个真实 AttackService panic 后，其他组继续响应和记账。
func TestMolePanickedGroupDoesNotStopOtherGroups(t *testing.T) {
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: newSettlementFixture()})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	main := &moleFaultMain{main: NewMain("mole", "settlement", testMoleCheckpoint(t), nil)}
	address, _ := startMoleTestNode(t, "mole", routes, main)
	routes["mole"] = address
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	_, err := moleRPC(t, routes, "mole", "attack-broken", attackJoinCommand, uint64(2), "login-2", "access-01", "agent", "membership")
	if !errors.Is(err, runtime.ErrServicePanicked) {
		t.Fatalf("fault did not panic group: %v", err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}
