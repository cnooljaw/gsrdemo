package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// 斗地主目录使用固定逻辑名称，全部命令由该 Service 统一维护。
//
//rpc:service landlord public name=landlordName
const (
	landlordName                                   = "landlord"
	resolveCommand               runtime.CommandID = 1
	releaseCommand               runtime.CommandID = 10
	checkRouteCommand            runtime.CommandID = 11
	sealCommand                  runtime.CommandID = 12
	discardCommand               runtime.CommandID = 13
	reconcileReservationsCommand runtime.CommandID = 14
)

const callTimeout = time.Second
const reservationTimeout = 30 * time.Second

var (
	errLandlordInput = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.invalid_request", "invalid landlord request")
	errLandlordStale = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.stale_login", "login or room membership is not current")
	errLandlordTurn  = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.invalid_turn", "turn is not current")
	errLandlordCards = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.invalid_cards", "cards cannot be played")
	errLandlordBusy  = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.busy", "room is not available")
	errLandlordFunds = runtime.NewStatusError(runtime.ErrorClassDomain, "landlord.insufficient_silver", "wallet requires at least 20 silver")
)

type landlordRoute struct{ Room, LoginID, Membership string }
type landlordTable struct {
	Count   int
	Sealed  bool
	Pending map[uint64]time.Time // 只保存本桌尚未核对入座的预留期限，最多三份。
}

// landlordMember 是牌桌当前成员的身份快照，不携带手牌或共享引用。
type landlordMember struct {
	UserID              uint64
	LoginID, Membership string
}

// landlordService 只拥有随机凑桌的预留目录，不等待 Online、钱包、出牌或结算。
type landlordService struct {
	routes         map[uint64]landlordRoute
	tables         map[string]landlordTable
	journal        landlordJournal
	settlementNode cluster.NodeID
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*landlordService)(nil)

func newLandlordService(journal landlordJournal, settlementNode cluster.NodeID) *landlordService {
	service := &landlordService{routes: map[uint64]landlordRoute{}, tables: map[string]landlordTable{}, journal: journal, settlementNode: settlementNode}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		resolveCommand:               service.handleResolve,
		releaseCommand:               service.handleRelease,
		checkRouteCommand:            service.handleCheck,
		sealCommand:                  service.handleSeal,
		discardCommand:               service.handleDiscard,
		reconcileReservationsCommand: service.handleReconcileReservations,
	})
	return service
}

func (service *landlordService) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (service *landlordService) OnStop(context.Context) error {
	return nil
}

func (service *landlordService) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, cmd, args...)
}

func identifier(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func validIdentity(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range []byte(value) {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (service *landlordService) handleResolve(self gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	if uid == 0 || !validIdentity(login) {
		return nil, errLandlordInput
	}
	route, exists := service.routes[uid]
	if !exists {
		if len(service.routes) >= 3000 {
			return nil, errLandlordBusy
		}
		mostPlayers := -1
		for name, table := range service.tables {
			if table.Sealed || table.Count >= 3 {
				continue
			}
			if table.Count > mostPlayers || (table.Count == mostPlayers && name < route.Room) {
				route.Room = name
				mostPlayers = table.Count
			}
		}
		if route.Room == "" {
			if len(service.tables) >= 1000 {
				return nil, errLandlordBusy
			}
			name, err := identifier("table-")
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			_, err = self.CreateService(ctx, newAttackService(name, service.journal, nil, service.settlementNode))
			cancel()
			if err != nil {
				return nil, err
			}
			route.Room = name
			service.tables[name] = landlordTable{}
		}
	}
	membership, err := identifier("seat-")
	if err != nil {
		return nil, err
	}
	if !exists {
		table := service.tables[route.Room]
		table.Count++
		service.tables[route.Room] = table
	}
	route.LoginID, route.Membership = login, membership
	service.routes[uid] = route
	table := service.tables[route.Room]
	if table.Pending == nil {
		table.Pending = make(map[uint64]time.Time)
	}
	table.Pending[uid] = time.Now().Add(reservationTimeout)
	service.tables[route.Room] = table
	return map[string]any{"room_name": route.Room, "membership": membership}, nil
}

func (service *landlordService) handleCheck(_ gsr.CommandContext, uid uint64, login, room, membership string) (bool, error) {
	if service.routes[uid] != (landlordRoute{room, login, membership}) {
		return false, nil
	}
	deadline := service.tables[room].Pending[uid]
	return deadline.IsZero() || time.Now().Before(deadline), nil
}

func (service *landlordService) handleSeal(_ gsr.CommandContext, room string, sealed bool) error {
	table, ok := service.tables[room]
	if !ok {
		return errLandlordStale
	}
	table.Sealed = sealed
	service.tables[room] = table
	return nil
}

// handleRelease 确认旧成员可移除；同桌的新预留尚未 Join 时返回 false，保留原座位供重连。
func (service *landlordService) handleRelease(_ gsr.CommandContext, uid uint64, login, room, membership string, waiting bool) (bool, error) {
	route, exists := service.routes[uid]
	if !exists || route.Room != room {
		return true, nil
	}
	if route != (landlordRoute{room, login, membership}) {
		return false, nil
	}
	delete(service.routes, uid)
	table := service.tables[room]
	table.Count--
	delete(table.Pending, uid)
	if waiting {
		table.Sealed = false
	}
	if table.Count == 0 {
		delete(service.tables, room)
	} else {
		service.tables[room] = table
	}
	return true, nil
}

func (service *landlordService) handleDiscard(_ gsr.CommandContext, room string) error {
	delete(service.tables, room)
	for uid, route := range service.routes {
		if route.Room == room {
			delete(service.routes, uid)
		}
	}
	return nil
}

func (service *landlordService) handleReconcileReservations(self gsr.CommandContext, room string, members [3]landlordMember) (bool, error) {
	if self.Source() == 0 {
		return false, runtime.ErrUnknownCommand
	}
	if !validIdentity(room) {
		return false, errLandlordInput
	}
	seen := make(map[uint64]bool, 3)
	for _, member := range members {
		if member.UserID == 0 {
			continue
		}
		if seen[member.UserID] || !validIdentity(member.LoginID) || !validIdentity(member.Membership) {
			return false, errLandlordInput
		}
		seen[member.UserID] = true
	}
	return service.reconcileReservations(room, members, time.Now()), nil
}

func (service *landlordService) reconcileReservations(room string, members [3]landlordMember, now time.Time) bool {
	table, exists := service.tables[room]
	if !exists {
		return false
	}
	for uid, deadline := range table.Pending {
		route := service.routes[uid]
		if route.Room != room {
			delete(table.Pending, uid)
			continue
		}
		var current landlordMember
		for _, member := range members {
			if member.UserID == uid {
				current = member
				break
			}
		}
		if current.UserID != 0 && current.LoginID == route.LoginID && current.Membership == route.Membership {
			delete(table.Pending, uid) // 实际入座已发生，Call 的回执是否收到不影响确认。
			continue
		}
		if now.Before(deadline) {
			continue
		}
		if current.UserID != 0 {
			// 未完成的新登录不能永久挡住原成员的离开，也不能清掉原手牌。
			service.routes[uid] = landlordRoute{room, current.LoginID, current.Membership}
		} else {
			delete(service.routes, uid)
			table.Count--
		}
		delete(table.Pending, uid)
	}
	if table.Count == 0 {
		delete(service.tables, room)
		return false
	}
	service.tables[room] = table
	return true
}

func remote(self gsr.ServiceContext, node, name string, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return self.Cluster().Call(ctx, cluster.NodeID(node), cluster.ServiceName(name), command, args...)
}
