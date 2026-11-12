package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cnooljaw/gsr/runtime"
	"github.com/redis/go-redis/v9"
)

var errLandlordOperationConflict = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.operation_conflict", "landlord operation identity conflict")

// landlordJournal 只操作本游戏独立 Hash，每条记录对应一个原子三人结算。
type landlordJournal struct {
	client *redis.Client
	key    string
}

func (journal landlordJournal) save(ctx context.Context, operation *landlordOperation) error {
	if !operation.valid() {
		return errLandlordInput
	}
	data, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	// 重试必须保持原内容；不能覆盖相同 ID 的不同账务。
	const script = `local old=redis.call('HGET',KEYS[1],ARGV[1]); if old and old~=ARGV[2] then return 0 end; redis.call('HSET',KEYS[1],ARGV[1],ARGV[2]); return 1`
	value, err := journal.client.Eval(ctx, script, []string{journal.key}, operation.ID, string(data)).Int()
	if err != nil {
		return err
	}
	if value != 1 {
		return errLandlordOperationConflict
	}
	return nil
}

func (journal landlordJournal) load(ctx context.Context) ([]*landlordOperation, error) {
	values, err := journal.client.HGetAll(ctx, journal.key).Result()
	if err != nil {
		return nil, err
	}
	operations := make([]*landlordOperation, 0, len(values))
	for id, value := range values {
		var operation landlordOperation
		if err := json.Unmarshal([]byte(value), &operation); err != nil {
			return nil, err
		}
		if operation.ID != id || !operation.valid() {
			return nil, fmt.Errorf("invalid pending landlord operation %s", id)
		}
		operation.Status = "pending"
		operations = append(operations, &operation)
	}
	return operations, nil
}
