package gameagent

import (
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"google.golang.org/protobuf/proto"
)

// Component 是连接内游戏接入组件的最小契约，由所属 Agent 串行调用。
// 注册表在构造时提供；Disconnect 报错不会阻止其他组件及 Online 的清理。
type Component interface {
	Name() string
	ProtocolHandlers() map[uint32]any
	CommandHandlers() map[runtime.CommandID]any
	Entered() bool
	Disconnect(gsr.ServiceContext) error
}

// Identity 是当前连接身份的独立值快照，唯一写入者仍是所属 Agent。
type Identity struct {
	UserID     uint64
	LoginID    string
	AccessNode cluster.NodeID
	AgentName  cluster.ServiceName
}

// RequireLogin 拒绝未登录的游戏请求。
func (identity Identity) RequireLogin() error {
	if identity.UserID == 0 {
		return ErrLoginRequired
	}
	return nil
}

// Dependencies 只注入组件实际需要的能力，构造后不能变更。
// Call 必须有限等待；self 仅在当前回调中使用，不能由依赖保存或异步使用。
// Identity 每次读取当前快照，AllowEnter 由连接 owner 统一判断互斥。
type Dependencies struct {
	Identity   func() Identity
	AllowEnter func(string) error
	Call       func(self gsr.ServiceContext, node cluster.NodeID, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error)
	// SettlementNode 是配置中的目标节点名；服务名和命令由公开协议规定。
	SettlementNode cluster.NodeID
	Push           func(uint32, proto.Message) error
}

var (
	// ErrLoginRequired 表示当前连接尚未登录。
	ErrLoginRequired = runtime.NewStatusError(runtime.ErrorClassDomain, "access.login_required", "login required")
	// ErrEnterRequired 表示缺少所需游戏路由，或需要先离开当前游戏。
	ErrEnterRequired = runtime.NewStatusError(runtime.ErrorClassDomain, "access.enter_required", "enter mole first")
	// ErrInvalidBackendReply 表示内部响应不满足当前协议契约。
	ErrInvalidBackendReply = runtime.NewStatusError(runtime.ErrorClassDomain, "access.invalid_backend_reply", "invalid backend reply")
)
