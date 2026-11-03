package main

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/cnooljaw/gsr"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
)

func (service *attackService) handleJoin(self gsr.CommandContext, uid uint64, login, node, agent, membership string) (map[string]any, error) {
	if uid == 0 || !validIdentity(login) || !validIdentity(node) || !validIdentity(agent) || !validIdentity(membership) {
		return nil, errLandlordInput
	}
	value, err := remote(self, "online", "online", 2, uid)
	if err != nil {
		return nil, err
	}
	location, ok := value.(map[string]any)
	if !ok || location["login_id"] != login || location["access_node"] != node || location["agent_name"] != agent {
		return nil, errLandlordStale
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	allowed, err := self.CallName(ctx, landlordName, checkRouteCommand, uid, login, service.game.Name, membership)
	cancel()
	if err != nil {
		return nil, err
	}
	if allowed != true {
		return nil, errLandlordStale
	}
	seat := -1
	for i, p := range service.game.Players {
		if p.UserID == uid {
			seat = i
			break
		}
	}
	if seat < 0 {
		if service.game.Phase != "waiting" {
			return nil, errLandlordBusy
		}
		for i, p := range service.game.Players {
			if p.UserID == 0 {
				seat = i
				break
			}
		}
	}
	if seat < 0 {
		return nil, errLandlordBusy
	}
	player := &service.game.Players[seat]
	player.UserID, player.LoginID, player.AccessNode, player.AgentName, player.Membership = uid, login, node, agent, membership
	service.broadcast(self)
	return service.view(uid), nil
}

func (service *attackService) handleReady(self gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	seat, err := service.game.member(uid, login)
	if err != nil {
		return nil, err
	}
	if service.game.Phase != "waiting" {
		return nil, errLandlordBusy
	}
	balance, err := remote(self, "settlement", routing.ReadService(uintString(uid)), 4, uid)
	if err != nil {
		return nil, err
	}
	fields, ok := balance.(map[string]any)
	if !ok {
		return nil, errLandlordInput
	}
	amount, ok := fields["amount"].(int64)
	if !ok || amount < 2*baseSilver {
		return nil, errLandlordFunds
	}
	service.game.Players[seat].Ready = true
	all := true
	for _, p := range service.game.Players {
		all = all && p.UserID != 0 && p.Ready
	}
	if all {
		deck, err := shuffledDeck()
		if err != nil {
			return nil, err
		}
		if err := self.SendName(landlordName, sealCommand, service.game.Name, true); err != nil {
			return nil, err
		}
		service.game.deal(deck, rand.IntN(3), time.Now())
	}
	service.broadcast(self)
	return service.view(uid), nil
}

func (service *attackService) handleLeave(self gsr.CommandContext, uid uint64, login string) (bool, error) {
	seat, err := service.game.member(uid, login)
	if err != nil {
		return false, err
	}
	player := service.game.Players[seat]
	if (service.game.Phase != "waiting" && service.game.Phase != "finished") || service.result != nil && service.result.Status == "pending" {
		service.game.Players[seat].AccessNode, service.game.Players[seat].AgentName = "", ""
		return true, nil
	}
	if err := self.SendName(landlordName, releaseCommand, uid, login, service.game.Name, player.Membership, service.game.Phase == "waiting"); err != nil {
		return false, err
	}
	service.game.Players[seat] = landlordPlayer{}
	empty := true
	for _, p := range service.game.Players {
		empty = empty && p.UserID == 0
	}
	if empty {
		self.Exit()
		return true, nil
	}
	service.broadcast(self)
	return true, nil
}
