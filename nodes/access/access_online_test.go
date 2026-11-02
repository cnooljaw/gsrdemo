package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// verifyAccessOnlineUnavailable 强停本用例的 Online，验证拒绝新绑定且已进入的玩家继续推送、命中和入账。
func verifyAccessOnlineUnavailable(t *testing.T, ctx context.Context, onlineAddress, gateAddress string,
	players []accessDemoPlayer, db *sql.DB, operationIDs *[]string, onlineProcess *os.Process) {
	t.Helper()
	var before int64
	var previousVersion uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", players[0].id).
		Scan(&before, &previousVersion); err != nil {
		t.Fatal(err)
	}
	bound, snapshot := accessLoginAndEnter(t, gateAddress, &players[0])
	if err := onlineProcess.Kill(); err != nil {
		t.Fatal(err)
	}
	waitAccessProcessListenerClosed(t, onlineAddress)
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
	accessWaitCommitted(t, bound, hit.HitId)
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
