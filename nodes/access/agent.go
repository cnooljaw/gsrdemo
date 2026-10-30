package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// agent 独占当前连接身份和路由，不持有权威游戏状态或钱包。
type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
	commandDispatcher  *gsr.Dispatcher
	node               cluster.NodeID
	name               cluster.ServiceName
	userID             uint64
	loginID            string
	entryName          cluster.ServiceName
	attackName         cluster.ServiceName
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry) *agent {
	agent := &agent{client: client}
	agent.commandDispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		molePushCommand: agent.push, agentReplacedCommand: agent.replaced,
	})
	agent.protocolDispatcher = protocol.MustDispatcher[gsr.CommandContext](registry, client, map[uint32]any{
		uint32(demopb.MsgId_C2S_LOGIN):     agent.onLogin,
		uint32(demopb.MsgId_C2S_ENTER):     agent.onEnter,
		uint32(demopb.MsgId_C2S_HIT):       agent.onHit,
		uint32(demopb.MsgId_C2S_SNAPSHOT):  agent.onSnapshot,
		uint32(demopb.MsgId_C2S_LEAVE):     agent.onLeave,
		uint32(demopb.MsgId_C2S_OPERATION): agent.onOperation,
	})
	return agent
}

func (agent *agent) OnStart(_ context.Context, self gsr.ServiceContext) error {
	id, err := connectionID()
	if err != nil {
		return err
	}
	agent.name = cluster.ServiceName("agent-" + id)
	return self.Node().RegisterService(agent.name, self.Self())
}

func (agent *agent) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == gate.ClientDisconnectedCommand {
		agent.disconnect(self)
		self.Exit()
		return nil, nil
	}
	return agent.commandDispatcher.Handle(self, command, args...)
}

func (agent *agent) HandleClientMessage(self gsr.CommandContext, frame []byte) error {
	return agent.protocolDispatcher.HandleFrame(self, frame)
}

func (agent *agent) OnStop(_ context.Context) error {
	return agent.client.Close()
}

func connectionID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
