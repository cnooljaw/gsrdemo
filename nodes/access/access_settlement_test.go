package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// verifyAccessSettlementRecovery 覆盖 Mole 存活、未入账批次恢复和已入账但未确认三种路径，不重发客户端命中。
func verifyAccessSettlementRecovery(t *testing.T, ctx context.Context, gateAddress string,
	player *accessDemoPlayer, db *sql.DB, operationIDs *[]string,
	settlement, mole accessNodeProcess, redisAddress string) {
	t.Helper()
	for _, scenario := range []string{"settlement-only", "pending-before-commit", "committed-before-receipt"} {
		restartMole := scenario != "settlement-only"
		client, snapshot := accessLoginAndEnter(t, gateAddress, player)
		var before int64
		var previousVersion uint64
		if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
			Scan(&before, &previousVersion); err != nil {
			t.Fatal(err)
		}
		if err := settlement.process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitAccessProcessListenerClosed(t, settlement.address)
		hit := accessHitTarget(t, client, snapshot, "while-settlement-unavailable")
		*operationIDs = append(*operationIDs, hit.HitId)
		verifyAccessHitReplay(t, client, hit)
		response := accessRequest(t, client, demopb.MsgId_C2S_OPERATION, &demopb.OperationRequest{HitId: hit.HitId})
		if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
			t.Fatalf("结算不可用时不能把查询当成入账结果: %v", response)
		}
		var failure commonv1.ErrorResponse
		if err := proto.Unmarshal(response.Payload, &failure); err != nil || failure.Code != commonv1.ErrorCode_ERROR_CODE_INTERNAL {
			t.Fatalf("结算不可用应明确返回失败: %v %v", &failure, err)
		}
		var amount int64
		var version uint64
		if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
			Scan(&amount, &version); err != nil || amount != before || version != previousVersion {
			t.Fatalf("结算停止时钱包被改动: amount=%d version=%d err=%v", amount, version, err)
		}
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", hit.HitId).Scan(&count); err != nil || count != 0 {
			t.Fatalf("结算停止时出现账务记录: count=%d err=%v", count, err)
		}

		var prepared accessMoleCheckpoint
		if restartMole {
			t.Logf("等待源批次保存：HitID=%s", hit.HitId)
			prepared = waitAccessMoleCheckpoint(t, ctx, redisAddress, player.id, func(state accessMoleCheckpoint) bool {
				if state.Pending == nil {
					return false
				}
				for _, event := range state.Pending.Events {
					if event.HitID == hit.HitId {
						return event.Debit == hit.Debit && event.Credit == hit.Credit
					}
				}
				return false
			})
			if prepared.Pending.Sequence != prepared.Sequence+1 || len(prepared.Pending.Events) != 1 {
				t.Fatalf("源批次序号或事件数量错误: %+v", prepared.Pending)
			}
			t.Logf("源批次已保存：batch=%s sequence=%d", prepared.Pending.ID, prepared.Pending.Sequence)
			if err := mole.process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitAccessProcessListenerClosed(t, mole.address)
		}
		// 客户端不再提交 Hit，只让 Mole 按已接纳的原 ID 恢复提交。
		settlement.process = startAccessProcess(t, settlement.binary, []string{"-config", settlement.config}, settlement.address)
		if restartMole {
			if scenario == "committed-before-receipt" {
				applyAccessPreparedBatch(t, ctx, settlement.address, prepared)
				if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
					Scan(&amount, &version); err != nil || amount != before-hit.Debit+hit.Credit || version != previousVersion+1 {
					t.Fatalf("未构造出已入账状态: amount=%d version=%d err=%v", amount, version, err)
				}
				unconfirmed := waitAccessMoleCheckpoint(t, ctx, redisAddress, player.id, func(state accessMoleCheckpoint) bool {
					return state.Pending != nil && state.Pending.ID == prepared.Pending.ID && state.Sequence == prepared.Sequence
				})
				if unconfirmed.Pool != prepared.Pool {
					t.Fatal("Mole 停止时业务统计不应提前确认")
				}
				t.Log("已构造 SQL 已提交、Mole 检查点仍为 pending 的状态")
			}
			mole.process = startAccessProcess(t, mole.binary, []string{"-config", mole.config}, mole.address)
			t.Log("等待重启后完成原批次确认")
			confirmed := waitAccessMoleCheckpoint(t, ctx, redisAddress, player.id, func(state accessMoleCheckpoint) bool {
				return state.Pending == nil && state.Sequence == prepared.Pending.Sequence && state.LastBatchID == prepared.Pending.ID
			})
			if confirmed.Pool.Hits != prepared.Pool.Hits+1 || confirmed.Pool.Debit != prepared.Pool.Debit+hit.Debit || confirmed.Pool.Credit != prepared.Pool.Credit+hit.Credit {
				t.Fatalf("Mole 恢复后统计重复或丢失: before=%+v after=%+v", prepared.Pool, confirmed.Pool)
			}
			// 房间不持久化；仍用同一账号重新登录/进入，再查询旧 HitID。
			client, _ = accessLoginAndEnter(t, gateAddress, player)
			var origin string
			if err := db.QueryRowContext(ctx, "SELECT origin_batch FROM settlement_operations WHERE operation_id = ?", hit.HitId).Scan(&origin); err != nil || origin != prepared.Pending.ID {
				t.Fatalf("恢复更换了源批次: origin=%s err=%v", origin, err)
			}
		}
		accessWaitCommitted(t, client, hit.HitId)
		accessWaitCommitted(t, client, hit.HitId)
		if err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id = ?", player.id).
			Scan(&amount, &version); err != nil {
			t.Fatal(err)
		}
		if amount != before-hit.Debit+hit.Credit || version != previousVersion+1 {
			t.Fatalf("恢复后未按原操作补齐: amount=%d version=%d hit=%v", amount, version, hit)
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", hit.HitId).Scan(&count); err != nil || count != 1 {
			t.Fatalf("恢复后丢账或重复入账: count=%d err=%v", count, err)
		}
		var leave demopb.LeaveResponse
		accessProcessResponse(t, client, demopb.MsgId_C2S_LEAVE, &demopb.LeaveRequest{}, &leave)
		if !leave.Left {
			t.Fatal("结算恢复后无法离开")
		}
		t.Logf("结算恢复（%s）：原 HitID 仅入账一次，余额 %d，版本 %d → %d", scenario, amount, previousVersion, version)
	}
}

// verifyAccessHitReplay 保持同一连接/房间，验证原请求重提与内容冲突不会产生新的命中结果。
func verifyAccessHitReplay(t *testing.T, client *accessClient, hit *demopb.Hit) {
	t.Helper()
	request := &demopb.HitRequest{ActionId: hit.ActionId, SpawnSeq: hit.SpawnSeq, Hole: hit.Hole}
	var original demopb.HitResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_HIT, request, &original)
	if !proto.Equal(original.Hit, hit) || original.Silver == nil || original.Rejection != "" {
		t.Fatalf("原请求重提改变了 HitID/奖励: %v", &original)
	}
	for _, conflict := range []*demopb.HitRequest{
		{ActionId: hit.ActionId, SpawnSeq: hit.SpawnSeq + 1, Hole: hit.Hole},
		{ActionId: hit.ActionId, SpawnSeq: hit.SpawnSeq, Hole: (hit.Hole + 1) % 9},
	} {
		response := accessRequest(t, client, demopb.MsgId_C2S_HIT, conflict)
		if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
			t.Fatalf("同一 ActionID 改目标必须拒绝: %v", response)
		}
		var replay demopb.HitResponse
		accessProcessResponse(t, client, demopb.MsgId_C2S_HIT, request, &replay)
		if !proto.Equal(&original, &replay) {
			t.Fatalf("重复或冲突改变了原命中/余额: before=%v after=%v", &original, &replay)
		}
	}
	t.Log("同一 ActionID/内容多次提交保持原 HitID、奖励和余额；更改目标序号或洞位被拒，原请求仍返回原结果")
}

// accessMoleCheckpoint 只读取验收所需字段，不导入 Mole 实现或修改检查点。
type accessMoleCheckpoint struct {
	Pool struct {
		Hits, Debit, Credit int64
	} `json:"pool"`
	Sequence    uint64 `json:"sequence"`
	LastBatchID string `json:"last_batch_id"`
	Pending     *struct {
		ID       string `json:"id"`
		Sequence uint64 `json:"sequence"`
		// 持久化 hitEvent 使用 Go 字段名，不能按客户端协议的 hit_id 读取。
		Events []struct {
			HitID      string `json:"HitID"`
			UserID     uint64
			ActionID   string
			AttackName string
			Sequence   uint64
			Hole       int64
			Debit      int64
			Credit     int64
			Rule       string
		} `json:"events"`
	} `json:"pending"`
}

// applyAccessPreparedBatch 经已约定的结算接口提交原事件，只用于构造 SQL 已提交但 Mole 未收到结果的恢复边界。
func applyAccessPreparedBatch(t *testing.T, ctx context.Context, address string, state accessMoleCheckpoint) {
	t.Helper()
	batch := state.Pending
	operations := make([]any, 0, len(batch.Events))
	for _, event := range batch.Events {
		operations = append(operations, map[string]any{
			"operation_id": event.HitID, "business": "mole", "user_id": event.UserID,
			"business_ref": fmt.Sprintf("%s/%d/%d", event.AttackName, event.Sequence, event.Hole),
			"items": []any{
				map[string]any{"reason": "hammer_cost", "debit": event.Debit, "credit": int64(0)},
				map[string]any{"reason": "hammer_reward", "debit": int64(0), "credit": event.Credit},
			},
			"detail": map[string]any{"rule": event.Rule, "action_id": event.ActionID},
		})
	}
	request := map[string]any{"version": uint64(2), "batch_id": batch.ID, "operations": operations}
	result := accessSettlementWrite(t, ctx, address, batch.ID, 1, request)
	receipt, ok := result.(map[string]any)
	if !ok || receipt["batch_id"] != batch.ID {
		t.Fatalf("原批次提交结果不匹配: %v", result)
	}
}

func accessSettlementWrite(t *testing.T, ctx context.Context, address, identity string, command runtime.CommandID, request map[string]any) any {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var result any
	main := &accessFixtureCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		var err error
		result, err = self.Cluster().Call(wait, "settlement", cluster.ServiceName(routing.WriteService(identity)), command, request)
		return err
	}}
	if err := gsr.Run(wait, gsr.Config{
		Logging: logging.Config{Directory: t.TempDir()},
		Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"settlement": address}},
	}, main); err != nil {
		t.Fatal(err)
	}
	return result
}

func waitAccessMoleCheckpoint(t *testing.T, ctx context.Context, address string, userID uint64,
	matches func(accessMoleCheckpoint) bool) accessMoleCheckpoint {
	t.Helper()
	binary, err := exec.LookPath("redis-cli")
	if err != nil {
		t.Fatal("redis-cli required for explicit checkpoint recovery integration")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	key := accessRedisKey
	if lane := userID % 4; lane != 0 {
		key = fmt.Sprintf("%s:lane:%d", key, lane)
	}
	deadline := time.Now().Add(5 * time.Second)
	var state accessMoleCheckpoint
	for time.Now().Before(deadline) {
		read, cancel := context.WithTimeout(ctx, time.Second)
		contents, err := exec.CommandContext(read, binary, "-h", host, "-p", port, "--raw", "GET", key).Output()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		state = accessMoleCheckpoint{}
		if err := json.Unmarshal(contents, &state); err != nil {
			t.Fatal(err)
		}
		if matches(state) {
			return state
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Redis 检查点未在期限内达到预期状态：key=%s state=%+v pending=%+v", key, state, state.Pending)
	return accessMoleCheckpoint{}
}
