package main

import (
	"testing"
	"time"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

// TestMoleMatchingSettlementDoesNotRepeatReward 保护内存结果一致时不补发逐锤奖励，已消耗目标也不恢复。
func TestMoleMatchingSettlementDoesNotRepeatReward(t *testing.T) {
	fixture := newSettlementFixture()
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
	agent := newMoleAgentFixture()
	access, _ := startMoleTestNode(t, "access-01", nil, &moleTestPeer{node: "access-01", name: "agent", service: agent})
	routes := map[cluster.NodeID]string{"settlement": settlement, "online": online, "access-01": access}
	address, _ := startMoleTestNode(t, "mole", routes, newMoleTestMain(t, fixture))
	routes["mole"] = address
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	for len(agent.messages) > 0 {
		message := <-agent.messages
		if message.Command != accessagent.MoleSnapshotPushCommand {
			t.Fatalf("matching settlement emitted another result: %+v", message)
		}
	}
	current, err := moleRPC(t, routes, "mole", snapshot["attack_name"].(string), attackSnapshotCommand, uint64(1), "login")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, value := range current.(map[string]any)["hits"].([]any) {
		record := value.(map[string]any)
		if record["hit_id"] == hit["hit_id"] {
			found = true
			if record["status"] != "accepted" {
				t.Fatal("database completion changed game result")
			}
		}
	}
	if !found {
		t.Fatal("memory hit was lost")
	}
}

// TestMoleCorrectionUsesCurrentLogin 保护结算不一致时校正当前账号余额，不作为旧命中回执推送或复活目标。
func TestMoleCorrectionUsesCurrentLogin(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.hideReceipts = true
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
	agent := newMoleAgentFixture()
	access, _ := startMoleTestNode(t, "access-02", nil, &moleTestPeer{node: "access-02", name: "agent", service: agent})
	routes := map[cluster.NodeID]string{"settlement": settlement, "online": online, "access-02": access}
	address, _ := startMoleTestNode(t, "mole", routes, newMoleTestMain(t, fixture))
	routes["mole"] = address
	snapshot := enterMoleTestUser(t, routes, 1, "old-login", "access-01")
	if _, err := moleRPC(t, routes, "settlement", "settlement", 998, uint64(1), int64(0)); err != nil {
		t.Fatal(err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "old-login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("hit was not submitted")
	}
	_ = enterMoleTestUser(t, routes, 1, "new-login", "access-02")
	if _, err := moleRPC(t, routes, "settlement", "settlement", 997, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(moleTestWait)
	for {
		select {
		case message := <-agent.messages:
			if message.Command == accessagent.MoleSnapshotPushCommand {
				continue
			}
			if message.Command != accessagent.MoleBalanceCorrectionPushCommand || message.LoginID != "new-login" || message.Payload["balance"] != int64(0) {
				t.Fatalf("invalid account correction: %+v", message)
			}
			current, err := moleRPC(t, routes, "mole", snapshot["attack_name"].(string), attackSnapshotCommand, uint64(1), "new-login")
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range current.(map[string]any)["hits"].([]any) {
				record := v.(map[string]any)
				if record["hit_id"] == hit["hit_id"] && record["status"] != "accepted" {
					t.Fatal("financial correction changed hit")
				}
			}
			return
		case <-deadline:
			t.Fatal("balance was not corrected")
		}
	}
}

// TestMoleSettlementMayRejectCachedBalance 保护缓存预检查通过后，其他业务改变余额仍由结算最终拒绝。
func TestMoleSettlementMayRejectCachedBalance(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if _, err := moleRPC(t, routes, "settlement", "settlement", 998, uint64(1), int64(0)); err != nil {
		t.Fatal(err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "rejected")
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
	if err != nil || value.(map[string]any)["confirmed_balance"] != int64(0) || value.(map[string]any)["available"] != int64(0) {
		t.Fatalf("cache not reconciled: %v %v", value, err)
	}
}

// TestMoleReconnectKeepsProjectionWhileReceiptMissing 保护远端已入账但回执缺失时，重连使用本地视图且不再次查询覆盖余额。
func TestMoleReconnectKeepsProjectionWhileReceiptMissing(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.hideReceipts = true
	routes, _ := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "old-login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "old-login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("batch was not received by settlement")
	}
	reconnected := enterMoleTestUser(t, routes, 1, "new-login", "access-02")
	silver := reconnected["silver"].(map[string]any)
	if silver["balance"] != hit["balance"] || silver["confirmed_balance"] != int64(10000) ||
		silver["available"] != int64(9900) || len(fixture.balanceRequests) != 1 {
		t.Fatalf("reconnect changed pending projection or queried balance again: %v", silver)
	}
	if _, err := moleRPC(t, routes, "settlement", "settlement", 997, false); err != nil {
		t.Fatal(err)
	}
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
	if err != nil {
		t.Fatal(err)
	}
	silver = value.(map[string]any)
	if silver["balance"] != hit["balance"] || silver["confirmed_balance"] != hit["balance"] || silver["available"] != hit["balance"] {
		t.Fatalf("receipt did not reconcile pending projection: %v", silver)
	}
}
