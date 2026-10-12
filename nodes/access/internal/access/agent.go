package access

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/command"
	diagnosticpb "github.com/cnooljaw/gsrdemo/common/gen/demo/diagnostic/v1"
	"google.golang.org/protobuf/proto"
)

type agent struct {
	client             gate.ClientSession
	protocolDispatcher *protocol.Dispatcher[gsr.CommandContext]
	commandDispatcher  *gsr.Dispatcher
	name               cluster.ServiceName
	token              string
}

var (
	_ gsr.Service              = (*agent)(nil)
	_ gsr.ClientMessageHandler = (*agent)(nil)
)

func newAgent(client gate.ClientSession, registry *protocol.Registry) *agent {
	service := &agent{client: client}
	service.commandDispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{command.ProbePush: service.handlePush})
	service.protocolDispatcher = protocol.MustDispatcher[gsr.CommandContext](registry, client, map[uint32]any{
		uint32(diagnosticpb.MsgId_C2S_PROBE_REQUEST): service.handleProbe,
	})
	return service
}

func (service *agent) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

// Handle 与客户端入口共用 Mailbox；Push 不复用旧客户端请求上下文。
func (service *agent) Handle(self gsr.CommandContext, id runtime.CommandID, args ...any) (any, error) {
	if id == gate.ClientDisconnectedCommand {
		self.Exit()
		return nil, nil
	}
	return service.commandDispatcher.Handle(self, id, args...)
}

func (service *agent) HandleClientMessage(self gsr.CommandContext, frame []byte) error {
	return service.protocolDispatcher.HandleFrame(self, frame)
}

func (service *agent) handleProbe(self gsr.CommandContext, client protocol.Context, request *diagnosticpb.ProbeRequest) error {
	response := &diagnosticpb.ProbeResponse{OperationId: request.GetOperationId(), Text: request.GetText(), Code: "ok"}
	if request.GetOperationId() == "" || len(request.GetOperationId()) > 64 || request.GetText() == "" || len(request.GetText()) > 256 {
		response.Code = "invalid"
		return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
	}
	if err := service.register(self); err != nil {
		return err
	}
	payload, err := proto.Marshal(&diagnosticpb.NodeProbeRequest{
		Probe: request, AccessNode: "access", AgentName: string(service.name), AgentToken: service.token,
	})
	if err != nil {
		return err
	}
	// 总预算约束整条调用链，不为每个节点重新分配完整预算。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, target := range []string{"login", "online", "mole", "settlement"} {
		result, err := self.Cluster().Call(ctx, cluster.NodeID(target), cluster.ServiceName(target), command.Probe, payload)
		if err != nil {
			response.Code = "unavailable"
			response.FailedNode = target
			_ = self.Logger().Error("probe unavailable", "node", target, "operation_id", request.GetOperationId(), "error", err)
			return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
		}
		encoded, ok := result.([]byte)
		if !ok {
			return fmt.Errorf("unexpected %s probe result: %T", target, result)
		}
		reply := &diagnosticpb.NodeProbeResponse{}
		if err := proto.Unmarshal(encoded, reply); err != nil {
			return err
		}
		if reply.GetNode() != target || reply.GetOperationId() != request.GetOperationId() || reply.GetText() != request.GetText() {
			return errors.New("probe result mismatch")
		}
		response.Nodes = append(response.Nodes, target)
	}
	return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
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
	name := cluster.ServiceName("probe-agent/" + value)
	// 只在 Running Handler 中注册，名字与令牌不跨连接复用。
	if err := self.Register(name, self.Self()); err != nil {
		return err
	}
	service.name = name
	service.token = value
	return nil
}

func (service *agent) handlePush(self gsr.CommandContext, payload []byte) error {
	request := &diagnosticpb.AgentProbePush{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return err
	}
	if request.GetAgentToken() != service.token || service.token == "" {
		return nil
	}
	if request.GetPush() == nil {
		return errors.New("empty probe push")
	}
	if err := service.protocolDispatcher.Push(uint32(diagnosticpb.MsgId_S2C_PROBE_PUSH), request.GetPush()); err != nil {
		self.Exit()
		return errors.Join(err, service.client.Close())
	}
	return nil
}

func (service *agent) OnStop(_ context.Context) error {
	return service.client.Close()
}
