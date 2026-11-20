package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	dhmjrpc "github.com/cnooljaw/gsrdemo/common/rpc/dhmj"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// TestDhmjComponentLifecycle 保护定海麻将请求使用当前连接身份，恢复快照只包含本人暗手，确认离开才清路由。
func TestDhmjComponentLifecycle(t *testing.T) {
	identity := agentIdentity{UserID: 7, LoginID: "current", AccessNode: "access-01", AgentName: "agent-7"}
	snapshot := map[string]any{
		"table_name": "dhmj-1", "seat": int64(1), "phase": "playing",
		"dealer_seat": int64(0), "remaining": int64(0),
		"players": []any{
			map[string]any{"user_id": uint64(5), "seat": int64(0)},
			map[string]any{"user_id": uint64(7), "seat": int64(1), "ready": true},
		},
		"hand": []any{int64(1), int64(3)}, "discards": []any{[]any{int64(4)}, []any{}, []any{}, []any{}},
		"other_hands": []any{[]any{int64(9)}},
		"seat_tiles": []any{
			map[string]any{
				"flowers": []any{int64(0x41)},
				"melds": []any{map[string]any{
					"kind": "added_kong", "source_seat": int64(2), "tiles": []any{int64(7), int64(7), int64(7), int64(7)},
				}},
			},
			map[string]any{}, map[string]any{}, map[string]any{},
		},
	}
	var calls []runtime.CommandID
	leaveOK := false
	game := newComDhmj(componentDependencies{
		Identity: func() agentIdentity { return identity },
		AllowEnter: func(name string) error {
			if name != "dhmj" {
				t.Fatalf("游戏名称错误：%q", name)
			}
			return nil
		},
		Call: func(_ gsr.ServiceContext, node cluster.NodeID, service cluster.ServiceName, cmd runtime.CommandID, args ...any) (any, error) {
			want := []any{identity.UserID, identity.LoginID}
			if cmd == dhmjrpc.JoinCommand {
				want = append(want, string(identity.AccessNode), string(identity.AgentName))
			}
			if node != "dhmj" || service != dhmjrpc.ServiceName || !reflect.DeepEqual(args, want) {
				t.Fatalf("错误麻将路由：%s/%s/%d %v", node, service, cmd, args)
			}
			calls = append(calls, cmd)
			switch cmd {
			case dhmjrpc.JoinCommand, dhmjrpc.SnapshotCommand:
				return snapshot, nil
			case dhmjrpc.ReadyCommand:
				return true, nil
			case dhmjrpc.LeaveCommand:
				return leaveOK, nil
			default:
				t.Fatalf("意外麻将命令：%d", cmd)
				return nil, nil
			}
		},
	})

	for _, step := range []struct {
		request, response demopb.MsgId
	}{
		{demopb.MsgId_C2S_DHMJ_ENTER, demopb.MsgId_S2C_DHMJ_ENTER},
		{demopb.MsgId_C2S_DHMJ_READY, demopb.MsgId_S2C_ACTION_ACK},
		{demopb.MsgId_C2S_DHMJ_SNAPSHOT, demopb.MsgId_S2C_DHMJ_SNAPSHOT},
	} {
		response, err := componentRequest(t, game, step.request)
		if err != nil || response.MsgId != uint32(step.response) || !game.Entered() {
			t.Fatalf("麻将请求 %s：%v %v", step.request, response, err)
		}
		if step.request == demopb.MsgId_C2S_DHMJ_SNAPSHOT {
			var body demopb.DhmjSnapshotResponse
			if err := proto.Unmarshal(response.Payload, &body); err != nil {
				t.Fatal(err)
			}
			got := body.GetSnapshot()
			if got.DealerSeat == nil || got.GetDealerSeat() != 0 || got.Remaining == nil || got.GetRemaining() != 0 {
				t.Fatalf("庄家座位或零张余牌在转换时丢失：%v", got)
			}
			if got.GetTableName() != "dhmj-1" || !reflect.DeepEqual(got.GetHand(), []int64{1, 3}) ||
				len(got.GetDiscards()) != 4 || !reflect.DeepEqual(got.GetDiscards()[0].GetTiles(), []int64{4}) {
				t.Fatalf("快照内容错误：%v", got)
			}
			areas := got.GetSeatTiles()
			if len(areas) != 4 || !reflect.DeepEqual(areas[0].GetFlowers(), []int64{0x41}) || len(areas[0].GetMelds()) != 1 {
				t.Fatalf("快照未转出公开花牌与副露：%v", areas)
			}
			meld := areas[0].GetMelds()[0]
			if meld.GetKind() != "added_kong" || meld.GetSourceSeat() != 2 || !reflect.DeepEqual(meld.GetTiles(), []int64{7, 7, 7, 7}) {
				t.Fatalf("快照副露字段错误：%v", meld)
			}
		}
	}
	if _, err := componentRequest(t, game, demopb.MsgId_C2S_DHMJ_LEAVE); !errors.Is(err, errInvalidBackendReply) || !game.Entered() {
		t.Fatalf("未确认离开时丢失路由：%v", err)
	}
	leaveOK = true
	response, err := componentRequest(t, game, demopb.MsgId_C2S_DHMJ_LEAVE)
	if err != nil || response.MsgId != uint32(demopb.MsgId_S2C_ACTION_ACK) || game.Entered() {
		t.Fatalf("确认离开后没有清理路由：%v %v", response, err)
	}
	if want := []runtime.CommandID{dhmjrpc.JoinCommand, dhmjrpc.ReadyCommand, dhmjrpc.SnapshotCommand, dhmjrpc.LeaveCommand, dhmjrpc.LeaveCommand}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("麻将调用链错误：%v", calls)
	}
}

// TestDhmjUnconfirmedEntryDisconnects 保护进桌超时后按固定目录和原登录清理。
func TestDhmjUnconfirmedEntryDisconnects(t *testing.T) {
	calls := 0
	game := newComDhmj(componentDependencies{
		Identity:   func() agentIdentity { return agentIdentity{UserID: 7, LoginID: "original"} },
		AllowEnter: func(string) error { return nil },
		Call: func(_ gsr.ServiceContext, node cluster.NodeID, service cluster.ServiceName, cmd runtime.CommandID, args ...any) (any, error) {
			calls++
			if cmd == dhmjrpc.JoinCommand {
				return nil, runtime.ErrCallTimeout
			}
			if node != "dhmj" || service != dhmjrpc.ServiceName || cmd != dhmjrpc.LeaveCommand ||
				!reflect.DeepEqual(args, []any{uint64(7), "original"}) {
				t.Fatalf("断线没有按原身份清理：%d %v", cmd, args)
			}
			return true, nil
		},
	})
	_, err := componentRequest(t, game, demopb.MsgId_C2S_DHMJ_ENTER)
	var unknown *unknownEnterError
	if !errors.As(err, &unknown) {
		t.Fatalf("未走未知进桌结果的关闭策略：%v", err)
	}
	for range 2 {
		if err := game.Disconnect(nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || game.Entered() {
		t.Fatalf("断线清理未收口：calls=%d entered=%v", calls, game.Entered())
	}
}
