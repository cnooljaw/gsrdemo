package main

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/redis/go-redis/v9"
)

// landlordSaveFault 只注入保存待请求时的临时 Redis 故障，并观察保存次数。
type landlordSaveFault struct {
	unavailable atomic.Bool
	attempts    atomic.Int32
}

var _ redis.Hook = (*landlordSaveFault)(nil)

func (*landlordSaveFault) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (*landlordSaveFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (fault *landlordSaveFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if command.Name() == "eval" {
			fault.attempts.Add(1)
			if fault.unavailable.Load() {
				return errors.New("test Redis save unavailable")
			}
		}
		return next(ctx, command)
	}
}

// TestLandlordJournalPermanentFailurePausesRound 保护无效请求和身份冲突不覆盖 Redis 原记录，保留内存操作并停止再次保存或结算。
func TestLandlordJournalPermanentFailurePausesRound(t *testing.T) {
	for _, invalid := range []bool{true, false} {
		name, code := "identity conflict", "landlord.operation_conflict"
		if invalid {
			name, code = "invalid operation", "landlord.invalid_request"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			journal := landlordJournal{client: startLandlordRedis(t), key: "test:landlord:pending"}
			game := testLandlordGame(t)
			game.Phase, game.Winner = "finished", game.Landlord
			stored := operationForGame(*game)
			if err := journal.save(ctx, stored); err != nil {
				t.Fatal(err)
			}
			original, err := journal.client.HGet(ctx, journal.key, stored.ID).Result()
			if err != nil {
				t.Fatal(err)
			}
			operation := *stored
			if invalid {
				operation.Changes[0].Debit++
			} else {
				operation.Multiplier *= 2
				for i, change := range operation.Changes {
					operation.Changes[i] = operation.change(i, change.UserID)
				}
				if !operation.valid() {
					t.Fatal("冲突夹具必须是另一个合法请求")
				}
			}
			want := operation.wire()
			fault := &landlordSaveFault{}
			journal.client.AddHook(fault)
			round := roundSettlement{journal: journal, operation: &operation}
			err = round.submit(nil)
			var status *runtime.StatusError
			if !errors.As(err, &status) || status.Class != runtime.ErrorClassDomain || status.Code != runtime.ErrorCode(code) || round.fault == nil {
				t.Fatalf("确定性保存错误未分类并暂停: err=%v fault=%v", err, round.fault)
			}
			if !round.pending() || round.cleanupPending || !reflect.DeepEqual(round.operation.wire(), want) {
				t.Fatal("保存失败改变了待操作或伪造终态")
			}
			attempts := fault.attempts.Load()
			round.retryAt = time.Time{}
			if err := round.submit(nil); err != nil || fault.attempts.Load() != attempts {
				t.Fatalf("暂停后仍尝试保存: %v", err)
			}
			current, err := journal.client.HGet(ctx, journal.key, stored.ID).Result()
			if err != nil || current != original {
				t.Fatal("原 Redis 记录被覆盖或删除")
			}
		})
	}
}
