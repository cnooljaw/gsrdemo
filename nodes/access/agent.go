package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// Agent 的全部业务命令属于同一个 Service；连接实例名称由运行时生成。
//
//rpc:service accessagent public
const (
	moleSnapshotPushCommand          runtime.CommandID = 100
	replacedCommand                  runtime.CommandID = 101
	moleBalanceCorrectionPushCommand runtime.CommandID = 102
	landlordSnapshotPushCommand      runtime.CommandID = 110
)

// agent 独占当前连接身份和路由，不持有权威游戏状态或钱包。
type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
	commandDispatcher  *gsr.Dispatcher
	session            agentSession
	games              []gameComponent
	closing            bool
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry) *agent {
	agent := &agent{client: client}
	deps := componentDependencies{
		Identity:   agent.session.snapshot,
		AllowEnter: agent.allowEnter,
		Call:       callBackend,
		Push:       agent.push,
	}
	// 新增游戏的接入接线只需在此追加一项；其他入口均遍历此清单。
	agent.games = []gameComponent{
		newComMole(deps),
		newComLandlord(deps),
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
	if agent.closing {
		return nil
	}
	err := agent.protocolDispatcher.HandleFrame(self, frame)
	if err == nil {
		return nil
	}
	var envelope commonv1.Envelope
	loginRequest := proto.Unmarshal(frame, &envelope) == nil &&
		(envelope.MsgId == uint32(demopb.MsgId_C2S_LOGIN) || envelope.MsgId == uint32(demopb.MsgId_C2S_PASSWORD_AUTH))
	if loginRequest || errors.Is(err, errInvalidBackendReply) {
		agent.closeAfterFailure(self)
	}
	return err
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
			return errEnterRequired
		}
	}
	return nil
}

// push 在组件被调用时使用已完成组装的同一连接 Dispatcher。
func (agent *agent) push(id uint32, message proto.Message) error {
	return agent.protocolDispatcher.Push(id, message)
}
