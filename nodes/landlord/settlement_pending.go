package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// landlordJournal 只操作本游戏独立 Hash，每条记录对应一个原子三人结算。
type landlordJournal struct {
	client *redis.Client
	key    string
}

func (journal landlordJournal) save(ctx context.Context, result *landlordResult) error {
	if !result.valid() {
		return errors.New("invalid landlord result")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	// 重试必须保持原内容；不能覆盖相同 ID 的不同账务。
	const script = `local old=redis.call('HGET',KEYS[1],ARGV[1]); if old and old~=ARGV[2] then return 0 end; redis.call('HSET',KEYS[1],ARGV[1],ARGV[2]); return 1`
	value, err := journal.client.Eval(ctx, script, []string{journal.key}, result.ID, string(data)).Int()
	if err != nil {
		return err
	}
	if value != 1 {
		return errors.New("landlord result identity conflict")
	}
	return nil
}

func (journal landlordJournal) load(ctx context.Context) ([]*landlordResult, error) {
	values, err := journal.client.HGetAll(ctx, journal.key).Result()
	if err != nil {
		return nil, err
	}
	results := make([]*landlordResult, 0, len(values))
	for id, value := range values {
		var result landlordResult
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return nil, err
		}
		if result.ID != id || !result.valid() {
			return nil, fmt.Errorf("invalid pending landlord result %s", id)
		}
		result.Status = "pending"
		results = append(results, &result)
	}
	return results, nil
}
