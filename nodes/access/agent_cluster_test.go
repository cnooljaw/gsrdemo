package main

import (
	"fmt"
	"testing"
	"time"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// TestAccessMoleRequestsUseCurrentIdentityAndPush 保护命中转发保留当前身份和 ActionID，拒绝结果不伪造余额，推送过滤旧登录。
func TestAccessMoleRequestsUseCurrentIdentityAndPush(t *testing.T) {
	gateAddress, backend, hits := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if response := accessLogin(t, client, "alice", "password"); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(response)
	}
	location := accessFixtureRPC(t, backend, 99).(map[string]any)
	if response := accessRequest(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatal(response)
	}
	for range 2 {
		response := accessRequest(t, client, demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: "same-action", SpawnSeq: 9, Hole: 2})
		if response.MsgId != uint32(demopb.MsgId_S2C_HIT) {
			t.Fatal(response)
		}
		args := <-hits
		if args[0] != uint64(7) || args[1] != location["login_id"] || args[2] != "same-action" || args[3] != uint64(9) || args[4] != int64(2) {
			t.Fatalf("hit args=%v", args)
		}
	}
	rejected := accessRequest(t, client, demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: "rejected-action", SpawnSeq: 9, Hole: 2})
	var rejectedHit demopb.HitResponse
	if err := proto.Unmarshal(rejected.Payload, &rejectedHit); err != nil || rejected.MsgId != uint32(demopb.MsgId_S2C_HIT) || rejectedHit.GetHit().GetStatus() != "rejected" || rejectedHit.Silver != nil {
		t.Fatalf("拒绝重试不能伪造余额: %v %v", &rejectedHit, err)
	}
	accessFixtureRPC(t, backend, 98, uint64(accessagent.MoleBalanceCorrectionPushCommand), "old-login", map[string]any{"balance": int64(1), "available": int64(1), "revision": uint64(99)})
	accessFixtureRPC(t, backend, 98, uint64(accessagent.MoleBalanceCorrectionPushCommand), location["login_id"], map[string]any{"balance": int64(900), "available": int64(900), "revision": uint64(3)})
	response := readAccessEnvelope(t, client)
	var push demopb.BalancePush
	if response.RequestId != 0 || response.MsgId != uint32(demopb.MsgId_S2C_BALANCE_PUSH) {
		t.Fatal(response)
	}
	if err := proto.Unmarshal(response.Payload, &push); err != nil || push.GetSilver().GetBalance() != 900 {
		t.Fatalf("push=%v err=%v", &push, err)
	}
}

// TestAccessLandlordSnapshotResponseBeforePush 保护等待牌桌查询时发出的推送不会先于该次响应。
func TestAccessLandlordSnapshotResponseBeforePush(t *testing.T) {
	barrier := newAccessEntryBarrier()
	gateAddress, backend, _ := startAccessBusinessFixture(t, "landlord-snapshot-order", barrier)
	t.Cleanup(barrier.release)
	client := dialAccess(t, gateAddress)
	if response := accessLogin(t, client, "alice", "password"); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(response)
	}
	location := accessFixtureRPC(t, backend, 99).(map[string]any)
	if response := accessRequest(t, client, demopb.MsgId_C2S_LANDLORD_ENTER, &demopb.LandlordEnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_LANDLORD_ENTER) {
		t.Fatal(response)
	}
	payload, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(demopb.MsgId_C2S_LANDLORD_SNAPSHOT), RequestId: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(payload)); err != nil {
		t.Fatal(err)
	}
	barrier.wait(t)
	accessFixtureRPC(t, backend, 98, uint64(accessagent.LandlordPlayPushCommand), location["login_id"], map[string]any{"room_name": "room", "revision": uint64(2), "user_id": uint64(7), "turn": uint64(2)})
	barrier.release()
	response, push := readAccessEnvelope(t, client), readAccessEnvelope(t, client)
	if response.RequestId != 2 || response.MsgId != uint32(demopb.MsgId_S2C_LANDLORD_SNAPSHOT) || push.RequestId != 0 || push.MsgId != uint32(demopb.MsgId_S2C_LANDLORD_PLAY_PUSH) {
		t.Fatalf("消息顺序错误: response=%v push=%v", response, push)
	}
	var old demopb.LandlordSnapshotResponse
	var newer demopb.LandlordPlayPush
	if err := proto.Unmarshal(response.Payload, &old); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(push.Payload, &newer); err != nil {
		t.Fatal(err)
	}
	if old.GetSnapshot().GetTurn() != 1 || newer.GetTurn() != 2 {
		t.Fatalf("快照轮次错误: response=%v push=%v", &old, &newer)
	}
}

// TestAccessReplacedConnectionCannotUnbindNewLogin 保护重复登录后旧 Agent 退出及迟到断线不会清理新 LoginID。
func TestAccessReplacedConnectionCannotUnbindNewLogin(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	old := dialAccess(t, gateAddress)
	first := accessLogin(t, old, "alice", "password")
	if first.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(first)
	}
	previous := accessFixtureRPC(t, backend, 99).(map[string]any)
	current := dialAccess(t, gateAddress)
	second := accessLogin(t, current, "alice", "password")
	if second.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(second)
	}
	_ = old.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := old.ReadMessage(); err == nil {
		t.Fatal("旧连接未关闭")
	}
	now := accessFixtureRPC(t, backend, 99).(map[string]any)
	if now["login_id"] == previous["login_id"] || now["access_node"] != "access-01" {
		t.Fatal(fmt.Sprint(now))
	}
	_ = old.Close()
	if response := accessRequest(t, current, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatal(response)
	}
}

// TestAccessOnlineFailureDoesNotOpenGame 保护认证成功但 Online 绑定失败时关闭连接，不留下已认证可游戏的 Agent。
func TestAccessOnlineFailureDoesNotOpenGame(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	accessFixtureRPC(t, backend, 97, true)
	client := dialAccess(t, gateAddress)
	payload, err := proto.Marshal(&demopb.LoginRequest{AccessToken: accessAuthenticate(t, client, "alice", "password").AccessToken})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(demopb.MsgId_C2S_LOGIN), RequestId: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(frame)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, contents, readErr := client.ReadMessage()
	if readErr != nil {
		t.Fatalf("Online 绑定失败应先返回错误: %v", readErr)
	}
	var envelope commonv1.Envelope
	if err := proto.Unmarshal(contents, &envelope); err != nil || envelope.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatalf("Online 失败不应返回登录成功: %v %v", &envelope, err)
	}
	accessExpectClosed(t, client)
	if current := accessFixtureRPC(t, backend, 99); current != nil {
		t.Fatal(current)
	}
	accessFixtureRPC(t, backend, 97, false)
	healthy := dialAccess(t, gateAddress)
	if response := accessLogin(t, healthy, "alice", "password"); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal("恢复后新连接不能登录", response)
	}
}

// TestAccessOnlyKnownTargetRefusalCanContinue 保护只有明确的目标失效才返回可继续响应，超时、繁忙和错误类别不被当成未扣款。
func TestAccessOnlyKnownTargetRefusalCanContinue(t *testing.T) {
	gateAddress, _, hits := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if response := accessLogin(t, client, "alice", "password"); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(response)
	}
	if response := accessRequest(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatal(response)
	}
	for _, action := range []string{"target-gone", "timeout", "busy", "wrong-class"} {
		response := accessRequest(t, client, demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: action, SpawnSeq: 9, Hole: 2})
		<-hits
		if action != "target-gone" {
			if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
				t.Fatalf("%s 不能变为明确拒绝: %v", action, response)
			}
			continue
		}
		var hit demopb.HitResponse
		if err := proto.Unmarshal(response.Payload, &hit); err != nil || response.MsgId != uint32(demopb.MsgId_S2C_HIT) || hit.Rejection != "target_unavailable" || hit.Hit != nil || hit.Silver != nil {
			t.Fatalf("目标失效不应有奖励或余额: %v %v", &hit, err)
		}
	}
}

// TestAccessDhmjReconnectAndLeave 保护真实 WebSocket 麻将接线、互斥，以及旧连接断开时按旧登录清理。
func TestAccessDhmjReconnectAndLeave(t *testing.T) {
	gateAddress, backend, calls := startAccessBusiness(t)
	old := dialAccess(t, gateAddress)
	if got := accessLogin(t, old, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	oldLogin := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
	if got := accessRequest(t, old, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) {
		t.Fatal(got)
	}
	if got := accessRequest(t, old, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); got.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatalf("麻将入桌后仍能进入地鼠：%v", got)
	}
	current := dialAccess(t, gateAddress)
	if got := accessLogin(t, current, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	response := accessRequest(t, current, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{})
	var joined demopb.DhmjEnterResponse
	if err := proto.Unmarshal(response.Payload, &joined); err != nil || response.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) ||
		joined.GetSnapshot().GetTableName() != "dhmj-1" || len(joined.GetSnapshot().GetHand()) != 2 {
		t.Fatalf("重登未恢复本人快照：%v %v", response, err)
	}
	select {
	case call := <-calls:
		if call[0] != "leave-dhmj" || call[1] != uint64(7) || call[2] != oldLogin {
			t.Fatalf("清理未使用旧登录：%v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("旧连接未离开麻将")
	}
	if got := accessRequest(t, current, demopb.MsgId_C2S_DHMJ_SNAPSHOT, &demopb.DhmjSnapshotRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_SNAPSHOT) {
		t.Fatalf("旧连接清理影响了当前快照：%v", got)
	}
	if got := accessRequest(t, current, demopb.MsgId_C2S_DHMJ_LEAVE, &demopb.DhmjLeaveRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_ACTION_ACK) {
		t.Fatal(got)
	}
	if got := accessRequest(t, current, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatalf("麻将离桌后互斥占位仍存在：%v", got)
	}
}
