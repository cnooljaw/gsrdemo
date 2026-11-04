package gameagent

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// Landlord 处理本游戏接入协议，只持有路由，由所属 Agent 串行调用。
type Landlord struct {
	deps Dependencies
	room cluster.ServiceName
}

var _ Component = (*Landlord)(nil)

// NewLandlord 创建每连接独立的游戏接入组件，构造时不调用注入能力。
func NewLandlord(deps Dependencies) *Landlord {
	return &Landlord{deps: deps}
}

// Name 返回用于接入互斥与日志的游戏名称。
func (landlord *Landlord) Name() string {
	return "landlord"
}

const landlordSnapshotPushCommand runtime.CommandID = 110

// Entered 从本游戏路由判断是否已进入，不另存状态。
func (landlord *Landlord) Entered() bool {
	return landlord.room != ""
}

func (landlord *Landlord) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordEnterRequest) error {
	identity := landlord.deps.Identity()
	if err := landlord.deps.AllowEnter(landlord.Name()); err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		value, err := landlord.deps.Call(self, "landlord", "landlord", 1, identity.UserID, identity.LoginID)
		if err != nil {
			return err
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return ErrInvalidBackendReply
		}
		room, roomOK := fields["room_name"].(string)
		membership, memberOK := fields["membership"].(string)
		if !roomOK || !memberOK || room == "" || membership == "" {
			return ErrInvalidBackendReply
		}
		value, err = landlord.deps.Call(self, "landlord", cluster.ServiceName(room), 1, identity.UserID, identity.LoginID, string(identity.AccessNode), string(identity.AgentName), membership)
		if err != nil {
			// 只有明确房间消失才丢弃目录；超时可能已加入，不能随机换桌。
			if errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed) || errors.Is(err, runtime.ErrServicePanicked) {
				if _, discardErr := landlord.deps.Call(self, "landlord", "landlord", 13, room); discardErr != nil {
					return discardErr
				}
				continue
			}
			return err
		}
		snapshot, err := landlordSnapshot(value)
		if err != nil {
			return err
		}
		landlord.room = cluster.ServiceName(room)
		return landlordResponse(reply, demopb.MsgId_S2C_LANDLORD_ENTER, snapshot)
	}
	return runtime.ErrServiceNotFound
}

func (landlord *Landlord) callRoom(self gsr.CommandContext, reply protocol.Context, command runtime.CommandID, responseID demopb.MsgId, args ...any) error {
	identity := landlord.deps.Identity()
	if err := identity.RequireLogin(); err != nil {
		return err
	}
	if landlord.room == "" {
		return ErrEnterRequired
	}
	values := append([]any{identity.UserID, identity.LoginID}, args...)
	value, err := landlord.deps.Call(self, "landlord", landlord.room, command, values...)
	if err != nil {
		return err
	}
	snapshot, err := landlordSnapshot(value)
	if err != nil {
		return err
	}
	return landlordResponse(reply, responseID, snapshot)
}

func (landlord *Landlord) onReady(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordReadyRequest) error {
	return landlord.callRoom(self, reply, 2, demopb.MsgId_S2C_LANDLORD_READY)
}

func (landlord *Landlord) onPlay(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordPlayRequest) error {
	cards := make([]any, len(request.Cards))
	for i, c := range request.Cards {
		cards[i] = c
	}
	return landlord.callRoom(self, reply, 3, demopb.MsgId_S2C_LANDLORD_PLAY, request.Turn, cards)
}

func (landlord *Landlord) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordSnapshotRequest) error {
	return landlord.callRoom(self, reply, 4, demopb.MsgId_S2C_LANDLORD_SNAPSHOT)
}

func (landlord *Landlord) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordLeaveRequest) error {
	identity := landlord.deps.Identity()
	if err := identity.RequireLogin(); err != nil {
		return err
	}
	if landlord.room == "" {
		return ErrEnterRequired
	}
	value, err := landlord.deps.Call(self, "landlord", landlord.room, 5, identity.UserID, identity.LoginID)
	if err != nil {
		return err
	}
	if value != true {
		return ErrInvalidBackendReply
	}
	landlord.room = ""
	return reply.Response(uint32(demopb.MsgId_S2C_LANDLORD_LEAVE), &demopb.LandlordLeaveResponse{})
}

func landlordSnapshot(value any) (*demopb.LandlordSnapshot, error) {
	snapshot := &demopb.LandlordSnapshot{}
	if err := clientMessage(value, snapshot); err != nil {
		return nil, err
	}
	if snapshot.RoomName == "" {
		return nil, ErrInvalidBackendReply
	}
	return snapshot, nil
}

func (landlord *Landlord) handleSnapshotPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	identity := landlord.deps.Identity()
	if identity.UserID == 0 || login != identity.LoginID || landlord.room == "" {
		return nil
	}
	snapshot, err := landlordSnapshot(payload)
	if err != nil {
		return err
	}
	if cluster.ServiceName(snapshot.RoomName) != landlord.room {
		return nil
	}
	return landlord.deps.Push(uint32(demopb.MsgId_S2C_LANDLORD_PUSH), &demopb.LandlordPush{Snapshot: snapshot})
}

func landlordResponse(reply protocol.Context, id demopb.MsgId, snapshot *demopb.LandlordSnapshot) error {
	switch id {
	case demopb.MsgId_S2C_LANDLORD_ENTER:
		return reply.Response(uint32(id), &demopb.LandlordEnterResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_READY:
		return reply.Response(uint32(id), &demopb.LandlordReadyResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_PLAY:
		return reply.Response(uint32(id), &demopb.LandlordPlayResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_SNAPSHOT:
		return reply.Response(uint32(id), &demopb.LandlordSnapshotResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_CALL:
		return reply.Response(uint32(id), &demopb.LandlordCallResponse{Snapshot: snapshot})
	case demopb.MsgId_S2C_LANDLORD_ROB:
		return reply.Response(uint32(id), &demopb.LandlordRobResponse{Snapshot: snapshot})
	}
	return ErrInvalidBackendReply
}

func (landlord *Landlord) onCall(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordCallRequest) error {
	return landlord.callRoom(self, reply, 6, demopb.MsgId_S2C_LANDLORD_CALL, request.Turn, request.Yes)
}

func (landlord *Landlord) onRob(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordRobRequest) error {
	return landlord.callRoom(self, reply, 7, demopb.MsgId_S2C_LANDLORD_ROB, request.Turn, request.Yes)
}

// ProtocolHandlers 返回本游戏的客户端协议入口。
func (landlord *Landlord) ProtocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_LANDLORD_ENTER):    landlord.onEnter,
		uint32(demopb.MsgId_C2S_LANDLORD_READY):    landlord.onReady,
		uint32(demopb.MsgId_C2S_LANDLORD_CALL):     landlord.onCall,
		uint32(demopb.MsgId_C2S_LANDLORD_ROB):      landlord.onRob,
		uint32(demopb.MsgId_C2S_LANDLORD_PLAY):     landlord.onPlay,
		uint32(demopb.MsgId_C2S_LANDLORD_SNAPSHOT): landlord.onSnapshot,
		uint32(demopb.MsgId_C2S_LANDLORD_LEAVE):    landlord.onLeave,
	}
}

// CommandHandlers 返回本游戏的节点推送入口。
func (landlord *Landlord) CommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		landlordSnapshotPushCommand: landlord.handleSnapshotPush,
	}
}

// Disconnect 尽力离开并清空路由；失败由所属 Agent 记录，组件不关闭连接。
func (landlord *Landlord) Disconnect(self gsr.ServiceContext) error {
	defer func() { landlord.room = "" }()
	if landlord.room == "" {
		return nil
	}
	identity := landlord.deps.Identity()
	_, err := landlord.deps.Call(self, "landlord", landlord.room, 5, identity.UserID, identity.LoginID)
	return err
}
