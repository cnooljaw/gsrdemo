package main

import (
	"errors"
	"testing"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

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
	for _, command := range []runtime.CommandID{moleSnapshotCommand, moleLeaveCommand} {
		args := []any{uint64(1), "old"}
		if command == moleSnapshotCommand {
			args = append(args, name)
		}
		if _, err := moleRPC(t, routes, "mole", moleServiceName, command, args...); !errors.Is(err, errMoleStaleLogin) {
			t.Fatalf("old login command %d: %v", command, err)
		}
	}
	if _, err := callMoleEnter(t, routes, uint64(1), "old", "access-01", "agent"); !errors.Is(err, errMoleStaleLogin) {
		t.Fatalf("late Enter replaced new login: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", moleServiceName, moleLeaveCommand, uint64(2), "login-2"); err != nil {
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
	if _, err := moleRPC(t, routes, "online", onlinerpc.ServiceName, 99); err != nil {
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
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"settlement-02": address, "online": online}
	mole, _ := startMoleTestNode(t, "mole", routes, NewMain("mole", "settlement-02", fixture.checkpoint, nil))
	routes["mole"] = mole
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "configured-node")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}
