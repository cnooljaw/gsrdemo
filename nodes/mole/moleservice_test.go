package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// delayedAttackFixture 只延迟第一次 Join，实际状态变化仍由真实 AttackService 完成。
type delayedAttackFixture struct {
	attack  *attackService
	delayed bool
}

var _ gsr.Service = (*delayedAttackFixture)(nil)

func (service *delayedAttackFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return service.attack.OnStart(ctx, self)
}

func (service *delayedAttackFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == attackJoinCommand && !service.delayed {
		service.delayed = true
		time.Sleep(650 * time.Millisecond)
	}
	return service.attack.Handle(self, command, args...)
}

func (service *delayedAttackFixture) OnStop(ctx context.Context) error {
	return service.attack.OnStop(ctx)
}

type seededMoleFixture struct{ checkpoint moleCheckpoint }

var _ gsr.Service = (*seededMoleFixture)(nil)

func (service *seededMoleFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	root := newMoleService()
	slow, other := newAttackService("attack-a-slow"), newAttackService("attack-b-other")
	for userID := uint64(2); userID <= 4; userID++ {
		attack, seat := slow, int64(userID-2)
		if userID == 4 {
			attack, seat = other, 0
		}
		attack.members[userID] = attackMember{UserID: userID, LoginID: "seeded", AccessNode: "access-01", AgentName: "agent", Seat: seat, Membership: "seeded"}
		root.routes[userID] = moleRoute{Attack: attack.game.name, LoginID: "seeded", Membership: "seeded"}
		root.groups[attack.game.name]++
	}
	for _, child := range []gsr.Service{&delayedAttackFixture{attack: slow}, other} {
		if _, err := self.CreateService(ctx, child); err != nil {
			return err
		}
	}
	for _, child := range []struct {
		name    cluster.ServiceName
		service gsr.Service
	}{
		{moleAccountingName, newMoleAccountingService()},
		{moleSubmissionName, newMoleSubmissionService(service.checkpoint)},
		{moleServiceName, root},
	} {
		id, err := self.CreateService(ctx, child.service)
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(child.name, id); err != nil {
			return err
		}
	}
	if err := self.Node().OpenCluster("mole"); err != nil {
		return err
	}
	self.Exit()
	return nil
}

func (service *seededMoleFixture) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *seededMoleFixture) OnStop(context.Context) error {
	return nil
}

// TestMoleJoinTimeoutKeepsOneReservedRoute 保护 Join 超时后仍保留座位目录，其他用户去别组，重试不重复分组。
func TestMoleJoinTimeoutKeepsOneReservedRoute(t *testing.T) {
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: newSettlementFixture()})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	address, _ := startMoleTestNode(t, "mole", routes, &seededMoleFixture{checkpoint: testMoleCheckpoint(t)})
	routes["mole"] = address
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := moleRPC(t, routes, "mole", moleSubmissionName, submissionBalanceCommand, uint64(1))
		if err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("accounting not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := moleRPC(t, routes, "online", "online", 1, uint64(1), "login", "access-01", "agent"); err != nil {
		t.Fatal(err)
	}
	_, err := moleRPC(t, routes, "mole", moleServiceName, moleEnterCommand, uint64(1), "login", "access-01", "agent")
	if !errors.Is(err, runtime.ErrCallTimeout) {
		t.Fatalf("expected Join timeout: %v", err)
	}
	fifth := enterMoleTestUser(t, routes, 5, "login-5", "access-02")
	if fifth["attack_name"] != "attack-b-other" {
		t.Fatalf("unknown Join did not retain its reservation: %v", fifth["attack_name"])
	}
	retried := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if retried["attack_name"] != "attack-a-slow" || retried["seat"] != int64(2) {
		t.Fatalf("Join retry changed group or seat: %v", retried)
	}
}
