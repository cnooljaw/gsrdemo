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

// TestMoleEntryDoesNotExposeOperationQuery 保护旧 Entry 查询命令明确拒绝，内部结算查询不再从进入入口转发。
func TestMoleEntryDoesNotExposeOperationQuery(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	if _, err := moleRPC(t, routes, "mole", moleEntryName, 3, uint64(1), "old-hit"); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatalf("旧账务查询命令仍可调用: %v", err)
	}
}

// delayedAttackFixture 只延迟第一次 Join，实际状态变化仍由真实 AttackService 完成。
type delayedAttackFixture struct {
	attack  *attackService
	delayed bool
	started chan struct{}
}

var _ gsr.Service = (*delayedAttackFixture)(nil)

func (service *delayedAttackFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return service.attack.OnStart(ctx, self)
}

func (service *delayedAttackFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == attackJoinCommand && !service.delayed {
		service.delayed = true
		if service.started != nil {
			close(service.started)
		}
		time.Sleep(650 * time.Millisecond)
	}
	return service.attack.Handle(self, command, args...)
}

func (service *delayedAttackFixture) OnStop(ctx context.Context) error {
	return service.attack.OnStop(ctx)
}

type seededMoleFixture struct {
	checkpoint  moleCheckpoint
	slowStarted chan struct{}
}

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
	for _, child := range []gsr.Service{&delayedAttackFixture{attack: slow, started: service.slowStarted}, other} {
		if _, err := self.CreateService(ctx, child); err != nil {
			return err
		}
	}
	for _, child := range []struct {
		name    cluster.ServiceName
		service gsr.Service
	}{
		{moleAccountingName, newMoleAccountingService()},
		{moleServiceName, root},
		{moleEntryName, newMoleEntryService("settlement")},
		{moleSubmissionName, newMoleSubmissionService(service.checkpoint, "settlement", "mole")},
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

// TestMoleSlowJoinDoesNotBlockDirectoryOrRooms 保护单入口的慢 Join 有限等待，不占住目录和已进入房间。
func TestMoleSlowJoinDoesNotBlockDirectoryOrRooms(t *testing.T) {
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: newSettlementFixture()})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	started := make(chan struct{})
	address, _ := startMoleTestNode(t, "mole", routes, &seededMoleFixture{checkpoint: testMoleCheckpoint(t), slowStarted: started})
	routes["mole"] = address
	for _, userID := range []uint64{1, 5} {
		if _, err := moleRPC(t, routes, "online", "online", 1, userID, "login", "access-01", "agent"); err != nil {
			t.Fatal(err)
		}
	}
	// 等待内存账初始化；不通过待测的进入路径判断准备就绪。
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { _, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow Join did not start")
	}
	begin := time.Now()
	value, err := moleRPC(t, routes, "mole", moleServiceName, moleResolveEntryCommand, uint64(5), "login", "access-01", "agent")
	if err != nil || value.(map[string]any)["entry_name"] != moleEntryName {
		t.Fatalf("目录等待慢 Join: %v %v", value, err)
	}
	if _, err := moleRPC(t, routes, "mole", "attack-b-other", attackSnapshotCommand, uint64(4), "seeded"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 300*time.Millisecond {
		t.Errorf("目录或房间等待慢 Join: %v", elapsed)
	}
	if err := <-done; !errors.Is(err, runtime.ErrCallTimeout) {
		t.Fatalf("slow Join: %v", err)
	}
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
		_, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand)
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
	_, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent")
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

// TestMoleSharedGroupsAndReconnect 保护真实 TCP 下分组补位、跨接入重连、迟到进入和旧连接操作隔离。
func TestMoleSharedGroupsAndReconnect(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	first := enterMoleTestUser(t, routes, 1, "old", "access-01")
	second := enterMoleTestUser(t, routes, 2, "login-2", "access-02")
	third := enterMoleTestUser(t, routes, 3, "login-3", "access-01")
	fourth := enterMoleTestUser(t, routes, 4, "login-4", "access-02")
	name := first["attack_name"].(string)
	if second["attack_name"] != name || third["attack_name"] != name || fourth["attack_name"] == name {
		t.Fatal("three-seat grouping failed")
	}
	reconnected := enterMoleTestUser(t, routes, 1, "new", "access-02")
	if reconnected["attack_name"] != name || reconnected["seat"] != first["seat"] {
		t.Fatal("reconnect moved group or seat")
	}
	for _, command := range []runtime.CommandID{attackSnapshotCommand, attackLeaveCommand} {
		if _, err := moleRPC(t, routes, "mole", name, command, uint64(1), "old"); !errors.Is(err, errMoleStaleLogin) {
			t.Fatalf("old login command %d: %v", command, err)
		}
	}
	if _, err := callMoleEnter(t, routes, uint64(1), "old", "access-01", "agent"); !errors.Is(err, errMoleStaleLogin) {
		t.Fatalf("late Enter replaced new login: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", name, attackLeaveCommand, uint64(2), "login-2"); err != nil {
		t.Fatal(err)
	}
	joined := enterMoleTestUser(t, routes, 5, "login-5", "access-02")
	if joined["attack_name"] != name || joined["seat"] != second["seat"] {
		t.Fatal("vacant group or seat was not filled")
	}
}

// TestMoleOnlineFailureOnlyBlocksEntry 保护 Online 不可用时拒绝新进入，已进入用户仍可直接命中并结算。
func TestMoleOnlineFailureOnlyBlocksEntry(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if _, err := moleRPC(t, routes, "online", "online", 99); err != nil {
		t.Fatal(err)
	}
	if _, err := callMoleEnter(t, routes, uint64(2), "login-2", "access-02", "agent"); !errors.Is(err, errMoleOnlineUnavailable) {
		t.Fatalf("entry ignored Online failure: %v", err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}

// TestMoleUsesConfiguredSettlementNode 保护余额查询与批次提交都使用配置目标，节点表无需存在默认 settlement 名称。
func TestMoleUsesConfiguredSettlementNode(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.checkpoint = testMoleCheckpoint(t)
	address, _ := startMoleTestNode(t, "settlement-02", nil, &moleTestPeer{node: "settlement-02", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"settlement-02": address, "online": online}
	mole, _ := startMoleTestNode(t, "mole", routes, NewMain("mole", "settlement-02", fixture.checkpoint, nil))
	routes["mole"] = mole
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "configured-node")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}
