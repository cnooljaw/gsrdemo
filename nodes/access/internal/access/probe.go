package access

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsrdemo/common/command"
	diagnosticpb "github.com/cnooljaw/gsrdemo/common/gen/demo/diagnostic/v1"
)

func (service *agent) handleProbe(self gsr.CommandContext, client protocol.Context, request *diagnosticpb.ProbeRequest) error {
	response := &diagnosticpb.ProbeResponse{OperationId: request.GetOperationId(), Text: request.GetText(), Code: "ok"}
	if !service.enableProbe {
		response.Code = "disabled"
		return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
	}
	if request.GetOperationId() == "" || len(request.GetOperationId()) > 64 || request.GetText() == "" || len(request.GetText()) > 256 {
		response.Code = "invalid"
		return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
	}
	if err := service.register(self); err != nil {
		return err
	}
	// 总预算约束整条调用链，不为每个节点重新分配完整预算。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, target := range []string{"login", "online", "mole", "settlement"} {
		result, err := self.Cluster().Call(ctx, cluster.NodeID(target), cluster.ServiceName(target), command.Probe, request.OperationId, request.Text, "access", string(service.name), service.token)
		if err != nil {
			response.Code = "unavailable"
			response.FailedNode = target
			_ = self.Logger().Error("probe unavailable", "node", target, "operation_id", request.GetOperationId(), "error", err)
			return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
		}
		reply, ok := result.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected %s probe result: %T", target, result)
		}
		if reply["node"] != target || reply["operation_id"] != request.OperationId || reply["text"] != request.Text {
			return errors.New("probe result mismatch")
		}
		response.Nodes = append(response.Nodes, target)
	}
	return client.Response(uint32(diagnosticpb.MsgId_S2C_PROBE_RESPONSE), response)
}

func (service *agent) handlePush(self gsr.CommandContext, token, operationID, text, node string) error {
	if token != service.token || service.token == "" {
		return nil
	}
	push := &diagnosticpb.ProbePush{OperationId: operationID, Text: text, Node: node}
	if err := service.protocolDispatcher.Push(uint32(diagnosticpb.MsgId_S2C_PROBE_PUSH), push); err != nil {
		service.disconnect(self)
		self.Exit()
		return errors.Join(err, service.client.Close())
	}
	return nil
}
