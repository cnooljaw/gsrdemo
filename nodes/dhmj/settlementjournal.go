package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/redis/go-redis/v9"
)

type settlementJournal struct {
	client *redis.Client
	key    string
}

var errOperationConflict = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.operation_conflict", "定海麻将结算标识冲突")

func (j settlementJournal) save(ctx context.Context, op roundOperation) error {
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	const script = `local old=redis.call('HGET',KEYS[1],ARGV[1]); if old and old~=ARGV[2] then return 0 end; redis.call('HSET',KEYS[1],ARGV[1],ARGV[2]); return 1`
	ok, err := j.client.Eval(ctx, script, []string{j.key}, op.ID, data).Int()
	if err != nil {
		return err
	}
	if ok != 1 {
		return errOperationConflict
	}
	return nil
}

func (j settlementJournal) remove(ctx context.Context, id string) error {
	return j.client.HDel(ctx, j.key, id).Err()
}

func (j settlementJournal) load(ctx context.Context) ([]roundSettlement, error) {
	items, err := j.client.HGetAll(ctx, j.key).Result()
	if err != nil {
		return nil, err
	}
	pending := make([]roundSettlement, 0, len(items))
	for id, item := range items {
		var op roundOperation
		if err := json.Unmarshal([]byte(item), &op); err != nil || op.ID != id || !op.valid() {
			return nil, fmt.Errorf("定海麻将待结算记录无效 %q", id)
		}
		pending = append(pending, roundSettlement{operation: op, saved: true})
	}
	return pending, nil
}
