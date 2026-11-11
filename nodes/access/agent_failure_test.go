package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/protocol"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	landlordrpc "github.com/cnooljaw/gsrdemo/common/rpc/landlord"
	"github.com/cnooljaw/gsrdemo/common/rpc/landlordattack"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleattack"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleentry"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// rejectCreateContext 只注入辅助 Service 创建失败，清理仍使用当前真实回调的能力。
type rejectCreateContext struct {
	gsr.ServiceContext
	exited bool
}

func (*rejectCreateContext) CreateService(context.Context, gsr.Service) (runtime.ServiceID, error) {
	return 0, errors.New("service creation refused")
}

func (self *rejectCreateContext) Exit() { self.exited = true }

// TestAgentFailureCloserStartupFailureCleansSession 保护关闭辅助 Service 创建失败时仍尝试游戏清理，清空身份并退出。
func TestAgentFailureCloserStartupFailureCleansSession(t *testing.T) {
	cleaned := false
	agent := &agent{
		session: agentSession{userID: 7, loginID: "current"},
		games: []gameComponent{&fixtureGame{name: "game", disconnect: func(gsr.ServiceContext) error {
			cleaned = true
			return errors.New("leave unavailable")
		}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	main := &accessFixtureCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		failed := &rejectCreateContext{ServiceContext: self}
		agent.closeAfterFailure(failed)
		if !failed.exited || !agent.closing || !cleaned || agent.session.userID != 0 || agent.session.loginID != "" {
			t.Fatalf("创建失败后未清理并退出: exited=%v closing=%v cleaned=%v session=%+v", failed.exited, agent.closing, cleaned, agent.session)
		}
		return nil
	}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}}, main); err != nil {
		t.Fatal(err)
	}
}

// TestGameInvalidEnterReplyKeepsCleanupRoute 保护 Join 后回包非法时保留已知路由，断线仍按当前身份向原目标尝试 Leave。
func TestGameInvalidEnterReplyKeepsCleanupRoute(t *testing.T) {
	for _, name := range []string{"mole balance", "landlord snapshot", "landlord wrong room"} {
		t.Run(name, func(t *testing.T) {
			leaves := 0
			deps := componentDependencies{
				Identity:   func() agentIdentity { return agentIdentity{UserID: 7, LoginID: "current"} },
				AllowEnter: func(string) error { return nil },
				Call: func(_ gsr.ServiceContext, node cluster.NodeID, service cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
					switch {
					case node == "mole" && service == "mole":
						return map[string]any{"entry_name": "entry"}, nil
					case node == "mole" && service == "entry" && command == moleentry.MoleEnterCommand:
						snapshot := fixtureSnapshot()
						snapshot["silver"] = map[string]any{"balance": "invalid"}
						return snapshot, nil
					case node == "landlord" && service == landlordrpc.ServiceName && command == landlordrpc.ResolveCommand:
						return map[string]any{"room_name": "original-room", "membership": "member"}, nil
					case node == "landlord" && service == "original-room" && command == landlordattack.RoomJoinCommand:
						if name == "landlord wrong room" {
							return map[string]any{"room_name": "other-room"}, nil
						}
						return map[string]any{}, nil
					case node == "mole" && service == "attack" && command == moleattack.AttackLeaveCommand,
						node == "landlord" && service == "original-room" && command == landlordattack.RoomLeaveCommand:
						leaves++
						if len(args) != 2 || args[0] != uint64(7) || args[1] != "current" {
							t.Fatalf("清理身份错误: %v", args)
						}
						return true, nil
					}
					t.Fatalf("意外调用: %s/%s/%d", node, service, command)
					return nil, nil
				},
			}
			var game gameComponent
			id := demopb.MsgId_C2S_ENTER
			if name == "mole balance" {
				game = newComMole(deps)
			} else {
				game = newComLandlord(deps)
				id = demopb.MsgId_C2S_LANDLORD_ENTER
			}
			registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
			if err != nil {
				t.Fatal(err)
			}
			dispatcher := protocol.MustDispatcher[gsr.CommandContext](registry, &testClientSession{}, game.ProtocolHandlers())
			frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(id), RequestId: 1})
			if err != nil {
				t.Fatal(err)
			}
			err = dispatcher.HandleFrame(nil, frame)
			if !errors.Is(err, errInvalidBackendReply) || !game.Entered() {
				t.Fatalf("非法回包应保留清理路由: entered=%v err=%v", game.Entered(), err)
			}
			if err := game.Disconnect(nil); err != nil || leaves != 1 || game.Entered() {
				t.Fatalf("未清理原路由: leaves=%d entered=%v err=%v", leaves, game.Entered(), err)
			}
		})
	}
}

// TestAccessInvalidGameReplyClosesConnection 保护非法进入回包先返回错误，封闭排队中的命中请求，再延时断线并清理 Online。
func TestAccessInvalidGameReplyClosesConnection(t *testing.T) {
	address, backend, hits := startAccessBusiness(t, "invalid-enter-balance")
	client := dialAccess(t, address)
	accessLoginResponse(t, client, "alice", "password", &demopb.LoginResponse{})
	for index, request := range []struct {
		id      demopb.MsgId
		message proto.Message
	}{
		{demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}},
		{demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: "must-not-run", SpawnSeq: 9, Hole: 0}},
	} {
		payload, err := proto.Marshal(request.message)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(request.id), RequestId: uint32(index + 1), Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		_ = client.SetWriteDeadline(time.Now().Add(time.Second))
		if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(frame)); err != nil {
			t.Fatal(err)
		}
	}
	response := readAccessEnvelope(t, client)
	if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || response.RequestId != 1 {
		t.Fatalf("非法回包未返回进入错误: %v", response)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("错误后仍收到业务响应")
	} else {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			t.Fatal("非法回包后未关闭连接")
		}
	}
	if len(hits) != 0 {
		t.Fatal("非法回包后仍向后端发送了命中")
	}
	deadline := time.Now().Add(time.Second)
	for accessFixtureRPC(t, backend, 99) != nil {
		if time.Now().After(deadline) {
			t.Fatal("断线后未清理 Online")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
