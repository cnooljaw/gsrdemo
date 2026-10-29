package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/redis/go-redis/v9"
)

// moleCheckpoint 只由 Submission 访问；Redis 客户端由宿主在 Runtime 停止后关闭。
// 一个值保存统计和最多一个批次，CAS 整体替换避免多字段部分更新与 Lua 浮点精度问题。
type moleCheckpoint struct {
	client *redis.Client
	key    string
}

type moleCheckpointState struct {
	Version     int        `json:"version"`
	Pool        molePool   `json:"pool"`
	Sequence    uint64     `json:"sequence"`
	LastBatchID string     `json:"last_batch_id"`
	Pending     *moleBatch `json:"pending,omitempty"`
}

var replaceMoleCheckpoint = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2])
return 1
`)

func (store moleCheckpoint) initialize(ctx context.Context, pool molePool) error {
	if pool.Hits < 0 || pool.Debit < 0 || pool.Credit < 0 {
		return errInvalidMoleInput
	}
	encoded, err := json.Marshal(moleCheckpointState{Version: 1, Pool: pool})
	if err != nil {
		return err
	}
	if _, err := store.client.SetNX(ctx, store.key, encoded, 0).Result(); err != nil {
		return err
	}
	_, err = store.load(ctx)
	return err
}

func (store moleCheckpoint) read(ctx context.Context) (moleCheckpointState, string, error) {
	var state moleCheckpointState
	if store.client == nil || store.key == "" {
		return state, "", errAccountingUnavailable
	}
	raw, err := store.client.Get(ctx, store.key).Result()
	if err != nil {
		return state, "", fmt.Errorf("read mole checkpoint (initialize explicitly if missing): %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, "", fmt.Errorf("decode mole checkpoint: %w", err)
	}
	if state.Version != 1 || state.Pool.Hits < 0 || state.Pool.Debit < 0 || state.Pool.Credit < 0 ||
		(state.Sequence == 0 && state.LastBatchID != "") || (state.Sequence != 0 && !validMoleIdentifier(state.LastBatchID)) {
		return state, "", errors.New("invalid mole checkpoint")
	}
	if state.Pending != nil && (state.Sequence == math.MaxUint64 || state.Pending.Sequence != state.Sequence+1 || !validMoleBatch(*state.Pending)) {
		return state, "", errors.New("invalid pending mole batch")
	}
	return state, raw, nil
}

func (store moleCheckpoint) load(ctx context.Context) (moleCheckpointState, error) {
	state, _, err := store.read(ctx)
	return state, err
}

func (store moleCheckpoint) replace(ctx context.Context, previous string, state moleCheckpointState) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	changed, err := replaceMoleCheckpoint.Run(ctx, store.client, []string{store.key}, previous, string(encoded)).Int()
	if err != nil {
		return fmt.Errorf("save mole checkpoint: %w", err)
	}
	if changed != 1 {
		return errMoleConflict
	}
	return nil
}

func (store moleCheckpoint) prepare(ctx context.Context, batch moleBatch) error {
	if !validMoleBatch(batch) || batch.Sequence == 0 {
		return errInvalidMoleInput
	}
	state, raw, err := store.read(ctx)
	if err != nil {
		return err
	}
	if batch.Sequence == state.Sequence && batch.ID == state.LastBatchID {
		return nil
	}
	if state.Sequence == math.MaxUint64 || batch.Sequence != state.Sequence+1 {
		return errMoleConflict
	}
	if state.Pending != nil {
		if !reflect.DeepEqual(*state.Pending, batch) {
			return errMoleConflict
		}
		return nil
	}
	copied := batch.clone()
	state.Pending = &copied
	return store.replace(ctx, raw, state)
}

func (store moleCheckpoint) finish(ctx context.Context, batch moleBatch, receipt batchReceipt) (moleCheckpointState, error) {
	state, raw, err := store.read(ctx)
	if err != nil {
		return state, err
	}
	if batch.Sequence == state.Sequence && batch.ID == state.LastBatchID {
		return state, nil
	}
	if state.Pending == nil || !reflect.DeepEqual(*state.Pending, batch) || receipt.BatchID != batch.ID || len(receipt.Results) != len(batch.Events) {
		return state, errMoleConflict
	}
	events := make(map[string]hitEvent, len(batch.Events))
	for _, event := range batch.Events {
		events[event.HitID] = event
	}
	for _, result := range receipt.Results {
		event, exists := events[result.HitID]
		if !exists || (result.Status != "committed" && result.Status != "rejected") || !validMoleIdentifier(result.OriginBatch) {
			return state, errInvalidMoleInput
		}
		delete(events, result.HitID)
		if result.Status == "committed" && result.OriginBatch == batch.ID {
			if err := state.Pool.add(event.Debit, event.Credit); err != nil {
				return state, err
			}
		}
	}
	state.Sequence, state.LastBatchID, state.Pending = batch.Sequence, batch.ID, nil
	if err := store.replace(ctx, raw, state); err != nil {
		return state, err
	}
	return state, nil
}

func validMoleBatch(batch moleBatch) bool {
	if !validMoleIdentifier(batch.ID) || len(batch.Events) == 0 || len(batch.Events) > maxBatchOperations || batchSize(batch) > maxBatchBytes {
		return false
	}
	seen := make(map[string]bool, len(batch.Events))
	for _, event := range batch.Events {
		if !validHitEvent(event) || seen[event.HitID] {
			return false
		}
		seen[event.HitID] = true
	}
	return true
}
