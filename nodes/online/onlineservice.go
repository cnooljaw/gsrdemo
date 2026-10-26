package main

import (
	"context"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	// onlineServiceName 是 online 节点内的位置目录名称。
	onlineServiceName = "online"
	// bindCommand 接收 UserID、LoginID、AccessNode、AgentName，返回被替换的位置或 nil。
	bindCommand runtime.CommandID = 1
	// lookupCommand 接收 UserID，返回当前的位置快照或 nil。
	lookupCommand runtime.CommandID = 2
	// unbindCommand 接收 UserID、LoginID，返回是否删除了匹配的绑定。
	unbindCommand runtime.CommandID = 3
)

var (
	errInvalidOnlineInput = runtime.NewStatusError(runtime.ErrorClassDomain, "online.invalid_request", "invalid online input")
	errLoginIDConflict    = runtime.NewStatusError(runtime.ErrorClassDomain, "online.login_id_conflict", "login ID already bound to another connection")
)

// onlineLocation 是一条连接的位置；AgentName 仅在 AccessNode 内唯一。
type onlineLocation struct {
	LoginID    string
	AccessNode string
	AgentName  string
}

func (location onlineLocation) snapshot() map[string]any {
	return map[string]any{
		"login_id":    location.LoginID,
		"access_node": location.AccessNode,
		"agent_name":  location.AgentName,
	}
}

// onlineService 独占在线位置目录；不拥有连接、账号、游戏状态或持久化资源。
type onlineService struct {
	locations  map[uint64]onlineLocation
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*onlineService)(nil)

func newOnlineService() *onlineService {
	service := &onlineService{locations: make(map[uint64]onlineLocation)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		bindCommand:   service.bind,
		lookupCommand: service.lookup,
		unbindCommand: service.unbind,
	})
	return service
}

func (service *onlineService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	return nil
}

func (service *onlineService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *onlineService) bind(_ gsr.CommandContext, userID uint64, loginID, accessNode, agentName string) (any, error) {
	if userID == 0 || !validOnlineIdentifier(loginID) || !validOnlineIdentifier(accessNode) || !validOnlineIdentifier(agentName) {
		return nil, errInvalidOnlineInput
	}
	location := onlineLocation{LoginID: loginID, AccessNode: accessNode, AgentName: agentName}
	previous, exists := service.locations[userID]
	if exists && previous.LoginID == loginID {
		if previous != location {
			return nil, errLoginIDConflict
		}
		// 当前连接的重复提交没有替换任何连接，不能返回自身供调用方踢除。
		return nil, nil
	}
	service.locations[userID] = location
	if !exists {
		return nil, nil
	}
	return previous.snapshot(), nil
}

func (service *onlineService) lookup(_ gsr.CommandContext, userID uint64) (any, error) {
	if userID == 0 {
		return nil, errInvalidOnlineInput
	}
	location, exists := service.locations[userID]
	if !exists {
		return nil, nil
	}
	return location.snapshot(), nil
}

func (service *onlineService) unbind(_ gsr.CommandContext, userID uint64, loginID string) (bool, error) {
	if userID == 0 || !validOnlineIdentifier(loginID) {
		return false, errInvalidOnlineInput
	}
	location, exists := service.locations[userID]
	if !exists || location.LoginID != loginID {
		return false, nil
	}
	delete(service.locations, userID)
	return true, nil
}

func (service *onlineService) OnStop(_ context.Context) error {
	service.locations = nil
	return nil
}

// validOnlineIdentifier 接受 1..128 字节可见 ASCII，不裁剪或规范化连接地址。
func validOnlineIdentifier(identifier string) bool {
	if len(identifier) == 0 || len(identifier) > 128 {
		return false
	}
	for index := range identifier {
		if identifier[index] < 0x21 || identifier[index] > 0x7e {
			return false
		}
	}
	return true
}
