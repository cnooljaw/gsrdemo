package main

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
	"github.com/redis/go-redis/v9"
)

func drawReceipt(op roundOperation) map[string]any {
	fees, changes, balances := []any{}, []any{}, []any{}
	for i, uid := range op.Users {
		debit, credit, fee := int64(50), int64(10), int64(50)
		if i == 0 {
			debit, credit, fee = 80, 0, 50
		}
		fees = append(fees, map[string]any{"user_id": uid, "amount": fee})
		changes = append(changes, map[string]any{"user_id": uid, "debit": debit, "credit": credit})
		balances = append(balances, map[string]any{"user_id": uid, "amount": int64(0), "version": uint64(2)})
	}
	edges := op.wire()["transfers"].([]any)
	for _, value := range edges {
		value.(map[string]any)["amount"] = int64(10)
	}
	return map[string]any{"balances": balances, "results": []any{map[string]any{
		"operation_id": op.ID, "status": "committed", "reason": "",
		"actual_fees": fees, "actual_changes": changes, "actual_transfers": edges,
	}}}
}

// TestDrawSettlementUsesActualPayments 保护流局仍由庄家付三家，回执金额取实扣而且拒绝不一致的明细。
func TestDrawSettlementUsesActualPayments(t *testing.T) {
	op, err := newRoundOperation([4]uint64{4, 3, 2, 1}, defaultRoomRules, roundEnd{kind: draw, dealer: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Transfers) != 3 || op.Transfers[2] != (roundTransfer{0, 3, 100}) {
		t.Fatalf("流局付款错误：%v", op.Transfers)
	}
	reply := drawReceipt(op)
	got, err := op.receipt(reply)
	if err != nil || got[0].Paid != 30 || got[0].Fee != 50 || got[3].Received != 10 {
		t.Fatalf("错误实扣：%+v %v", got, err)
	}
	item := reply["results"].([]any)[0].(map[string]any)
	item["actual_changes"].([]any)[0].(map[string]any)["debit"] = int64(350)
	if _, err := op.receipt(reply); err == nil {
		t.Fatal("接受了与实际费用和付款不符的扣款")
	}
}

type settlementBackend struct {
	journal   settlementJournal
	requests  chan map[string]any
	failFirst bool
}

func (*settlementBackend) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*settlementBackend) OnStop(context.Context) error                      { return nil }

func (b *settlementBackend) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd != settlementwrite.SettleTransferGroupWithFeesCommand {
		return nil, runtime.ErrUnknownCommand
	}
	request := args[0].(map[string]any)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pending, err := b.journal.load(ctx)
	if err != nil || len(pending) != 1 || !reflect.DeepEqual(request, pending[0].operation.wire()) {
		return nil, errors.New("结算调用前未保存完整原请求")
	}
	b.requests <- request
	if b.failFirst {
		b.failFirst = false
		return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "模拟临时中断")
	}
	return drawReceipt(pending[0].operation), nil
}

// TestRoundSettlementRedisRecovery 保护终局先保存、首次调用失败后重启原样补交，以及回执确认后删除待记录。
func TestRoundSettlementRedisRecovery(t *testing.T) {
	addr := os.Getenv("GSRDEMO_DHMJ_REDIS_ADDR")
	if addr == "" {
		t.Skip("set GSRDEMO_DHMJ_REDIS_ADDR to test with local Redis")
	}
	client := redis.NewClient(&redis.Options{Addr: addr, Protocol: 2, DisableIdentity: true, ContextTimeoutEnabled: true, MaxRetries: -1})
	journal := settlementJournal{client: client, key: "gsrdemo:test:dhmj:" + rand.Text()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.Del(ctx, journal.key).Err(); err != nil {
			t.Error(err)
		}
		_ = client.Close()
	})
	routes := make(map[cluster.NodeID]string)
	requests := make(chan map[string]any, 4)
	startDirectoryFixture(t, routes, "settlement", settlementwrite.ServiceName, &settlementBackend{journal: journal, requests: requests, failFirst: true})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	table, err := newAttackService(wallWith(t, nil), 0, 0x31)
	if err != nil {
		t.Fatal(err)
	}
	table.game.end = &roundEnd{kind: draw, dealer: 0}
	table.users, table.journal, table.rules = [4]uint64{1, 2, 3, 4}, journal, defaultRoomRules
	cfg := gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	done := make(chan error, 1)
	go func() {
		done <- gsr.Run(firstCtx, cfg, fixtureMain{onStart: func(ctx context.Context, self gsr.ServiceContext) error {
			_, err := self.CreateService(ctx, table)
			self.Exit()
			return err
		}})
	}()
	first := awaitSettlementRequest(t, ctx, requests, done)
	stopFirst()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	pending, err := journal.load(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("未知结果未保留待记录：%v %v", pending, err)
	}
	routes["dhmj"] = "127.0.0.1:0"
	restartCtx, stopRestart := context.WithCancel(ctx)
	defer stopRestart()
	go func() {
		done <- gsr.Run(restartCtx, cfg, &mainService{node: "dhmj", rules: defaultRoomRules, journal: journal, pending: pending})
	}()
	second := awaitSettlementRequest(t, ctx, requests, done)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("重启改变了原结算请求")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := journal.load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("回执确认后未删除待记录")
		}
	}
	stopRestart()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func awaitSettlementRequest(t *testing.T, ctx context.Context, requests <-chan map[string]any, done <-chan error) map[string]any {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case err := <-done:
		t.Fatalf("定海麻将提前退出：%v", err)
	case <-ctx.Done():
		t.Fatal("未收到结算请求")
	}
	return nil
}
