package main

import (
	"crypto/rand"
	"encoding/hex"
	"math"
)

const (
	hammerCost      int64 = 100
	hammerReward    int64 = 198
	moleRuleVersion       = "demo-99-v1"
)

// molePool 只统计唯一已入账操作，不影响下一锤的概率。
type molePool struct {
	Hits   int64 `json:"hits"`
	Debit  int64 `json:"debit"`
	Credit int64 `json:"credit"`
}

func (pool molePool) snapshot() map[string]any {
	return map[string]any{"hits": pool.Hits, "debit": pool.Debit, "credit": pool.Credit, "net": pool.Debit - pool.Credit}
}

func (pool *molePool) add(debit, credit int64) error {
	if debit < 0 || credit < 0 || pool.Hits == math.MaxInt64 ||
		pool.Debit > math.MaxInt64-debit || pool.Credit > math.MaxInt64-credit {
		return errInvalidMoleInput
	}
	pool.Hits++
	pool.Debit += debit
	pool.Credit += credit
	return nil
}

func randomInstance(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}
