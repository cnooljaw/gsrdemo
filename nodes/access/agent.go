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
)

// agent 独占当前连接身份和路由，不持有权威游戏状态或钱包。
type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
	commandDispatcher  *gsr.Dispatcher
	session            agentSession
	mole               moleAgent
	landlord           landlordAgent
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry) *agent {
	agent := &agent{client: client}
	agent.mole = moleAgent{identity: agent.session.snapshot, allowEnter: agent.allowEnter}
	agent.landlord = landlordAgent{identity: agent.session.snapshot, allowEnter: agent.allowEnter}
	agent.commandDispatcher = gsr.MustDispatcher(mustMergeHandlers(
		agent.commonCommandHandlers(),
		agent.mole.commandHandlers(),
		agent.landlord.commandHandlers(),
	))
	agent.protocolDispatcher = protocol.MustDispatcher[gsr.CommandContext](registry, client, mustMergeHandlers(
		agent.commonProtocolHandlers(),
		agent.mole.protocolHandlers(),
		agent.landlord.protocolHandlers(),
	))
	agent.mole.push = agent.protocolDispatcher.Push
	agent.landlord.push = agent.protocolDispatcher.Push
	return agent
}

func (agent *agent) OnStart(_ context.Context, self gsr.ServiceContext) error {
	id, err := connectionID()
	if err != nil {
		return err
	}
	agent.session.name = cluster.ServiceName("agent-" + id)
	return self.Node().RegisterService(agent.session.name, self.Self())
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

// allowEnter 由连接 owner 统一协调进入互斥，不在组件间保存彼此的指针。
func (agent *agent) allowEnter(game string) error {
	if err := agent.session.requireLogin(); err != nil {
		return err
	}
	if game != "mole" && agent.mole.entered() {
		return errEnterRequired
	}
	if game != "landlord" && agent.landlord.entered() {
		return errEnterRequired
	}
	return nil
}
