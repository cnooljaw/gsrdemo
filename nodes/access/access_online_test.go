package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// verifyAccessOnlineUnavailable 验证 Online 中断时游戏继续，重启后显式登录重建绑定且旧身份不能误删。
func verifyAccessOnlineUnavailable(t *testing.T, ctx context.Context, online accessNodeProcess, gateAddress string,
	players []accessDemoPlayer, db *sql.DB, operationIDs *[]string) {
	t.Helper()
	var before int64
	var previousVersion uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", players[0].id).
		Scan(&before, &previousVersion); err != nil {
		t.Fatal(err)
	}
	bound, snapshot := accessLoginAndEnter(t, gateAddress, &players[0])
	previous := accessOnlineRPC(t, online.address, 2, players[0].id).(map[string]any)
	if err := online.process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitAccessProcessListenerClosed(t, online.address)
	failedAt := time.Now().UnixMilli()
	accessExpectLoginRejected(t, gateAddress, &players[1])

	// 必须收到故障之后生成的快照，不能把队列中故障前的推送当成继续运行的证据。
	deadline := time.Now().Add(5 * time.Second)
	for snapshot.ServerMs <= failedAt && time.Now().Before(deadline) {
		snapshot = accessNextSnapshot(t, bound, snapshot.AttackName)
	}
	if snapshot.ServerMs <= failedAt {
		t.Fatal("Online 不可用后游戏未继续推送")
	}
	var fresh demopb.SnapshotResponse
	accessProcessResponse(t, bound, demopb.MsgId_C2S_SNAPSHOT, &demopb.SnapshotRequest{}, &fresh)
	hit := accessHitTarget(t, bound, fresh.Snapshot, "while-online-unavailable")
	*operationIDs = append(*operationIDs, hit.HitId)
	accessWaitCommitted(t, ctx, db, hit.HitId)
	var amount int64
	var version uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", players[0].id).
		Scan(&amount, &version); err != nil {
		t.Fatal(err)
	}
	if amount != before-hit.Debit+hit.Credit || version != previousVersion+1 {
		t.Fatalf("Online 不可用时已有玩家入账差异: amount=%d version=%d hit=%v", amount, version, hit)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", hit.HitId).Scan(&count); err != nil || count != 1 {
		t.Fatalf("Online 不可用时丢账或重复: count=%d err=%v", count, err)
	}
	var leave demopb.LeaveResponse
	accessProcessResponse(t, bound, demopb.MsgId_C2S_LEAVE, &demopb.LeaveRequest{}, &leave)
	if !leave.Left {
		t.Fatal("Online 不可用时已有玩家无法离开")
	}
	t.Logf("SIGKILL Online：新登录被拒且连接关闭；已有玩家沿原连接收到新推送、命中并入账，余额 %d，版本 %d → %d", amount, previousVersion, version)

	// Online 不持久化目录；只恢复本测试节点，再由客户端重新登录建立新身份。
	startAccessProcess(t, online.binary, []string{"-config", online.config}, online.address)
	if location := accessOnlineRPC(t, online.address, 2, players[0].id); location != nil {
		t.Fatalf("Online 重启不应保留旧目录: %v", location)
	}
	recovered, snapshot := accessLoginAndEnter(t, gateAddress, &players[0])
	location := accessOnlineRPC(t, online.address, 2, players[0].id).(map[string]any)
	if location["access_node"] != "access-02" || location["login_id"] == previous["login_id"] {
		t.Fatalf("Online 重启后未建立新绑定: %v", location)
	}
	if removed := accessOnlineRPC(t, online.address, 3, players[0].id, previous["login_id"]); removed != false {
		t.Fatal("Online 重启后旧 LoginID 删除了新绑定")
	}
	current := accessOnlineRPC(t, online.address, 2, players[0].id).(map[string]any)
	if current["login_id"] != location["login_id"] || current["agent_name"] != location["agent_name"] {
		t.Fatalf("迟到解绑改变了恢复绑定: %v", current)
	}
	_ = bound.Close()
	snapshot = accessNextSnapshot(t, recovered, snapshot.AttackName)
	recoveryHit := accessHitTarget(t, recovered, snapshot, "after-online-restart")
	*operationIDs = append(*operationIDs, recoveryHit.HitId)
	accessWaitCommitted(t, ctx, db, recoveryHit.HitId, hit.HitId)
	var recoveredAmount int64
	var recoveredVersion uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", players[0].id).
		Scan(&recoveredAmount, &recoveredVersion); err != nil ||
		recoveredAmount != amount-recoveryHit.Debit+recoveryHit.Credit || recoveredVersion != version+1 {
		t.Fatalf("Online 重启后入账差异: amount=%d version=%d err=%v", recoveredAmount, recoveredVersion, err)
	}
	for _, id := range []string{hit.HitId, recoveryHit.HitId} {
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", id).
			Scan(&count); err != nil || count != 1 {
			t.Fatalf("Online 重启后丢账或重复: count=%d err=%v", count, err)
		}
	}
	accessProcessResponse(t, recovered, demopb.MsgId_C2S_LEAVE, &demopb.LeaveRequest{}, &leave)
	if !leave.Left {
		t.Fatal("Online 重启后无法离开")
	}
	t.Logf("Online 重启清空目录，显式重登重建绑定，旧身份解绑无效；重新进入后命中并唯一入账，余额 %d，版本 %d → %d", recoveredAmount, version, recoveredVersion)
}

func accessExpectLoginRejected(t *testing.T, address string, player *accessDemoPlayer) {
	t.Helper()
	client := dialAccess(t, address)
	payload, err := proto.Marshal(&demopb.LoginRequest{Username: player.username, Password: player.password})
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
	_ = client.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, contents, err := client.ReadMessage()
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			t.Fatal("Online 不可用时登录未在期限内拒绝/关闭")
		}
		return
	}
	var response commonv1.Envelope
	if err := proto.Unmarshal(contents, &response); err != nil || response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatalf("Online 不可用不应返回登录成功: %v %v", &response, err)
	}
	accessExpectClosed(t, client)
}
