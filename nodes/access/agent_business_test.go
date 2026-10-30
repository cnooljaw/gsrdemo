package main

import (
	"errors"
	"math"
	"testing"

	"github.com/cnooljaw/gsr/protocol"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// TestAgentRequiresLoginBeforeGame 保护未登录连接不能调用任何游戏入口，不触发下游请求。
func TestAgentRequiresLoginBeforeGame(t *testing.T) {
	agent := &agent{}
	calls := []func() error{
		func() error { return agent.onEnter(nil, protocol.Context{}, &demopb.EnterRequest{}) },
		func() error { return agent.onHit(nil, protocol.Context{}, &demopb.HitRequest{}) },
		func() error { return agent.onSnapshot(nil, protocol.Context{}, &demopb.SnapshotRequest{}) },
		func() error { return agent.onLeave(nil, protocol.Context{}, &demopb.LeaveRequest{}) },
		func() error { return agent.onOperation(nil, protocol.Context{}, &demopb.OperationRequest{}) },
	}
	for index, call := range calls {
		if err := call(); !errors.Is(err, errLoginRequired) {
			t.Fatalf("入口 %d: %v", index, err)
		}
	}
}

// TestAgentBalanceConversionKeepsInt64 保护协议转换保留超过 JS 安全整数范围的余额，不经过 float64。
func TestAgentBalanceConversionKeepsInt64(t *testing.T) {
	balance, err := clientBalance(map[string]any{
		"balance": int64(math.MaxInt64), "available": int64(7), "revision": uint64(8),
	})
	if err != nil || balance.Balance != math.MaxInt64 || balance.Available != 7 || balance.Revision != 8 {
		t.Fatalf("balance=%v err=%v", balance, err)
	}
	if _, err := clientBalance(map[string]any{"balance": float64(10)}); !errors.Is(err, errInvalidBackendReply) {
		t.Fatalf("不允许浮点金额: %v", err)
	}
	confirmed, err := clientBalance(map[string]any{"balance": int64(10), "confirmed_balance": int64(20), "version": uint64(math.MaxUint64)})
	if err != nil || confirmed.GetConfirmed().GetAmount() != 20 || confirmed.GetConfirmed().GetVersion() != math.MaxUint64 {
		t.Fatalf("确认金额与版本必须成对保留: %v %v", confirmed, err)
	}
}
