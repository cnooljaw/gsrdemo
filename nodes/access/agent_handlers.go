package main

import (
	"fmt"
	"strings"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
)

// mustMergeHandlers 只用于静态接线；不同协议组的重复 ID 必须在构造 Dispatcher 前暴露。
func mustMergeHandlers[ID ~uint32](groups ...map[ID]any) map[ID]any {
	handlers := make(map[ID]any)
	for _, group := range groups {
		for id, handler := range group {
			if _, exists := handlers[id]; exists {
				panic(fmt.Sprintf("access: duplicate handler ID %d", id))
			}
			handlers[id] = handler
		}
	}
	return handlers
}

// mustBindHandlers 只在连接构造时接线，名称和编号冲突属于静态编程错误。
func (agent *agent) mustBindHandlers(registry *protocol.Registry) {
	names := make(map[string]bool, len(agent.games))
	commands := []map[runtime.CommandID]any{agent.commonCommandHandlers()}
	protocols := []map[uint32]any{agent.commonProtocolHandlers()}
	for _, game := range agent.games {
		name := game.Name()
		if strings.TrimSpace(name) == "" || names[name] {
			panic(fmt.Sprintf("access: empty or duplicate game name %q", name))
		}
		names[name] = true
		commands = append(commands, game.CommandHandlers())
		protocols = append(protocols, game.ProtocolHandlers())
	}
	commandHandlers := mustMergeHandlers(commands...)
	if _, exists := commandHandlers[gate.ClientDisconnectedCommand]; exists {
		panic(fmt.Sprintf("access: reserved command ID %d", gate.ClientDisconnectedCommand))
	}
	agent.commandDispatcher = gsr.MustDispatcher(commandHandlers)
	agent.protocolDispatcher = protocol.MustDispatcher[gsr.CommandContext](
		registry, agent.client, mustMergeHandlers(protocols...),
		protocol.WithErrorPolicy(agent.clientErrorPolicy),
	)
}
