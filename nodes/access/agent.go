package main

import (
	"context"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
)

// agent 只组合当前连接能力与协议分发，不保存业务状态或 Handler 上下文。
type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry) *agent {
	return &agent{
		client:             client,
		protocolDispatcher: protocol.MustDispatcher[gsr.CommandContext](registry, client, nil),
	}
}

func (agent *agent) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

func (agent *agent) Handle(self gsr.CommandContext, command runtime.CommandID, _ ...any) (any, error) {
	if command == gate.ClientDisconnectedCommand {
		self.Exit()
		return nil, nil
	}
	return nil, runtime.ErrUnknownCommand
}

func (agent *agent) HandleClientMessage(self gsr.CommandContext, frame []byte) error {
	return agent.protocolDispatcher.HandleFrame(self, frame)
}

func (agent *agent) OnStop(_ context.Context) error {
	return agent.client.Close()
}
