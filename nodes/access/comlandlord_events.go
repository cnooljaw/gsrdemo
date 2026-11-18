package main

import (
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// pushEvent 按连接身份和牌桌路由过滤迟到事件，再将规范内部值转换为客户端消息。
func (landlord *comLandlord) pushEvent(login string, payload map[string]any, id demopb.MsgId, message proto.Message) error {
	identity := landlord.deps.Identity()
	if identity.UserID == 0 || login != identity.LoginID || landlord.room == "" {
		return nil
	}
	room, roomOK := payload["room_name"].(string)
	revision, revisionOK := payload["revision"].(uint64)
	if !roomOK || room == "" || !revisionOK || revision == 0 {
		return errInvalidBackendReply
	}
	if cluster.ServiceName(room) != landlord.room {
		return nil
	}
	switch message.(type) {
	case *demopb.LandlordSeatPush, *demopb.LandlordReadyPush, *demopb.LandlordBidPush, *demopb.LandlordPlayPush, *demopb.LandlordTrusteePush:
		if userID, ok := payload["user_id"].(uint64); !ok || userID == 0 {
			return errInvalidBackendReply
		}
	case *demopb.LandlordDealPush:
		if hand, ok := payload["hand"].([]any); !ok || len(hand) != 17 {
			return errInvalidBackendReply
		}
	case *demopb.LandlordSettlementPush:
		settlement, ok := payload["settlement"].(map[string]any)
		operationID, validID := settlement["operation_id"].(string)
		if !ok || settlement["status"] != "committed" || !validID || operationID == "" {
			return errInvalidBackendReply
		}
	case *demopb.LandlordTurnPush:
		if turn, ok := payload["turn"].(uint64); !ok || turn == 0 {
			return errInvalidBackendReply
		}
	}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	return landlord.deps.Push(uint32(id), message)
}

func (landlord *comLandlord) handleSeatPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_SEAT_PUSH, &demopb.LandlordSeatPush{})
}

func (landlord *comLandlord) handleReadyPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_READY_PUSH, &demopb.LandlordReadyPush{})
}

func (landlord *comLandlord) handleDealPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_DEAL_PUSH, &demopb.LandlordDealPush{})
}

func (landlord *comLandlord) handleBidPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_BID_PUSH, &demopb.LandlordBidPush{})
}

func (landlord *comLandlord) handlePlayPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_PLAY_PUSH, &demopb.LandlordPlayPush{})
}

func (landlord *comLandlord) handleSettlementPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_SETTLEMENT_PUSH, &demopb.LandlordSettlementPush{})
}

func (landlord *comLandlord) handleTrusteePush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_TRUSTEE_PUSH, &demopb.LandlordTrusteePush{})
}

func (landlord *comLandlord) handleTurnPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	return landlord.pushEvent(login, payload, demopb.MsgId_S2C_LANDLORD_TURN_PUSH, &demopb.LandlordTurnPush{})
}
