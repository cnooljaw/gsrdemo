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
	"github.com/cnooljaw/gsrdemo/nodes/access/gameagent"
	"google.golang.org/protobuf/proto"
)

// agent 独占当前连接身份和路由，不持有权威游戏状态或钱包。
type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
	commandDispatcher  *gsr.Dispatcher
	session            agentSession
	games              []gameagent.Component
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry) *agent {
	agent := &agent{client: client}
	deps := gameagent.Dependencies{
		Identity:   agent.session.snapshot,
		AllowEnter: agent.allowEnter,
		Call:       callBackend,
		Push:       agent.push,
	}
	// 新增游戏的接入接线只需在此追加一项；其他入口均遍历此清单。
	agent.games = []gameagent.Component{
		gameagent.NewMole(deps),
		gameagent.NewLandlord(deps),
	}
	agent.mustBindHandlers(registry)
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
	if err := agent.session.snapshot().RequireLogin(); err != nil {
		return err
	}
	for _, candidate := range agent.games {
		if candidate.Name() != game && candidate.Entered() {
			return gameagent.ErrEnterRequired
		}
	}
	return nil
}

// push 在组件被调用时使用已完成组装的同一连接 Dispatcher。
func (agent *agent) push(id uint32, message proto.Message) error {
	return agent.protocolDispatcher.Push(id, message)
}
