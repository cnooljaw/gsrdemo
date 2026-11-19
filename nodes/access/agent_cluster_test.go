package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	dhmjrpc "github.com/cnooljaw/gsrdemo/common/rpc/dhmj"
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

// TestAccessDhmjActionRequests 保护六种客户端动作的 RPC 类型与参数顺序、通用成功回包和麻将规则错误。
func TestAccessDhmjActionRequests(t *testing.T) {
	gateAddress, backend, calls := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if got := accessLogin(t, client, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_DISCARD, &demopb.DhmjDiscardRequest{Turn: 1, Tile: 5}); got.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatalf("未入桌的连接提交了麻将动作：%v", got)
	}
	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) {
		t.Fatal(got)
	}

	const turn uint64 = 1<<54 + 1
	const windowID uint64 = 1<<55 + 3
	for _, action := range []struct {
		id      demopb.MsgId
		request proto.Message
		command runtime.CommandID
		args    []any
	}{
		{demopb.MsgId_C2S_DHMJ_DISCARD, &demopb.DhmjDiscardRequest{Turn: turn, Tile: 5}, dhmjrpc.DiscardCommand, []any{turn, int64(5)}},
		{demopb.MsgId_C2S_DHMJ_RESPOND, &demopb.DhmjRespondRequest{WindowId: windowID, Action: 6}, dhmjrpc.RespondCommand, []any{windowID, int64(6)}},
		{demopb.MsgId_C2S_DHMJ_SELF_WIN, &demopb.DhmjSelfWinRequest{Turn: turn}, dhmjrpc.SelfWinCommand, []any{turn}},
		{demopb.MsgId_C2S_DHMJ_KONG, &demopb.DhmjKongRequest{Turn: turn, Tile: 7}, dhmjrpc.KongCommand, []any{turn, int64(7)}},
		{demopb.MsgId_C2S_DHMJ_PASS_SELF, &demopb.DhmjPassSelfRequest{Turn: turn}, dhmjrpc.PassSelfCommand, []any{turn}},
		{demopb.MsgId_C2S_DHMJ_TRUSTEE, &demopb.DhmjTrusteeRequest{Enabled: true}, dhmjrpc.TrusteeCommand, []any{true}},
	} {
		got := accessRequest(t, client, action.id, action.request)
		if got.MsgId != uint32(demopb.MsgId_S2C_ACTION_ACK) || got.RequestId != 1 || len(got.Payload) != 0 {
			t.Fatalf("%s 未返回空确认：%v", action.id, got)
		}
		want := append([]any{action.command, uint64(7), login}, action.args...)
		select {
		case call := <-calls:
			if !reflect.DeepEqual(call, want) {
				t.Fatalf("%s 的 RPC 参数：%#v，预期 %#v", action.id, call, want)
			}
		default:
			t.Fatalf("%s 未走麻将 RPC", action.id)
		}
	}

	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_DISCARD, &demopb.DhmjDiscardRequest{Tile: 5}); got.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || got.RequestId != 1 {
		t.Fatalf("麻将拒绝旧轮次后仍返回成功：%v", got)
	}
	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_SNAPSHOT, &demopb.DhmjSnapshotRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_SNAPSHOT) {
		t.Fatalf("业务拒绝后无法恢复牌桌：%v", got)
	}
}

// TestAccessDhmjSettlementPushFiltersRecipient 保护本人结算经过真实 Cluster 和 WebSocket，只向当前桌的当前登录推送。
func TestAccessDhmjSettlementPushFiltersRecipient(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if got := accessLogin(t, client, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) {
		t.Fatal(got)
	}

	const balance int64 = 1<<54 + 30
	const version uint64 = 1<<55 + 2
	push := func(target any, table string, uid uint64, operationID string, fees any) {
		t.Helper()
		accessFixtureRPC(t, backend, 98, uint64(dhmjSettlementPushCommand), target, map[string]any{
			"table_name": table,
			"settlement": map[string]any{
				"operation_id": operationID, "status": "committed", "user_id": uid,
				"fee": fees, "paid": int64(10), "received": int64(0), "balance": balance, "version": version,
			},
		})
	}
	push("old-login", "dhmj-1", 7, "old-login", int64(50))
	push(login, "another-table", 7, "old-table", int64(50))
	push(login, "dhmj-1", 8, "another-user", int64(50))
	push(login, "dhmj-1", 7, "bad-fee", nil)
	push(login, "dhmj-1", 7, "round-1", int64(50))

	response := readAccessEnvelope(t, client)
	var body demopb.DhmjSettlementPush
	if err := proto.Unmarshal(response.Payload, &body); err != nil || response.MsgId != uint32(demopb.MsgId_S2C_DHMJ_SETTLEMENT_PUSH) || response.RequestId != 0 {
		t.Fatalf("没有收到麻将结算推送：%v %v", response, err)
	}
	silver := body.GetSettlement()
	if body.GetTableName() != "dhmj-1" || silver.GetOperationId() != "round-1" || silver.GetUserId() != 7 ||
		silver.GetFee() != 50 || silver.GetPaid() != 10 || silver.GetBalance() != balance || silver.GetVersion() != version {
		t.Fatalf("收到错误的麻将回执：%v", &body)
	}
}

// TestAccessDhmjDiscardPush 保护出牌事件仅转给当前桌和当前登录，且包含快照恢复所需的事件序号。
func TestAccessDhmjDiscardPush(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if got := accessLogin(t, client, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
	response := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{})
	var entry demopb.DhmjEnterResponse
	if err := proto.Unmarshal(response.Payload, &entry); err != nil || response.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) ||
		entry.GetSnapshot().GetRevision() != 8 {
		t.Fatalf("进桌快照缺少事件序号：%v %v", response, err)
	}

	push := func(target any, table string, revision uint64, seat int64) {
		t.Helper()
		accessFixtureRPC(t, backend, 98, uint64(accessagent.DhmjDiscardPushCommand), target, map[string]any{
			"table_name": table, "revision": revision, "seat": seat, "turn": uint64(7), "tile": int64(0x25),
		})
	}
	push("old-login", "dhmj-1", 9, 0)
	push(login, "another-table", 9, 1)
	push(login, "dhmj-1", 0, 2)
	push(login, "dhmj-1", 9, 4)
	push(login, "dhmj-1", 9, 3)

	response = readAccessEnvelope(t, client)
	var discard demopb.DhmjDiscardPush
	if err := proto.Unmarshal(response.Payload, &discard); err != nil || response.MsgId != uint32(demopb.MsgId_S2C_DHMJ_DISCARD_PUSH) || response.RequestId != 0 {
		t.Fatalf("未收到麻将出牌推送：%v %v", response, err)
	}
	if discard.GetTableName() != "dhmj-1" || discard.GetRevision() != 9 || discard.GetSeat() != 3 ||
		discard.GetTurn() != 7 || discard.GetTile() != 0x25 {
		t.Fatalf("出牌推送错误：%v", &discard)
	}
}

// TestAccessDhmjTilePushHidesOtherSeats 保护摸牌和补花的公开信息仍可送达，其他座位的暗牌在 Access 被剔除。
func TestAccessDhmjTilePushHidesOtherSeats(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if got := accessLogin(t, client, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) {
		t.Fatal(got)
	}
	for i, seat := range []int64{2, 0} {
		revision := uint64(9 + i*2)
		accessFixtureRPC(t, backend, 98, uint64(accessagent.DhmjFlowerPushCommand), login, map[string]any{
			"table_name": "dhmj-1", "revision": revision, "seat": seat, "remaining": int64(13), "from_draw": true,
			"flowers": []any{int64(0x41), int64(0x42)}, "replacements": []any{int64(0x42), int64(5)},
		})
		got := readAccessEnvelope(t, client)
		var flower demopb.DhmjFlowerPush
		if err := proto.Unmarshal(got.Payload, &flower); err != nil || got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_FLOWER_PUSH) || got.RequestId != 0 ||
			flower.GetSeat() != seat || flower.GetRevision() != revision || !flower.GetFromDraw() || flower.GetRemaining() != 13 ||
			!reflect.DeepEqual(flower.GetFlowers(), []int64{0x41, 0x42}) {
			t.Fatalf("补花公开信息错误：%v %v", got, err)
		}
		var wantReplacements []int64
		if seat == 0 {
			wantReplacements = []int64{0x42, 5}
		}
		if !reflect.DeepEqual(flower.GetReplacements(), wantReplacements) {
			t.Fatalf("补花替换牌可见性错误：%v", &flower)
		}

		accessFixtureRPC(t, backend, 98, uint64(accessagent.DhmjDrawPushCommand), login, map[string]any{
			"table_name": "dhmj-1", "revision": revision + 1, "seat": seat, "remaining": int64(13), "turn": uint64(7), "tile": int64(5),
		})
		got = readAccessEnvelope(t, client)
		var draw demopb.DhmjDrawPush
		if err := proto.Unmarshal(got.Payload, &draw); err != nil || got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_DRAW_PUSH) || got.RequestId != 0 ||
			draw.GetSeat() != seat || draw.GetRevision() != revision+1 || draw.GetTurn() != 7 || draw.GetRemaining() != 13 {
			t.Fatalf("摸牌公开信息错误：%v %v", got, err)
		}
		if (seat == 0 && draw.GetTile() != 5) || (seat != 0 && draw.Tile != nil) {
			t.Fatalf("摸到的暗牌可见性错误：%v", &draw)
		}
	}
}

// TestAccessDhmjDealPush 保护开局发牌只转给当前桌的本人，拒绝错座、错用户或不完整的起手牌。
func TestAccessDhmjDealPush(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if got := accessLogin(t, client, "alice", "password"); got.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(got)
	}
	login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
	if got := accessRequest(t, client, demopb.MsgId_C2S_DHMJ_ENTER, &demopb.DhmjEnterRequest{}); got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_ENTER) {
		t.Fatal(got)
	}

	ids := []any{uint64(7), uint64(8), uint64(9), uint64(10)}
	hand := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 0x11, 0x12, 0x13, 0x41}
	push := func(table string, seat int64, ids []any, tiles []int64) {
		t.Helper()
		cards := make([]any, len(tiles))
		for i, tile := range tiles {
			cards[i] = tile
		}
		accessFixtureRPC(t, backend, 98, uint64(accessagent.DhmjDealPushCommand), login, map[string]any{
			"table_name": table, "revision": uint64(1), "seat": seat, "dealer_seat": int64(2),
			"remaining": int64(92), "player_ids": ids, "hand": cards,
		})
	}
	push("another-table", 0, ids, hand)
	push("dhmj-1", 1, ids, hand)
	push("dhmj-1", 0, []any{uint64(11), uint64(8), uint64(9), uint64(10)}, hand)
	push("dhmj-1", 0, ids, hand[:12])
	push("dhmj-1", 0, ids, hand)

	got := readAccessEnvelope(t, client)
	var deal demopb.DhmjDealPush
	if err := proto.Unmarshal(got.Payload, &deal); err != nil || got.MsgId != uint32(demopb.MsgId_S2C_DHMJ_DEAL_PUSH) || got.RequestId != 0 {
		t.Fatalf("未收到发牌推送：%v %v", got, err)
	}
	if deal.GetTableName() != "dhmj-1" || deal.GetRevision() != 1 || deal.GetSeat() != 0 ||
		deal.GetDealerSeat() != 2 || deal.GetRemaining() != 92 ||
		!reflect.DeepEqual(deal.GetPlayerIds(), []uint64{7, 8, 9, 10}) || !reflect.DeepEqual(deal.GetHand(), hand) {
		t.Fatalf("错误发牌信息：%v", &deal)
	}
}
