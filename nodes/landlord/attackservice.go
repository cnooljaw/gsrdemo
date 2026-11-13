package main

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
)

// 每桌独立实例使用目录返回的名称；恢复实例不注册远程名称。
//
//rpc:service landlordattack public
const (
	roomJoinCommand     runtime.CommandID = 1
	roomReadyCommand    runtime.CommandID = 2
	roomPlayCommand     runtime.CommandID = 3
	roomSnapshotCommand runtime.CommandID = 4
	roomLeaveCommand    runtime.CommandID = 5
	roomBidCommand      runtime.CommandID = 6
	roomRobCommand      runtime.CommandID = 7
	roomTickCommand     runtime.CommandID = 10
)

const releaseRetryDelay = time.Second
const reservationCheckInterval = time.Second

// attackService 独占一桌状态；局末直接 Call 结算，确认后推送结果并清理待请求。
type attackService struct {
	game               attackGame
	settlement         roundSettlement
	recovery           bool
	releaseRetryAt     time.Time
	reservationCheckAt time.Time
	dispatcher         *gsr.Dispatcher
}

var _ gsr.Service = (*attackService)(nil)

func newAttackService(name string, journal landlordJournal, pending *landlordOperation, settlementNode cluster.NodeID) *attackService {
	service := &attackService{
		game:       newAttackGame(name),
		settlement: roundSettlement{journal: journal, operation: pending, node: settlementNode},
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
		service.settlement.prepareOperation(service.game)
		if !service.settlement.pending() {
			service.pushSnapshots(self)
		}
	}
	service.retrySettlement(self)
	if time.Now().Before(service.releaseRetryAt) {
		return nil
	}
	if !service.settlement.pending() && (service.game.Phase == "waiting" || service.game.Phase == "finished") {
		reclaimed, err := service.releaseDetachedPlayers(self)
		if reclaimed {
			service.pushSnapshots(self)
		}
		if err != nil {
			return err
		}
	}
	if service.recovery {
		if !service.settlement.pending() && !service.settlement.cleanupPending {
			self.Exit()
			return nil
		}
		return nil
	}
	if time.Now().Before(service.reservationCheckAt) {
		return nil
	}
	occupied, err := service.reconcileReservations(self)
	if err != nil {
		return err
	}
	if !occupied && service.game.empty() && !service.settlement.pending() && !service.settlement.cleanupPending {
		self.Exit()
		return nil
	}
	return nil
}

// reconcileReservations 上报值类型身份快照；目录不反向 Call 本桌，避免相互等待。
func (service *attackService) reconcileReservations(self gsr.ServiceContext) (bool, error) {
	defer func() { service.reservationCheckAt = time.Now().Add(reservationCheckInterval) }()
	var members [3]landlordMember
	for seat, player := range service.game.Players {
		members[seat] = landlordMember{player.UserID, player.LoginID, player.Membership}
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	value, err := self.CallName(ctx, landlordName, reconcileReservationsCommand, service.game.Name, members)
	if err != nil {
		return false, err
	}
	occupied, ok := value.(bool)
	if !ok {
		return false, errLandlordInput
	}
	return occupied, nil
}

// releaseDetachedPlayers 在牌局不再需要手牌后回收已离开成员；尚未完成的结算仍保留全部参与者。
func (service *attackService) releaseDetachedPlayers(self gsr.ServiceContext) (bool, error) {
	reclaimed := false
	for seat, player := range service.game.Players {
		if player.UserID == 0 || player.AccessNode != "" {
			continue
		}
		released, err := service.releaseRoute(self, player)
		if err != nil {
			return reclaimed, err
		}
		if released {
			service.game.remove(seat)
			reclaimed = true
		}
	}
	return reclaimed, nil
}

func (service *attackService) releaseRoute(self gsr.ServiceContext, player landlordPlayer) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	value, err := self.CallName(ctx, landlordName, releaseCommand, player.UserID, player.LoginID, service.game.Name, player.Membership, service.game.Phase == "waiting")
	if err != nil {
		service.releaseRetryAt = time.Now().Add(releaseRetryDelay)
		return false, err
	}
	released, ok := value.(bool)
	if !ok {
		service.releaseRetryAt = time.Now().Add(releaseRetryDelay)
		return false, errLandlordInput
	}
	return released, nil
}

// retrySettlement 由局末和 Pulse 调用；确认后只推送一次结果，后续只重试清理待记录。
func (service *attackService) retrySettlement(self gsr.ServiceContext) {
	if service.settlement.pending() {
		if err := service.settlement.submit(self); err != nil {
			_ = self.Logger().Error("斗地主待结算保留原操作", "operation_id", service.settlement.operation.ID, "error", err.Error())
			return
		}
		if service.settlement.pending() {
			return
		}
		if !service.recovery {
			service.pushSnapshots(self)
		}
	}
	service.deletePendingSettlement(self)
}

func (service *attackService) deletePendingSettlement(self gsr.ServiceContext) {
	if !service.settlement.cleanupPending {
		return
	}
	if err := service.settlement.deletePending(); err != nil {
		_ = self.Logger().Error("斗地主结算已确认，待重试清理 Redis", "operation_id", service.settlement.operation.ID, "error", err.Error())
	}
}

func (service *attackService) handleJoin(self gsr.CommandContext, uid uint64, login, node, agent, membership string) (map[string]any, error) {
	if uid == 0 || !validIdentity(login) || !validIdentity(node) || !validIdentity(agent) || !validIdentity(membership) {
		return nil, errLandlordInput
	}
	value, err := remote(self, "online", onlinerpc.ServiceName, onlinerpc.LookupCommand, uid)
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
	// 目录可能已释放旧成员而本桌丢失了回执；主动 Join 不等待后台回收的退避期。
	if service.game.Phase == "waiting" {
		if _, err := service.releaseDetachedPlayers(self); err != nil {
			return nil, err
		}
	}
	if err := service.game.join(uid, login, node, agent, membership); err != nil {
		return nil, err
	}
	service.pushSnapshots(self)
	return service.snapshotFor(uid), nil
}

func (service *attackService) handleReady(self gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	seat, err := service.game.readySeat(uid, login)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	balance, err := self.Cluster().Call(ctx, service.settlement.node, settlementread.ServiceName, settlementread.GetBalanceCommand, uid)
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
	// 当前回调内暂存准备和发牌结果；markReady 只改值字段，deal 分配新的手牌切片。
	next := service.game
	if next.markReady(seat) {
		deck, err := shuffledDeck()
		if err != nil {
			return nil, err
		}
		if err := self.SendName(landlordName, sealCommand, next.Name, true); err != nil {
			return nil, err
		}
		next.deal(deck, rand.IntN(3), time.Now())
	}
	service.game = next
	service.pushSnapshots(self)
	return service.snapshotFor(uid), nil
}

func (service *attackService) handleLeave(self gsr.CommandContext, uid uint64, login string) (bool, error) {
	seat, err := service.game.member(uid, login)
	if err != nil {
		return false, err
	}
	player := service.game.Players[seat]
	service.game.detach(seat)
	if (service.game.Phase != "waiting" && service.game.Phase != "finished") || service.settlement.pending() {
		return true, nil
	}
	released, err := service.releaseRoute(self, player)
	if err != nil {
		_ = self.Logger().Error("斗地主已接纳离开，待重试释放成员", "user_id", uid, "membership", player.Membership, "error", err.Error())
		return true, nil
	}
	if !released {
		return true, nil
	}
	service.game.remove(seat)
	// 即使本桌已空，也由 Pulse 核对其他有效预留后再退出。
	service.pushSnapshots(self)
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
	service.pushSnapshots(self)
	return service.snapshotFor(uid), nil
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
	service.settlement.prepareOperation(service.game)
	if !service.settlement.pending() {
		service.pushSnapshots(self)
	}
	service.retrySettlement(self)
	return service.snapshotFor(uid), nil
}

func (service *attackService) handleSnapshot(_ gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	if _, err := service.game.member(uid, login); err != nil {
		return nil, err
	}
	return service.snapshotFor(uid), nil
}

func (service *attackService) snapshotFor(uid uint64) map[string]any {
	view := service.game.view(uid)
	if settlement := service.settlement.snapshotFor(uid); settlement != nil {
		view["settlement"] = settlement
	}
	return view
}

func (service *attackService) pushSnapshots(self gsr.ServiceContext) {
	if service.settlement.pending() {
		return
	}
	for _, p := range service.game.Players {
		if p.UserID == 0 || p.AccessNode == "" {
			continue
		}
		if err := self.Cluster().Send(cluster.NodeID(p.AccessNode), cluster.ServiceName(p.AgentName), accessagent.LandlordSnapshotPushCommand, p.LoginID, service.snapshotFor(p.UserID)); err != nil {
			_ = self.Logger().Error("斗地主推送未接纳，客户端可查询快照", "error", err.Error())
		}
	}
}
