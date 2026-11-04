package main

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	roomJoinCommand             runtime.CommandID = 1
	roomReadyCommand            runtime.CommandID = 2
	roomPlayCommand             runtime.CommandID = 3
	roomSnapshotCommand         runtime.CommandID = 4
	roomLeaveCommand            runtime.CommandID = 5
	roomBidCommand              runtime.CommandID = 6
	roomRobCommand              runtime.CommandID = 7
	roomTickCommand             runtime.CommandID = 10
	landlordSnapshotPushCommand runtime.CommandID = 110
)

// attackService 独占一桌的状态；局末 Redis/Settlement 等待只影响本桌。
type attackService struct {
	game       attackGame
	settlement roundSettlement
	recovery   bool
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*attackService)(nil)

func newAttackService(name string, journal landlordJournal, pending *landlordResult, settlementNode cluster.NodeID) *attackService {
	service := &attackService{
		game:       newAttackGame(name),
		settlement: roundSettlement{journal: journal, result: pending, node: settlementNode},
		recovery:   pending != nil,
	}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		roomJoinCommand:     service.handleJoin,
		roomReadyCommand:    service.handleReady,
		roomPlayCommand:     service.handlePlay,
		roomBidCommand:      service.handleCallLandlord,
		roomRobCommand:      service.handleRobLandlord,
		roomSnapshotCommand: service.handleSnapshot,
		roomLeaveCommand:    service.handleLeave,
		roomTickCommand:     service.handleTick,
	})
	return service
}

func (service *attackService) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if !service.recovery {
		if err := self.Node().RegisterService(cluster.ServiceName(service.game.Name), self.Self()); err != nil {
			return err
		}
	}
	_, err := self.CreateService(ctx, &landlordPulse{target: self.Self()})
	return err
}

func (service *attackService) OnStop(context.Context) error {
	return nil
}

func (service *attackService) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, cmd, args...)
}

func (service *attackService) handleTick(self gsr.CommandContext) error {
	// 唤醒来自本地的独立 Pulse；远程 Command 的 Source 为零。
	if self.Source() == 0 {
		return runtime.ErrUnknownCommand
	}
	changed, err := service.game.timeout(time.Now())
	if err != nil {
		return err
	}
	if changed {
		service.settlement.prepare(service.game)
		if !service.settlement.pending() {
			service.broadcast(self)
		}
	}
	service.advanceSettlement(self)
	if service.settlement.result != nil && !service.settlement.pending() && !service.settlement.cleanupPending {
		if !service.recovery {
			for _, player := range service.game.Players {
				if player.UserID != 0 {
					return nil
				}
			}
		}
		self.Exit()
		return nil
	}
	return nil
}

// advanceSettlement 先发布有效回执，再清理恢复记录；清理失败只重试 HDel。
func (service *attackService) advanceSettlement(self gsr.ServiceContext) {
	if service.settlement.pending() {
		if err := service.settlement.settle(self); err != nil {
			_ = self.Logger().Error("斗地主待结算保留原操作", "operation_id", service.settlement.result.ID, "error", err.Error())
			return
		}
		if !service.recovery {
			service.broadcast(self)
		}
	}
	if service.settlement.cleanupPending {
		if err := service.settlement.cleanup(); err != nil {
			_ = self.Logger().Error("斗地主结算已确认，待重试清理 Redis", "operation_id", service.settlement.result.ID, "error", err.Error())
		}
	}
}

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
	if err := service.game.join(uid, login, node, agent, membership); err != nil {
		return nil, err
	}
	service.broadcast(self)
	return service.view(uid), nil
}

func (service *attackService) handleReady(self gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	seat, err := service.game.readySeat(uid, login)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	balance, err := self.Cluster().Call(ctx, service.settlement.node, "settlement-read", 4, uid)
	cancel()
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
	if service.game.markReady(seat) {
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
	if (service.game.Phase != "waiting" && service.game.Phase != "finished") || service.settlement.pending() {
		service.game.detach(seat)
		return true, nil
	}
	if err := self.SendName(landlordName, releaseCommand, uid, login, service.game.Name, player.Membership, service.game.Phase == "waiting"); err != nil {
		return false, err
	}
	if service.game.remove(seat) {
		if service.settlement.cleanupPending {
			return true, nil
		}
		self.Exit()
		return true, nil
	}
	service.broadcast(self)
	return true, nil
}

func (service *attackService) bid(self gsr.CommandContext, uid uint64, login string, turn uint64, yes bool) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	if !time.Now().Before(service.game.Deadline) {
		return nil, errLandlordTurn
	}
	if err := service.game.applyBid(uid, turn, yes, time.Now()); err != nil {
		return nil, err
	}
	service.broadcast(self)
	return service.view(uid), nil
}

func (service *attackService) handleCallLandlord(self gsr.CommandContext, uid uint64, login string, turn uint64, yes bool) (map[string]any, error) {
	if service.game.Phase != "calling" {
		return nil, errLandlordTurn
	}
	return service.bid(self, uid, login, turn, yes)
}

func (service *attackService) handleRobLandlord(self gsr.CommandContext, uid uint64, login string, turn uint64, yes bool) (map[string]any, error) {
	if service.game.Phase != "robbing" {
		return nil, errLandlordTurn
	}
	return service.bid(self, uid, login, turn, yes)
}

func (service *attackService) handlePlay(self gsr.CommandContext, uid uint64, login string, turn uint64, values []any) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	cards := make([]int64, len(values))
	if len(values) > 20 {
		return nil, errLandlordCards
	}
	for i, v := range values {
		card, ok := v.(int64)
		if !ok {
			return nil, errLandlordCards
		}
		cards[i] = card
	}
	now := time.Now()
	if !now.Before(service.game.Deadline) {
		return nil, errLandlordTurn
	}
	if err := service.game.play(uid, turn, cards, now); err != nil {
		return nil, err
	}
	service.settlement.prepare(service.game)
	if !service.settlement.pending() {
		service.broadcast(self)
	}
	service.advanceSettlement(self)
	return service.view(uid), nil
}

func (service *attackService) handleSnapshot(_ gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	return service.view(uid), nil
}

func (service *attackService) view(uid uint64) map[string]any {
	view := service.game.view(uid)
	if result := service.settlement.view(uid); result != nil {
		view["settlement"] = result
	}
	return view
}

func (service *attackService) broadcast(self gsr.ServiceContext) {
	for _, p := range service.game.Players {
		if p.UserID == 0 || p.AccessNode == "" {
			continue
		}
		if err := self.Cluster().Send(cluster.NodeID(p.AccessNode), cluster.ServiceName(p.AgentName), landlordSnapshotPushCommand, p.LoginID, service.view(p.UserID)); err != nil {
			_ = self.Logger().Error("斗地主推送未接纳，客户端可查询快照", "error", err.Error())
		}
	}
}
