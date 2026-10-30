package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// TestAccessTwoNodesReconnectLiveMySQL 保护双接入的并发游戏、跨节点替换/重连、推送路由与原 HitID 查询不重复扣款。
func TestAccessTwoNodesReconnectLiveMySQL(t *testing.T) {
	runAccessClientsLiveMySQL(t, 3, 2)
}

func accessProcessResponse(t *testing.T, client *accessClient, id demopb.MsgId, request, response proto.Message) {
	t.Helper()
	envelope := accessRequest(t, client, id, request)
	if envelope.MsgId != uint32(id+1) {
		t.Fatalf("request %v returned %v", id, envelope)
	}
	if err := proto.Unmarshal(envelope.Payload, response); err != nil {
		t.Fatal(err)
	}
}

func accessOnlineRPC(t *testing.T, address string, command runtime.CommandID, args ...any) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var result any
	main := &accessFixtureCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		var err error
		result, err = self.Cluster().Call(ctx, "online", "online", command, args...)
		return err
	}}
	err := gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: t.TempDir()},
		Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"online": address}},
	}, main)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func accessLoginAndEnter(t *testing.T, address string, player *accessDemoPlayer) (*accessClient, *demopb.Snapshot) {
	t.Helper()
	client := dialAccess(t, address)
	var login demopb.LoginResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_LOGIN,
		&demopb.LoginRequest{Username: player.username, Password: player.password}, &login)
	if login.UserId != player.id {
		t.Fatalf("跨接入身份变化: got=%d want=%d", login.UserId, player.id)
	}
	var enter demopb.EnterResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}, &enter)
	if enter.GetSnapshot().GetAttackName() == "" || enter.Silver == nil {
		t.Fatalf("重连缺少快照或初始余额: %v", &enter)
	}
	return client, enter.Snapshot
}

func accessNextSnapshot(t *testing.T, client *accessClient, attackName string) *demopb.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		envelope := readAccessEnvelope(t, client)
		if envelope.MsgId != uint32(demopb.MsgId_S2C_SNAPSHOT_PUSH) {
			continue
		}
		var push demopb.SnapshotPush
		if err := proto.Unmarshal(envelope.Payload, &push); err != nil {
			t.Fatal(err)
		}
		if push.GetSnapshot().GetAttackName() != attackName {
			t.Fatalf("收到其它组推送: %v", &push)
		}
		return push.Snapshot
	}
	t.Fatal("新接入未收到游戏推送")
	return nil
}

func accessWaitCommitted(t *testing.T, client *accessClient, hitID string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var operation demopb.OperationResponse
		accessProcessResponse(t, client, demopb.MsgId_C2S_OPERATION, &demopb.OperationRequest{HitId: hitID}, &operation)
		if operation.HitId != hitID {
			t.Fatalf("查询返回其它 HitID: %v", &operation)
		}
		if operation.Found {
			if operation.Status != "committed" {
				t.Fatalf("命中未入账: %v", &operation)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("原 HitID 未在期限内入账")
}

func verifyAccessReconnect(t *testing.T, ctx context.Context, routes map[string]string, gates []string,
	player *accessDemoPlayer, db *sql.DB, operationIDs *[]string) {
	t.Helper()
	var initial int64
	if err := db.QueryRowContext(ctx, "SELECT amount FROM silver_balances WHERE user_id = ?", player.id).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	old, snapshot := accessLoginAndEnter(t, gates[0], player)
	previous := accessOnlineRPC(t, routes["online"], 2, player.id).(map[string]any)
	if previous["access_node"] != "access-01" {
		t.Fatalf("旧接入路由错误: %v", previous)
	}
	// 当前只有本账号在线，等待一个未过期目标，再仅提交一次命中。
	var target *demopb.Target
	deadline := time.Now().Add(5 * time.Second)
	for target == nil && time.Now().Before(deadline) {
		for _, candidate := range snapshot.Targets {
			if !candidate.Claimed && candidate.DeadlineMs > time.Now().Add(200*time.Millisecond).UnixMilli() {
				target = candidate
				break
			}
		}
		if target == nil {
			snapshot = accessNextSnapshot(t, old, snapshot.AttackName)
		}
	}
	if target == nil {
		t.Fatal("没有可命中的目标")
	}
	var hit demopb.HitResponse
	accessProcessResponse(t, old, demopb.MsgId_C2S_HIT,
		&demopb.HitRequest{ActionId: "cross-access-hit", SpawnSeq: target.SpawnSeq, Hole: target.Hole}, &hit)
	if hit.GetHit().GetHitId() == "" || hit.Hit.Status != "accepted" {
		t.Fatalf("命中未被接纳: %v", &hit)
	}
	*operationIDs = append(*operationIDs, hit.Hit.HitId)

	current, snapshot := accessLoginAndEnter(t, gates[1], player)
	_ = old.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := old.ReadMessage(); err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				t.Fatal("跨接入替换后旧连接未关闭")
			}
			break
		}
	}
	location := accessOnlineRPC(t, routes["online"], 2, player.id).(map[string]any)
	if location["access_node"] != "access-02" || location["login_id"] == previous["login_id"] {
		t.Fatalf("新接入绑定错误: %v", location)
	}
	// 主动注入旧 LoginID 的迟到断线，验证真实 Online 不会清理新绑定。
	if removed := accessOnlineRPC(t, routes["online"], 3, player.id, previous["login_id"]); removed != false {
		t.Fatalf("旧 LoginID 清理了新绑定: %v", removed)
	}
	accessNextSnapshot(t, current, snapshot.AttackName)
	accessWaitCommitted(t, current, hit.Hit.HitId)
	accessWaitCommitted(t, current, hit.Hit.HitId)

	// 客户端直接断开，再从第一接入登录；不增加自动重连或未知命中重试逻辑。
	_ = current.Close()
	reconnected, snapshot := accessLoginAndEnter(t, gates[0], player)
	if removed := accessOnlineRPC(t, routes["online"], 3, player.id, location["login_id"]); removed != false {
		t.Fatalf("断开连接的迟到清理删除了重连身份: %v", removed)
	}
	rebuilt := accessOnlineRPC(t, routes["online"], 2, player.id).(map[string]any)
	if rebuilt["access_node"] != "access-01" || rebuilt["login_id"] == location["login_id"] {
		t.Fatalf("重连绑定错误: %v", rebuilt)
	}
	accessNextSnapshot(t, reconnected, snapshot.AttackName)
	accessWaitCommitted(t, reconnected, hit.Hit.HitId)
	var amount int64
	var version uint64
	if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).Scan(&amount, &version); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", hit.Hit.HitId).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != 5 || amount != initial-hit.Hit.Debit+hit.Hit.Credit {
		t.Fatalf("跨接入查询重复扣款或丢账: count=%d amount=%d version=%d hit=%v", count, amount, version, hit.Hit)
	}
	var leave demopb.LeaveResponse
	accessProcessResponse(t, reconnected, demopb.MsgId_C2S_LEAVE, &demopb.LeaveRequest{}, &leave)
	if !leave.Left {
		t.Fatal("重连后无法正常离开")
	}
	t.Logf("同账号 access-01 → access-02 → access-01：旧连接关闭、迟到解绑被拒、新接入收到推送；原 HitID 三次查询仅一笔入账，余额 %d、版本 %d", amount, version)
}
