package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/protocol"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// fixtureGame 模拟新增游戏，只提供接入组件契约，不实现玩法或框架行为。
type fixtureGame struct {
	name       string
	entered    bool
	protocols  map[uint32]any
	commands   map[runtime.CommandID]any
	disconnect func(gsr.ServiceContext) error
}

var _ gameComponent = (*fixtureGame)(nil)

func (game *fixtureGame) Name() string {
	return game.name
}

func (game *fixtureGame) Entered() bool {
	return game.entered
}

func (game *fixtureGame) ProtocolHandlers() map[uint32]any {
	return game.protocols
}

func (game *fixtureGame) CommandHandlers() map[runtime.CommandID]any {
	return game.commands
}

func (game *fixtureGame) Disconnect(self gsr.ServiceContext) error {
	if game.disconnect != nil {
		return game.disconnect(self)
	}
	return nil
}

// TestAgentGameListAdmissionAndDispatch 保护第三个游戏只加入清单即可注册两种入口、参与互斥，通用登录不受游戏路由影响。
func TestAgentGameListAdmissionAndDispatch(t *testing.T) {
	registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	first := &fixtureGame{name: "first"}
	third := &fixtureGame{name: "third", commands: map[runtime.CommandID]any{
		200: func(_ gsr.CommandContext) error {
			calls = append(calls, "command")
			return nil
		},
	}, protocols: map[uint32]any{
		uint32(demopb.MsgId_C2S_ENTER): func(_ gsr.CommandContext, reply protocol.Context, _ *demopb.EnterRequest) error {
			calls = append(calls, "protocol")
			return reply.Response(uint32(demopb.MsgId_S2C_ENTER), &demopb.EnterResponse{})
		},
	}}
	agent := &agent{client: &testClientSession{}, games: []gameComponent{first, &fixtureGame{name: "second"}, third}}
	agent.mustBindHandlers(registry)
	if err := agent.allowEnter(third.Name()); !errors.Is(err, errLoginRequired) {
		t.Fatal(err)
	}
	agent.session.userID, agent.session.loginID = 7, "current-login"
	third.entered = true
	if err := agent.allowEnter(third.Name()); err != nil {
		t.Fatal(err)
	}
	if err := agent.allowEnter(first.Name()); !errors.Is(err, errEnterRequired) {
		t.Fatal(err)
	}
	if err := agent.onLogin(nil, protocol.Context{}, &demopb.LoginRequest{}); !errors.Is(err, errAlreadyLoggedIn) {
		t.Fatal(err)
	}
	if _, err := agent.Handle(&exitContext{}, 200); err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(demopb.MsgId_C2S_ENTER), RequestId: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.HandleClientMessage(&exitContext{}, frame); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"command", "protocol"}) {
		t.Fatal(calls)
	}
	third.entered = false
	if err := agent.allowEnter(first.Name()); err != nil {
		t.Fatal(err)
	}
	agent.session.userID, agent.session.loginID = 0, ""
	if err := agent.allowEnter(third.Name()); !errors.Is(err, errLoginRequired) {
		t.Fatal(err)
	}
}

// TestAgentGameListRejectsConflicts 保护空名称、重名游戏和通用/游戏编号冲突在组装时失败。
func TestAgentGameListRejectsConflicts(t *testing.T) {
	for name, games := range map[string][]gameComponent{
		"reserved command": {&fixtureGame{name: "third", commands: map[runtime.CommandID]any{gate.ClientDisconnectedCommand: nil}}},
		"empty name":       {&fixtureGame{}},
		"duplicate name":   {&fixtureGame{name: "same"}, &fixtureGame{name: "same"}},
		"common protocol":  {&fixtureGame{name: "third", protocols: map[uint32]any{uint32(demopb.MsgId_C2S_LOGIN): nil}}},
		"common command":   {&fixtureGame{name: "third", commands: map[runtime.CommandID]any{replacedCommand: nil}}},
		"game protocol":    {&fixtureGame{name: "one", protocols: map[uint32]any{123: nil}}, &fixtureGame{name: "two", protocols: map[uint32]any{123: nil}}},
		"game command":     {&fixtureGame{name: "one", commands: map[runtime.CommandID]any{123: nil}}, &fixtureGame{name: "two", commands: map[runtime.CommandID]any{123: nil}}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				value := recover()
				if value == nil {
					t.Fatal("错误接线未被拒绝")
				}
				expected := "duplicate handler ID"
				if name == "reserved command" {
					expected = "reserved command ID"
				}
				if name == "empty name" || name == "duplicate name" {
					expected = "game name"
				}
				if !strings.Contains(fmt.Sprint(value), expected) {
					t.Fatalf("意外的 panic: %v", value)
				}
			}()
			registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
			if err != nil {
				t.Fatal(err)
			}
			agent := &agent{client: &testClientSession{}, games: games}
			agent.mustBindHandlers(registry)
		})
	}
}

// TestAgentDisconnectContinuesAfterGameError 保护游戏清理报错后仍按清单顺序清理其余游戏、解绑 Online 并清空身份。
func TestAgentDisconnectContinuesAfterGameError(t *testing.T) {
	_, backend, _ := startAccessBusiness(t)
	order := []string{}
	agent := &agent{session: agentSession{userID: 7, loginID: "current-login"}}
	for _, name := range []string{"one", "two", "three"} {
		agent.games = append(agent.games, &fixtureGame{name: name, disconnect: func(gsr.ServiceContext) error {
			order = append(order, name)
			if name == "one" {
				return errors.New("leave failed")
			}
			return nil
		}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	main := &accessFixtureCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		if _, err := callBackend(self, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uint64(7), "current-login", "access-01", "agent-test"); err != nil {
			return err
		}
		agent.disconnect(self)
		value, err := callBackend(self, "online", onlinerpc.ServiceName, 99)
		if err != nil {
			return err
		}
		if value != nil {
			return fmt.Errorf("清理失败后未解绑 Online: %v", value)
		}
		return nil
	}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"online": backend}}}, main); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"one", "two", "three"}) {
		t.Fatal(order)
	}
	if agent.session.userID != 0 || agent.session.loginID != "" {
		t.Fatal("未清空身份")
	}
}
