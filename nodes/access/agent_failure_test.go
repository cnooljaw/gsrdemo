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
					case node == "mole" && service == moleentry.ServiceName && command == moleentry.MoleEnterCommand:
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

// TestAccessUnknownEnterClosesConnection 保护进入回执超时后封闭排队的另一游戏请求，地主按已知房间清理。
func TestAccessUnknownEnterClosesConnection(t *testing.T) {
	for _, game := range []string{"mole", "landlord"} {
		t.Run(game, func(t *testing.T) {
			address, backend, events := startAccessBusiness(t, "enter-timeout-"+game)
			client := dialAccess(t, address)
			accessLoginResponse(t, client, "alice", "password", &demopb.LoginResponse{})
			login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
			ids := []demopb.MsgId{demopb.MsgId_C2S_ENTER, demopb.MsgId_C2S_LANDLORD_ENTER}
			if game == "landlord" {
				ids[0], ids[1] = ids[1], ids[0]
			}
			for i, id := range ids {
				frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(id), RequestId: uint32(i + 1)})
				if err != nil {
					t.Fatal(err)
				}
				if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(frame)); err != nil {
					t.Fatal(err)
				}
			}
			response := readAccessEnvelope(t, client)
			if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || response.RequestId != 1 {
				t.Fatalf("未返回原进入错误: %v", response)
			}
			accessExpectClosed(t, client)
			deadline := time.Now().Add(time.Second)
			for accessFixtureRPC(t, backend, 99) != nil {
				if time.Now().After(deadline) {
					t.Fatal("断线未清理 Online")
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case event := <-events:
				if event[0] != "join-"+game {
					t.Fatalf("进入目标错误: %v", event)
				}
			default:
				t.Fatal("后端未收到进入请求")
			}
			if game == "landlord" {
				select {
				case event := <-events:
					if event[0] != "leave-landlord" || event[1] != uint64(7) || event[2] != login {
						t.Fatalf("未按原身份清理: %v", event)
					}
				default:
					t.Fatal("已知地主房间没有收到 Leave")
				}
			}
			if len(events) != 0 {
				t.Fatal("未知进入后仍调用了另一游戏")
			}
		})
	}
}

// TestEnterOutcomeUnknown 保护进入的未知结果与明确拒绝分开处理，且不改写原错误。
func TestEnterOutcomeUnknown(t *testing.T) {
	for _, entry := range []struct {
		err     error
		unknown bool
	}{
		{nil, false}, {runtime.ErrCallTimeout, true}, {context.DeadlineExceeded, true},
		{context.Canceled, true}, {errors.New("connection lost"), true}, {runtime.ErrHandlerFailed, true},
		{runtime.NewStatusError(runtime.ErrorClassDomain, "mole.busy", "busy"), false},
		{runtime.ErrMailboxFull, false}, {runtime.ErrServiceNotFound, false},
		{runtime.ErrServiceStarting, false}, {runtime.ErrServiceStopping, false},
		{runtime.ErrServiceClosed, false}, {runtime.ErrServicePanicked, false},
		{runtime.ErrUnknownCommand, false}, {runtime.ErrInvalidCommandArguments, false},
	} {
		wrapped := enterFailure(entry.err)
		var unknown *unknownEnterError
		if got := errors.As(wrapped, &unknown); got != entry.unknown {
			t.Errorf("err=%v unknown=%v want=%v", entry.err, got, entry.unknown)
		}
		if !errors.Is(wrapped, entry.err) || entry.err != nil && wrapped.Error() != entry.err.Error() {
			t.Errorf("原错误被改变: %v", wrapped)
		}
	}
}

// TestAccessRejectedEnterKeepsConnection 保护业务繁忙和邮箱满的明确拒绝不关闭连接，仍可进入另一游戏。
func TestAccessRejectedEnterKeepsConnection(t *testing.T) {
	for _, scenario := range []string{"enter-busy-domain", "enter-busy-mailbox"} {
		t.Run(scenario, func(t *testing.T) {
			address, _, _ := startAccessBusiness(t, scenario)
			client := dialAccess(t, address)
			accessLoginResponse(t, client, "alice", "password", &demopb.LoginResponse{})
			response := accessRequest(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{})
			if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
				t.Fatal("明确拒绝未返回错误", response)
			}
			accessProcessResponse(t, client, demopb.MsgId_C2S_LANDLORD_ENTER, &demopb.LandlordEnterRequest{}, &demopb.LandlordEnterResponse{})
		})
	}
}
