package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	businesspb "github.com/cnooljaw/gsrdemo/common/gen/demo/business/v1"
	diagnosticpb "github.com/cnooljaw/gsrdemo/common/gen/demo/diagnostic/v1"
	"github.com/cnooljaw/gsrdemo/common/model"
)

// agent 拥有一个连接；其它同接收者方法按登录、位置、游戏和诊断分文件组织。
type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
	commandDispatcher  *gsr.Dispatcher
	name               cluster.ServiceName
	token              string
	enableProbe        bool
	identity           model.Identity
	session            model.SessionRecord
	moleSnapshot       *businesspb.MoleSnapshot
	pending            *model.PositionCommand
	lastAuth           time.Time
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry, enableProbe bool) *agent {
	service := &agent{client: client, enableProbe: enableProbe}
	service.commandDispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		command.ProbePush: service.handlePush,
		command.RoomState: service.handleRoomState,
		command.Replaced:  service.handleReplaced,
	})
	service.protocolDispatcher = protocol.MustDispatcher[gsr.CommandContext](registry, client, map[uint32]any{
		uint32(diagnosticpb.MsgId_C2S_PROBE_REQUEST): service.handleProbe,
		uint32(businesspb.MsgId_C2S_PASSWORD_LOGIN):  service.handleLogin,
		uint32(businesspb.MsgId_C2S_QUERY_SESSION):   service.handleQuerySession,
		uint32(businesspb.MsgId_C2S_QUERY_OPERATION): service.handleQueryOperation,
		uint32(businesspb.MsgId_C2S_ENTER_MOLE):      service.handleEnter,
		uint32(businesspb.MsgId_C2S_LEAVE_ROOM):      service.handleLeave,
		uint32(businesspb.MsgId_C2S_MOLE_SNAPSHOT):   service.handleSnapshot,
		uint32(businesspb.MsgId_C2S_MOLE_HIT):        service.handleHit,
	})
	return service
}

func (service *agent) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

// Handle 与客户端入口共用 Mailbox；Push 不复用旧客户端请求上下文。
func (service *agent) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	if id == gate.ClientDisconnectedCommand {
		service.disconnect(self)
		self.Exit()
		return nil, nil
	}
	return service.commandDispatcher.Handle(self, id, args...)
}

func (service *agent) HandleClientMessage(self gsr.CommandContext, frame []byte) error {
	err := service.protocolDispatcher.HandleFrame(self, frame)
	if err != nil {
		service.disconnect(self)
		self.Exit()
		return errors.Join(err, service.client.Close())
	}
	return nil
}

func (service *agent) register(self gsr.CommandContext) error {
	if service.name != "" {
		return nil
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	value := hex.EncodeToString(token)
	name := cluster.ServiceName("agent/" + value)
	// 只在 Running Handler 中注册，名字与令牌不跨连接复用。
	if err := self.Register(name, self.Self()); err != nil {
		return err
	}
	service.name = name
	service.token = value
	return nil
}

func (service *agent) OnStop(_ context.Context) error {
	return service.client.Close()
}
