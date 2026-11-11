package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const agentCallTimeout = 3 * time.Second

// agentSession 只由 Agent 更新；组件读取独立值快照，不缓存另一份当前身份。
type agentSession struct {
	node    cluster.NodeID
	name    cluster.ServiceName
	userID  uint64
	loginID string
}

func (session *agentSession) snapshot() agentIdentity {
	return agentIdentity{
		UserID: session.userID, LoginID: session.loginID,
		AccessNode: session.node, AgentName: session.name,
	}
}

func callBackend(self gsr.ServiceContext, node cluster.NodeID, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), agentCallTimeout)
	defer cancel()
	return self.Cluster().Call(ctx, node, name, command, args...)
}
