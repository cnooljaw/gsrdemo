package main

import (
	"errors"
	"math"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleattack"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// TestAgentRequiresLoginBeforeGame 保护未登录连接不能调用任何游戏入口，不触发下游请求。
func TestAgentRequiresLoginBeforeGame(t *testing.T) {
	deps := componentDependencies{Identity: func() agentIdentity { return agentIdentity{} }, AllowEnter: func(string) error { return errLoginRequired }}
	mole, landlord := newComMole(deps), newComLandlord(deps)
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
		if err := call(); !errors.Is(err, errLoginRequired) {
			t.Fatalf("入口 %d: %v", index, err)
		}
	}
}

// TestMoleLeaveRequiresConfirmation 保护只有 Leave 明确返回 true 才清路由，拒绝和未知结果保留原路由供清理。
func TestMoleLeaveRequiresConfirmation(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
		err   error
		want  error
	}{
		{name: "confirmed", value: true},
		{name: "not confirmed", value: false, want: errInvalidBackendReply},
		{name: "invalid type", value: "true", want: errInvalidBackendReply},
		{name: "unknown outcome", err: runtime.ErrCallTimeout, want: runtime.ErrCallTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			mole := newComMole(componentDependencies{
				Identity: func() agentIdentity { return agentIdentity{UserID: 7, LoginID: "current"} },
				Call: func(_ gsr.ServiceContext, node cluster.NodeID, service cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
					if node != "mole" || service != "attack" || command != moleattack.AttackLeaveCommand ||
						len(args) != 2 || args[0] != uint64(7) || args[1] != "current" {
						t.Fatalf("离开请求未使用当前路由和身份: %s/%s/%d %v", node, service, command, args)
					}
					return test.value, test.err
				},
			})
			mole.attackName = "attack"
			response, err := componentRequest(t, mole, demopb.MsgId_C2S_LEAVE)
			if !errors.Is(err, test.want) || mole.Entered() != (test.want != nil) {
				t.Fatalf("离开确认错误: entered=%v err=%v want=%v", mole.Entered(), err, test.want)
			}
			if test.want == nil {
				var leave demopb.LeaveResponse
				if response.MsgId != uint32(demopb.MsgId_S2C_LEAVE) || proto.Unmarshal(response.Payload, &leave) != nil || !leave.Left {
					t.Fatalf("未返回离开确认: %v", response)
				}
			} else if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
				t.Fatalf("未确认离开却返回成功: %v", response)
			}
		})
	}
}

// TestGameSnapshotRepliesMatchCurrentRoute 保护主动快照及各桌内操作拒绝其他组/桌的回包，匹配路由时正常返回。
func TestGameSnapshotRepliesMatchCurrentRoute(t *testing.T) {
	for _, request := range []struct {
		name string
		id   demopb.MsgId
	}{
		{"mole snapshot", demopb.MsgId_C2S_SNAPSHOT},
		{"landlord ready", demopb.MsgId_C2S_LANDLORD_READY},
		{"landlord play", demopb.MsgId_C2S_LANDLORD_PLAY},
		{"landlord snapshot", demopb.MsgId_C2S_LANDLORD_SNAPSHOT},
		{"landlord call", demopb.MsgId_C2S_LANDLORD_CALL},
		{"landlord rob", demopb.MsgId_C2S_LANDLORD_ROB},
	} {
		t.Run(request.name, func(t *testing.T) {
			for _, route := range []string{"other-room", "current-room"} {
				t.Run(route, func(t *testing.T) {
					field := "room_name"
					if request.id == demopb.MsgId_C2S_SNAPSHOT {
						field = "attack_name"
					}
					deps := componentDependencies{
						Identity: func() agentIdentity { return agentIdentity{UserID: 7, LoginID: "current"} },
						Call: func(_ gsr.ServiceContext, _ cluster.NodeID, service cluster.ServiceName, _ runtime.CommandID, args ...any) (any, error) {
							if service != "current-room" || len(args) < 2 || args[0] != uint64(7) || args[1] != "current" {
								t.Fatalf("请求未使用当前路由和身份: %s %v", service, args)
							}
							return map[string]any{field: route}, nil
						},
					}
					mole, landlord := newComMole(deps), newComLandlord(deps)
					mole.attackName, landlord.room = "current-room", "current-room"
					var game gameComponent = landlord
					if request.id == demopb.MsgId_C2S_SNAPSHOT {
						game = mole
					}
					response, err := componentRequest(t, game, request.id)
					if route == "other-room" {
						if !errors.Is(err, errInvalidBackendReply) || response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
							t.Fatalf("其他房间快照未被拒绝: %v %v", response, err)
						}
					} else if err != nil || response.MsgId == uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
						t.Fatalf("当前房间快照未正常返回: %v %v", response, err)
					}
					if mole.attackName != "current-room" || landlord.room != "current-room" {
						t.Fatal("回包替换了当前路由")
					}
				})
			}
		})
	}
}

// componentRequest 通过真实协议分发器调用组件，并读取唯一的成功或错误响应。
func componentRequest(t *testing.T, game gameComponent, id demopb.MsgId) (*commonv1.Envelope, error) {
	t.Helper()
	registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
	if err != nil {
		t.Fatal(err)
	}
	client := &accessPushClient{frames: make(chan []byte, 1)}
	dispatcher := protocol.MustDispatcher[gsr.CommandContext](registry, client, game.ProtocolHandlers())
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(id), RequestId: 1})
	if err != nil {
		t.Fatal(err)
	}
	handleErr := dispatcher.HandleFrame(nil, frame)
	if len(client.frames) != 1 {
		t.Fatalf("需要唯一响应: frames=%d err=%v", len(client.frames), handleErr)
	}
	var response commonv1.Envelope
	if err := proto.Unmarshal(<-client.frames, &response); err != nil {
		t.Fatal(err)
	}
	return &response, handleErr
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

// TestGameReadsCurrentIdentity 保护组件不缓存构造时身份，登录和身份清理立即生效，RPC 使用当前 UserID/LoginID。
func TestGameReadsCurrentIdentity(t *testing.T) {
	for _, name := range []string{"mole", "landlord"} {
		t.Run(name, func(t *testing.T) {
			current := agentIdentity{}
			calls := 0
			deps := componentDependencies{Identity: func() agentIdentity { return current }, Call: func(_ gsr.ServiceContext, _ cluster.NodeID, _ cluster.ServiceName, _ runtime.CommandID, args ...any) (any, error) {
				calls++
				if args[0] != current.UserID || args[1] != current.LoginID {
					t.Fatalf("使用过期身份: %v", args)
				}
				return nil, runtime.ErrCallTimeout
			}}
			mole, landlord := newComMole(deps), newComLandlord(deps)
			mole.attackName, landlord.room = "mole-room", "landlord-room"
			snapshot := func() error { return mole.onSnapshot(nil, protocol.Context{}, &demopb.SnapshotRequest{}) }
			if name == "landlord" {
				snapshot = func() error { return landlord.onSnapshot(nil, protocol.Context{}, &demopb.LandlordSnapshotRequest{}) }
			}
			if err := snapshot(); !errors.Is(err, errLoginRequired) {
				t.Fatal(err)
			}
			for _, identity := range []agentIdentity{{UserID: 7, LoginID: "first"}, {UserID: 9, LoginID: "second"}} {
				current = identity
				if err := snapshot(); !errors.Is(err, runtime.ErrCallTimeout) {
					t.Fatal(err)
				}
			}
			current = agentIdentity{}
			if err := snapshot(); !errors.Is(err, errLoginRequired) {
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
	deps := componentDependencies{Identity: func() agentIdentity { return agentIdentity{UserID: 7, LoginID: "current"} }, Call: func(_ gsr.ServiceContext, _ cluster.NodeID, _ cluster.ServiceName, _ runtime.CommandID, _ ...any) (any, error) {
		calls++
		return nil, runtime.ErrCallTimeout
	}}
	mole, landlord := newComMole(deps), newComLandlord(deps)
	mole.attackName, landlord.room = "attack", "room"
	for _, game := range []gameComponent{mole, landlord} {
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
