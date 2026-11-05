package gameagent

import (
	"errors"
	"math"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// TestAgentRequiresLoginBeforeGame 保护未登录连接不能调用任何游戏入口，不触发下游请求。
func TestAgentRequiresLoginBeforeGame(t *testing.T) {
	deps := Dependencies{Identity: func() Identity { return Identity{} }, AllowEnter: func(string) error { return ErrLoginRequired }}
	mole, landlord := NewMole(deps), NewLandlord(deps)
	calls := []func() error{
		func() error { return mole.onEnter(nil, protocol.Context{}, &demopb.EnterRequest{}) },
		func() error { return mole.onHit(nil, protocol.Context{}, &demopb.HitRequest{}) },
		func() error { return mole.onSnapshot(nil, protocol.Context{}, &demopb.SnapshotRequest{}) },
		func() error { return mole.onLeave(nil, protocol.Context{}, &demopb.LeaveRequest{}) },
		func() error { return landlord.onEnter(nil, protocol.Context{}, &demopb.LandlordEnterRequest{}) },
		func() error { return landlord.onReady(nil, protocol.Context{}, &demopb.LandlordReadyRequest{}) },
		func() error { return landlord.onCall(nil, protocol.Context{}, &demopb.LandlordCallRequest{}) },
		func() error { return landlord.onRob(nil, protocol.Context{}, &demopb.LandlordRobRequest{}) },
		func() error { return landlord.onPlay(nil, protocol.Context{}, &demopb.LandlordPlayRequest{}) },
		func() error {
			return landlord.onSnapshot(nil, protocol.Context{}, &demopb.LandlordSnapshotRequest{})
		},
		func() error { return landlord.onLeave(nil, protocol.Context{}, &demopb.LandlordLeaveRequest{}) },
	}
	for index, call := range calls {
		if err := call(); !errors.Is(err, ErrLoginRequired) {
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
	if _, err := clientBalance(map[string]any{"balance": float64(10)}); !errors.Is(err, ErrInvalidBackendReply) {
		t.Fatalf("不允许浮点金额: %v", err)
	}
	confirmed, err := clientBalance(map[string]any{"balance": int64(10), "confirmed_balance": int64(20), "version": uint64(math.MaxUint64)})
	if err != nil || confirmed.GetConfirmed().GetAmount() != 20 || confirmed.GetConfirmed().GetVersion() != math.MaxUint64 {
		t.Fatalf("确认金额与版本必须成对保留: %v %v", confirmed, err)
	}
}

// TestGameReadsCurrentIdentity 保护组件不缓存构造时身份，登录和身份清理立即生效，RPC 使用当前 UserID/LoginID。
func TestGameReadsCurrentIdentity(t *testing.T) {
	for _, name := range []string{"mole", "landlord"} {
		t.Run(name, func(t *testing.T) {
			current := Identity{}
			calls := 0
			deps := Dependencies{Identity: func() Identity { return current }, Call: func(_ gsr.ServiceContext, _ cluster.NodeID, _ cluster.ServiceName, _ runtime.CommandID, args ...any) (any, error) {
				calls++
				if args[0] != current.UserID || args[1] != current.LoginID {
					t.Fatalf("使用过期身份: %v", args)
				}
				return nil, runtime.ErrCallTimeout
			}}
			mole, landlord := NewMole(deps), NewLandlord(deps)
			mole.attackName, landlord.room = "mole-room", "landlord-room"
			snapshot := func() error { return mole.onSnapshot(nil, protocol.Context{}, &demopb.SnapshotRequest{}) }
			if name == "landlord" {
				snapshot = func() error { return landlord.onSnapshot(nil, protocol.Context{}, &demopb.LandlordSnapshotRequest{}) }
			}
			if err := snapshot(); !errors.Is(err, ErrLoginRequired) {
				t.Fatal(err)
			}
			for _, identity := range []Identity{{UserID: 7, LoginID: "first"}, {UserID: 9, LoginID: "second"}} {
				current = identity
				if err := snapshot(); !errors.Is(err, runtime.ErrCallTimeout) {
					t.Fatal(err)
				}
			}
			current = Identity{}
			if err := snapshot(); !errors.Is(err, ErrLoginRequired) {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("未登录时调用下游: %d", calls)
			}
		})
	}
}

// TestGameDisconnectClearsRouteOnFailure 保护离开下游失败后仍清空本连接路由，重复清理不再发起 RPC。
func TestGameDisconnectClearsRouteOnFailure(t *testing.T) {
	calls := 0
	deps := Dependencies{Identity: func() Identity { return Identity{UserID: 7, LoginID: "current"} }, Call: func(_ gsr.ServiceContext, _ cluster.NodeID, _ cluster.ServiceName, _ runtime.CommandID, _ ...any) (any, error) {
		calls++
		return nil, runtime.ErrCallTimeout
	}}
	mole, landlord := NewMole(deps), NewLandlord(deps)
	mole.attackName, landlord.room = "attack", "room"
	for _, game := range []Component{mole, landlord} {
		if !game.Entered() {
			t.Fatal("缺少测试路由")
		}
		if err := game.Disconnect(nil); !errors.Is(err, runtime.ErrCallTimeout) {
			t.Fatal(err)
		}
		if game.Entered() {
			t.Fatalf("%s 失败后仍持有路由", game.Name())
		}
		if err := game.Disconnect(nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("清理失败后再次调用了下游: calls=%d", calls)
	}
}
