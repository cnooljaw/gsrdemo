package main

import (
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
)

func (service *attackService) handleSnapshot(_ gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	return service.view(uid), nil
}

func (service *attackService) view(uid uint64) map[string]any {
	view := service.game.view(uid)
	if service.result != nil {
		view["settlement"] = service.result.view(uid)
	}
	return view
}

func (service *attackService) broadcast(self gsr.ServiceContext) {
	for _, p := range service.game.Players {
		if p.UserID == 0 || p.AccessNode == "" {
			continue
		}
		if err := self.Cluster().Send(cluster.NodeID(p.AccessNode), cluster.ServiceName(p.AgentName), landlordPushCommand, p.LoginID, service.view(p.UserID)); err != nil {
			_ = self.Logger().Error("斗地主推送未接纳，客户端可查询快照", "error", err.Error())
		}
	}
}

func cardList(cards []int64) []any {
	values := make([]any, len(cards))
	for i, c := range cards {
		values[i] = c
	}
	return values
}

func (game *attackGame) view(userID uint64) map[string]any {
	players := make([]any, 0, 3)
	hand := []any{}
	seat := int64(-1)
	for i, p := range game.Players {
		if p.UserID == 0 {
			continue
		}
		players = append(players, map[string]any{"user_id": p.UserID, "seat": int64(i), "ready": p.Ready, "remaining": int64(len(p.Hand)), "call_choice": game.Calls[i], "rob_choice": game.Robs[i]})
		if p.UserID == userID {
			seat = int64(i)
			hand = cardList(p.Hand)
		}
	}
	bottom := []any{}
	if game.Landlord >= 0 {
		bottom = cardList(game.Bottom)
	}
	deadline := int64(0)
	if !game.Deadline.IsZero() {
		deadline = game.Deadline.UnixMilli()
	}
	return map[string]any{"room_name": game.Name, "phase": game.Phase, "seat": seat, "players": players, "hand": hand,
		"bottom": bottom, "last_cards": cardList(game.Last), "last_seat": int64(game.LastSeat), "turn_seat": int64(game.Seat),
		"turn": game.Turn, "deadline_ms": deadline, "landlord_seat": int64(game.Landlord), "candidate_seat": int64(game.Candidate), "multiplier": game.Multiplier, "redeals": int64(game.Redeals), "winner_seat": int64(game.Winner)}
}
