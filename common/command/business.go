package command

import (
	"context"
	"fmt"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"google.golang.org/protobuf/proto"
)

// 业务 CommandID 与客户端 msg_id 使用独立编号空间。
const (
	Authenticate runtime.CommandID = 10 + iota
	Position
	Disconnect
	AllocateRoom
	ReserveSeat
	BindRoom
	RemoveMember
	RoomSnapshot
	Hit
	RoomState
	RoomPhase
	Replaced
	Advance
	Sweep
)

// Call 在跨节点边界显式编码和恢复 Protobuf，不让业务结构体进入 Cluster codec。
func Call(ctx context.Context, self gsr.ServiceContext, node, name string, id runtime.CommandID, request, response proto.Message) error {
	payload, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	reply, err := self.Cluster().Call(ctx, cluster.NodeID(node), cluster.ServiceName(name), id, payload)
	if err != nil {
		return err
	}
	encoded, ok := reply.([]byte)
	if !ok {
		return fmt.Errorf("%s/%s command %d returned %T", node, name, id, reply)
	}
	return proto.Unmarshal(encoded, response)
}

// Send 成功只表示通知被接纳，接收者仍按绑定和版本过滤。
func Send(self gsr.ServiceContext, node, name string, id runtime.CommandID, message proto.Message) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	return self.Cluster().Send(cluster.NodeID(node), cluster.ServiceName(name), id, payload)
}
