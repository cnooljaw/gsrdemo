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
	accessFixtureRPC(t, backend, 98, uint64(accessagent.MoleBalanceCorrectionPushCommand), "old-login", map[string]any{"balance": int64(1), "revision": uint64(99)})
	accessFixtureRPC(t, backend, 98, uint64(accessagent.MoleBalanceCorrectionPushCommand), location["login_id"], map[string]any{"balance": int64(900), "revision": uint64(3)})
	response := readAccessEnvelope(t, client)
	var push demopb.BalancePush
	if response.RequestId != 0 || response.MsgId != uint32(demopb.MsgId_S2C_BALANCE_PUSH) {
		t.Fatal(response)
	}
	if err := proto.Unmarshal(response.Payload, &push); err != nil || push.GetSilver().GetBalance() != 900 {
		t.Fatalf("push=%v err=%v", &push, err)
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

// TestAccessRejectsLegacyOperationRequests 保护两个停用查账编号在真实 C/S 入口明确返回 unsupported，不能被已登录状态重新开放。
func TestAccessRejectsLegacyOperationRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		id      demopb.MsgId
		request proto.Message
	}{
		{"mole", demopb.MsgId_C2S_OPERATION, &demopb.OperationRequest{HitId: "old-hit"}},
		{"landlord", demopb.MsgId_C2S_LANDLORD_OPERATION, &demopb.LandlordOperationRequest{OperationId: "old-round"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, _, _ := startAccessBusiness(t)
			client := dialAccess(t, address)
			accessLoginResponse(t, client, "alice", "password", &demopb.LoginResponse{})
			accessProcessResponse(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}, &demopb.EnterResponse{})
			response := accessRequest(t, client, test.id, test.request)
			var failure commonv1.ErrorResponse
			if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || proto.Unmarshal(response.Payload, &failure) != nil || failure.Code != commonv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
				t.Fatalf("停用请求未明确拒绝: %v %v", response, &failure)
			}
		})
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
