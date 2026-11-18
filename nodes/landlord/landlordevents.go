package main

import (
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
)

// broadcast 为一桌的同一事件分配一个序号；个别消息按接收者生成私有字段。
func (service *attackService) broadcast(self gsr.ServiceContext, command runtime.CommandID, makePayload func(landlordPlayer) map[string]any) {
	service.revision++
	serverMS := time.Now().UnixMilli()
	for _, player := range service.game.Players {
		if player.UserID == 0 || player.AccessNode == "" {
			continue
		}
		payload := makePayload(player)
		payload["room_name"], payload["revision"] = service.game.Name, service.revision
		payload["server_ms"] = serverMS
		if err := self.Cluster().Send(cluster.NodeID(player.AccessNode), cluster.ServiceName(player.AgentName), command, player.LoginID, payload); err != nil {
			_ = self.Logger().Error("斗地主事件未接纳，客户端可恢复场景", "error", err.Error())
		}
	}
}

func (service *attackService) pushSeat(self gsr.ServiceContext, uid uint64, seat int, joined, ready bool) {
	service.broadcast(self, accessagent.LandlordSeatPushCommand, func(landlordPlayer) map[string]any {
		return map[string]any{"user_id": uid, "seat": int64(seat), "joined": joined, "ready": ready}
	})
}

func (service *attackService) pushReady(self gsr.ServiceContext, uid uint64, seat int) {
	service.broadcast(self, accessagent.LandlordReadyPushCommand, func(landlordPlayer) map[string]any {
		return map[string]any{"user_id": uid, "seat": int64(seat), "ready": true}
	})
}

func (service *attackService) pushTrustee(self gsr.ServiceContext, uid uint64, enabled bool) {
	service.broadcast(self, accessagent.LandlordTrusteePushCommand, func(landlordPlayer) map[string]any {
		return map[string]any{"user_id": uid, "trustee": enabled, "deadline_ms": landlordDeadline(service.game), "start_ms": landlordStart(service.game)}
	})
}

// pushTurn 通知全桌动画结束，服务器已开放当前轮次。
func (service *attackService) pushTurn(self gsr.ServiceContext) {
	service.broadcast(self, accessagent.LandlordTurnPushCommand, func(landlordPlayer) map[string]any {
		return map[string]any{"phase": service.game.Phase, "turn": service.game.Turn, "turn_seat": int64(service.game.Seat), "deadline_ms": landlordDeadline(service.game)}
	})
}

func (service *attackService) pushDeal(self gsr.ServiceContext) {
	service.broadcast(self, accessagent.LandlordDealPushCommand, func(player landlordPlayer) map[string]any {
		return landlordDealEvent(service.game, player.UserID)
	})
}

func (service *attackService) pushBid(self gsr.ServiceContext, before attackGame, uid uint64, yes bool) {
	game := service.game
	redealt := game.Redeals > before.Redeals
	service.broadcast(self, accessagent.LandlordBidPushCommand, func(landlordPlayer) map[string]any {
		var bottom []int64
		if game.Landlord >= 0 {
			bottom = game.Bottom
		}
		return map[string]any{"user_id": uid, "rob": before.Phase == "robbing", "yes": yes,
			"phase": game.Phase, "turn": game.Turn, "turn_seat": int64(game.Seat),
			"deadline_ms": landlordDeadline(game), "start_ms": landlordStart(game), "can_act": game.canAct(), "candidate_seat": int64(game.Candidate),
			"landlord_seat": int64(game.Landlord), "multiplier": game.Multiplier,
			"bottom": cardList(bottom), "redealt": redealt}
	})
	if redealt {
		service.pushDeal(self)
	}
}

func (service *attackService) pushPlay(self gsr.ServiceContext, uid uint64, cards []int64) {
	service.broadcast(self, accessagent.LandlordPlayPushCommand, func(landlordPlayer) map[string]any {
		return landlordPlayEvent(service.game, uid, cards)
	})
}

func (service *attackService) pushSettlement(self gsr.ServiceContext) {
	service.broadcast(self, accessagent.LandlordSettlementPushCommand, func(player landlordPlayer) map[string]any {
		return map[string]any{"settlement": service.settlement.snapshotFor(player.UserID)}
	})
}

// landlordPlayEvent 描述一次已接受的出牌或不出，只包含本桌公开信息。
func landlordPlayEvent(game attackGame, userID uint64, cards []int64) map[string]any {
	var remaining int64
	for _, player := range game.Players {
		if player.UserID == userID {
			remaining = int64(len(player.Hand))
			break
		}
	}
	return map[string]any{
		"user_id": userID, "cards": cardList(cards), "remaining": remaining,
		"phase": game.Phase, "turn": game.Turn, "turn_seat": int64(game.Seat),
		"deadline_ms": landlordDeadline(game), "start_ms": landlordStart(game), "can_act": game.canAct(), "last_cards": cardList(game.Last),
		"last_seat": int64(game.LastSeat), "winner_seat": int64(game.Winner), "multiplier": game.Multiplier,
	}
}

// landlordDealEvent 为指定玩家生成发牌消息，不包含其他人的手牌。
func landlordDealEvent(game attackGame, userID uint64) map[string]any {
	var hand []int64
	for _, player := range game.Players {
		if player.UserID == userID {
			hand = player.Hand
			break
		}
	}
	return map[string]any{"hand": cardList(hand), "turn": game.Turn, "turn_seat": int64(game.Seat),
		"deadline_ms": landlordDeadline(game), "start_ms": landlordStart(game), "redeals": int64(game.Redeals)}
}

func landlordStart(game attackGame) int64 {
	if game.WaitUntil.IsZero() {
		return 0
	}
	return game.WaitUntil.UnixMilli()
}

func landlordDeadline(game attackGame) int64 {
	if game.nextDeadline().IsZero() {
		return 0
	}
	return game.nextDeadline().UnixMilli()
}
