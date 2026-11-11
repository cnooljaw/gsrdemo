package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	landlordrpc "github.com/cnooljaw/gsrdemo/common/rpc/landlord"
	"github.com/cnooljaw/gsrdemo/common/rpc/landlordattack"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// comLandlord 处理本游戏接入协议，只持有路由，由所属 Agent 串行调用。
type comLandlord struct {
	deps componentDependencies
	room cluster.ServiceName
}

var _ gameComponent = (*comLandlord)(nil)

// newComLandlord 创建每连接独立的游戏接入组件，构造时不调用注入能力。
func newComLandlord(deps componentDependencies) *comLandlord {
	return &comLandlord{deps: deps}
}

// Name 返回用于接入互斥与日志的游戏名称。
func (landlord *comLandlord) Name() string {
	return "landlord"
}

// Entered 从本游戏路由判断是否已进入，不另存状态。
func (landlord *comLandlord) Entered() bool {
	return landlord.room != ""
}

func (landlord *comLandlord) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordEnterRequest) error {
	identity := landlord.deps.Identity()
	if err := landlord.deps.AllowEnter(landlord.Name()); err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		value, err := landlord.deps.Call(self, "landlord", landlordrpc.ServiceName, landlordrpc.ResolveCommand, identity.UserID, identity.LoginID)
		if err != nil {
			return err
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return errInvalidBackendReply
		}
		room, roomOK := fields["room_name"].(string)
		membership, memberOK := fields["membership"].(string)
		if !roomOK || !memberOK || room == "" || membership == "" {
			return errInvalidBackendReply
		}
		value, err = landlord.deps.Call(self, "landlord", cluster.ServiceName(room), landlordattack.RoomJoinCommand, identity.UserID, identity.LoginID, string(identity.AccessNode), string(identity.AgentName), membership)
		if err != nil {
			// 只有明确房间消失才丢弃目录；超时可能已加入，不能随机换桌。
			if errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed) || errors.Is(err, runtime.ErrServicePanicked) {
				if _, discardErr := landlord.deps.Call(self, "landlord", landlordrpc.ServiceName, landlordrpc.DiscardCommand, room); discardErr != nil {
					return discardErr
				}
				continue
			}
			return err
		}
		// Join 已返回，快照非法也必须保留已知目标，供断线时尝试 Leave。
		landlord.room = cluster.ServiceName(room)
		snapshot, err := landlordSnapshot(value)
		if err != nil {
			return err
		}
		if snapshot.RoomName != room {
			return errInvalidBackendReply
		}
		return landlordResponse(reply, demopb.MsgId_S2C_LANDLORD_ENTER, snapshot)
	}
	return runtime.ErrServiceNotFound
}

func (landlord *comLandlord) callRoom(self gsr.CommandContext, reply protocol.Context, command runtime.CommandID, responseID demopb.MsgId, args ...any) error {
	identity := landlord.deps.Identity()
	if err := identity.RequireLogin(); err != nil {
		return err
	}
	if landlord.room == "" {
		return errEnterRequired
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
	if cluster.ServiceName(snapshot.RoomName) != landlord.room {
		return errInvalidBackendReply
	}
	return landlordResponse(reply, responseID, snapshot)
}

func (landlord *comLandlord) onReady(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordReadyRequest) error {
	return landlord.callRoom(self, reply, landlordattack.RoomReadyCommand, demopb.MsgId_S2C_LANDLORD_READY)
}

func (landlord *comLandlord) onPlay(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordPlayRequest) error {
	cards := make([]any, len(request.Cards))
	for i, c := range request.Cards {
		cards[i] = c
	}
	return landlord.callRoom(self, reply, landlordattack.RoomPlayCommand, demopb.MsgId_S2C_LANDLORD_PLAY, request.Turn, cards)
}

func (landlord *comLandlord) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordSnapshotRequest) error {
	return landlord.callRoom(self, reply, landlordattack.RoomSnapshotCommand, demopb.MsgId_S2C_LANDLORD_SNAPSHOT)
}

func (landlord *comLandlord) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.LandlordLeaveRequest) error {
	identity := landlord.deps.Identity()
	if err := identity.RequireLogin(); err != nil {
		return err
	}
	if landlord.room == "" {
		return errEnterRequired
	}
	value, err := landlord.deps.Call(self, "landlord", landlord.room, landlordattack.RoomLeaveCommand, identity.UserID, identity.LoginID)
	if err != nil {
		return err
	}
	if value != true {
		return errInvalidBackendReply
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
		return nil, errInvalidBackendReply
	}
	return snapshot, nil
}

func (landlord *comLandlord) handleSnapshotPush(_ gsr.CommandContext, login string, payload map[string]any) error {
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
	return errInvalidBackendReply
}

func (landlord *comLandlord) onCall(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordCallRequest) error {
	return landlord.callRoom(self, reply, landlordattack.RoomBidCommand, demopb.MsgId_S2C_LANDLORD_CALL, request.Turn, request.Yes)
}

func (landlord *comLandlord) onRob(self gsr.CommandContext, reply protocol.Context, request *demopb.LandlordRobRequest) error {
	return landlord.callRoom(self, reply, landlordattack.RoomRobCommand, demopb.MsgId_S2C_LANDLORD_ROB, request.Turn, request.Yes)
}

// ProtocolHandlers 返回本游戏的客户端协议入口。
func (landlord *comLandlord) ProtocolHandlers() map[uint32]any {
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
func (landlord *comLandlord) CommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		landlordSnapshotPushCommand: landlord.handleSnapshotPush,
	}
}

// Disconnect 尽力离开并清空路由；失败由所属 Agent 记录，组件不关闭连接。
func (landlord *comLandlord) Disconnect(self gsr.ServiceContext) error {
	defer func() { landlord.room = "" }()
	if landlord.room == "" {
		return nil
	}
	identity := landlord.deps.Identity()
	_, err := landlord.deps.Call(self, "landlord", landlord.room, landlordattack.RoomLeaveCommand, identity.UserID, identity.LoginID)
	return err
}
