package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
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

// terminalClient 仅记录 Agent 选择的发送入口，不模拟 Gate 排队和网络写出。
type terminalClient struct {
	normal, terminal, closes int
	frame                    []byte
	terminalErr              error
}

func (client *terminalClient) Send(frame []byte) error {
	client.normal++
	client.frame = append([]byte(nil), frame...)
	return nil
}
func (client *terminalClient) SendAndClose(frame []byte) error {
	client.terminal++
	client.frame = append([]byte(nil), frame...)
	return client.terminalErr
}
func (client *terminalClient) Close() error {
	client.closes++
	return nil
}

// TestAgentErrorPolicy 保护错误策略使用唯一安全响应，末帧接纳失败不回错两次，closing 拦截后续客户端请求。
func TestAgentErrorPolicy(t *testing.T) {
	for _, entry := range []struct {
		name           string
		id             demopb.MsgId
		cause, sendErr error
		close          bool
	}{
		{name: "login", id: demopb.MsgId_C2S_LOGIN, close: true},
		{name: "password auth", id: demopb.MsgId_C2S_PASSWORD_AUTH, close: true},
		{name: "invalid backend", cause: errInvalidBackendReply, close: true},
		{name: "unknown enter", cause: enterFailure(runtime.ErrCallTimeout), close: true},
		{name: "terminal queue full", cause: errInvalidBackendReply, sendErr: gate.ErrClientQueueFull, close: true},
		{name: "terminal frame too large", cause: errInvalidBackendReply, sendErr: gate.ErrFrameTooLarge, close: true},
		{name: "busy", cause: runtime.NewStatusError(runtime.ErrorClassDomain, "mole.busy", "private diagnostic")},
		{name: "mailbox full", cause: runtime.ErrMailboxFull},
	} {
		t.Run(entry.name, func(t *testing.T) {
			id := entry.id
			if id == 0 {
				id = demopb.MsgId_C2S_ENTER
			}
			calls := 0
			client := &terminalClient{terminalErr: entry.sendErr}
			agent := &agent{client: client, session: agentSession{userID: 7, loginID: "current"}, games: []gameComponent{
				&fixtureGame{name: "fixture", protocols: map[uint32]any{
					uint32(demopb.MsgId_C2S_ENTER): func(_ gsr.CommandContext, _ protocol.Context, _ *demopb.EnterRequest) error {
						calls++
						return entry.cause
					},
				}},
			}}
			registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
			if err != nil {
				t.Fatal(err)
			}
			agent.mustBindHandlers(registry)
			frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(id), RequestId: 17})
			if err != nil {
				t.Fatal(err)
			}
			err = agent.protocolDispatcher.HandleFrame(nil, frame)
			cause := entry.cause
			if entry.id != 0 {
				cause = errAlreadyLoggedIn
			}
			if !errors.Is(err, cause) || entry.sendErr != nil && !errors.Is(err, entry.sendErr) {
				t.Fatalf("原错误/发送错误丢失: %v", err)
			}
			if agent.closing != entry.close || client.normal+client.terminal != 1 || (client.terminal == 1) != entry.close {
				t.Fatalf("策略未接线: closing=%v normal=%d terminal=%d", agent.closing, client.normal, client.terminal)
			}
			if client.closes != 0 {
				t.Fatal("错误策略提前 Close，中止了末帧")
			}
			var envelope commonv1.Envelope
			var failure commonv1.ErrorResponse
			if proto.Unmarshal(client.frame, &envelope) != nil || envelope.RequestId != 17 || envelope.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || proto.Unmarshal(envelope.Payload, &failure) != nil {
				t.Fatal("错误未关联原请求")
			}
			if failure.Message != "request could not be processed" {
				t.Fatalf("错误文案不安全: %v", &failure)
			}
			if entry.close {
				if err := agent.HandleClientMessage(nil, frame); err != nil {
					t.Fatal(err)
				}
				if client.normal+client.terminal != 1 || calls > 1 {
					t.Fatal("closing 后仍处理客户端请求")
				}
			}
		})
	}
}

// TestGameInvalidEnterReplyKeepsCleanupRoute 保护 Join 后回包非法时保留已知路由，断线仍按当前身份向原目标尝试 Leave。
func TestGameInvalidEnterReplyKeepsCleanupRoute(t *testing.T) {
	for _, name := range []string{"mole balance", "mole snapshot", "mole missing route", "mole invalid route", "mole empty route", "mole nonmap", "landlord snapshot", "landlord wrong room"} {
		t.Run(name, func(t *testing.T) {
			leaves := 0
			deps := componentDependencies{
				Identity:   func() agentIdentity { return agentIdentity{UserID: 7, LoginID: "current"} },
				AllowEnter: func(string) error { return nil },
				Call: func(_ gsr.ServiceContext, node cluster.NodeID, service cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
					switch {
					case node == "mole" && service == moleentry.ServiceName && command == moleentry.MoleEnterCommand:
						snapshot := fixtureSnapshot()
						snapshot["silver"] = map[string]any{"balance": int64(1000)}
						switch name {
						case "mole balance":
							snapshot["silver"] = map[string]any{"balance": "invalid"}
						case "mole snapshot":
							snapshot["seat"] = "invalid"
						case "mole missing route":
							delete(snapshot, "attack_name")
						case "mole invalid route":
							snapshot["attack_name"] = uint64(7)
						case "mole empty route":
							snapshot["attack_name"] = ""
						case "mole nonmap":
							return nil, nil
						}
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
			if name == "landlord snapshot" || name == "landlord wrong room" {
				game = newComLandlord(deps)
				id = demopb.MsgId_C2S_LANDLORD_ENTER
			} else {
				mole := newComMole(deps)
				if name != "mole balance" && name != "mole snapshot" {
					// 无可用新路由时，非法回包不得抹掉原有清理目标。
					mole.attackName = "attack"
				}
				game = mole
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

// TestAccessInvalidGameReplyClosesConnection 保护非法进入回包先返回错误，封闭排队中的命中请求，再由 Gate 写出后关闭并清理 Online。
func TestAccessInvalidGameReplyClosesConnection(t *testing.T) {
	barrier := newAccessEntryBarrier()
	address, backend, hits := startAccessBusinessFixture(t, "invalid-enter-balance", barrier)
	t.Cleanup(barrier.release)
	client := dialAccess(t, address)
	accessLoginResponse(t, client, "alice", "password", &demopb.LoginResponse{})
	for index, request := range []struct {
		id      demopb.MsgId
		message proto.Message
	}{
		{demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}},
		{demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: "must-not-run", SpawnSeq: 9, Hole: 0}},
	} {
		if index == 1 {
			barrier.wait(t)
		}
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
	barrier.release()
	response := readAccessEnvelope(t, client)
	if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || response.RequestId != 1 {
		t.Fatalf("非法回包未返回进入错误: %v", response)
	}
	accessExpectNoMoreMessages(t, client)
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
			barrier := newAccessEntryBarrier()
			address, backend, events := startAccessBusinessFixture(t, "enter-timeout-"+game, barrier)
			t.Cleanup(barrier.release)
			client := dialAccess(t, address)
			accessLoginResponse(t, client, "alice", "password", &demopb.LoginResponse{})
			login := accessFixtureRPC(t, backend, 99).(map[string]any)["login_id"]
			ids := []demopb.MsgId{demopb.MsgId_C2S_ENTER, demopb.MsgId_C2S_LANDLORD_ENTER}
			if game == "landlord" {
				ids[0], ids[1] = ids[1], ids[0]
			}
			for i, id := range ids {
				if i == 1 {
					barrier.wait(t)
				}
				frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(id), RequestId: uint32(i + 1)})
				if err != nil {
					t.Fatal(err)
				}
				if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(frame)); err != nil {
					t.Fatal(err)
				}
			}
			barrier.release()
			response := readAccessEnvelope(t, client)
			if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || response.RequestId != 1 {
				t.Fatalf("未返回原进入错误: %v", response)
			}
			accessExpectNoMoreMessages(t, client)
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

// accessExpectNoMoreMessages 验证终止错误之后没有第二份响应，连接实际关闭而非读超时。
func accessExpectNoMoreMessages(t *testing.T, client *accessClient) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := client.ReadMessage(); err == nil {
		t.Fatal("终止错误后仍收到额外消息")
	} else {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			t.Fatal("终止错误后未关闭连接")
		}
	}
}
