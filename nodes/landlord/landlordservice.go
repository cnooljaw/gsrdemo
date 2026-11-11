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
	landlordName                        = "landlord"
	resolveCommand    runtime.CommandID = 1
	releaseCommand    runtime.CommandID = 10
	checkRouteCommand runtime.CommandID = 11
	sealCommand       runtime.CommandID = 12
	discardCommand    runtime.CommandID = 13
)

const callTimeout = time.Second

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
	Count  int
	Sealed bool
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
		resolveCommand:    service.handleResolve,
		releaseCommand:    service.handleRelease,
		checkRouteCommand: service.handleCheck,
		sealCommand:       service.handleSeal,
		discardCommand:    service.handleDiscard,
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
	return map[string]any{"room_name": route.Room, "membership": membership}, nil
}

func (service *landlordService) handleCheck(_ gsr.CommandContext, uid uint64, login, room, membership string) (bool, error) {
	return service.routes[uid] == (landlordRoute{room, login, membership}), nil
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

func remote(self gsr.ServiceContext, node, name string, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return self.Cluster().Call(ctx, cluster.NodeID(node), cluster.ServiceName(name), command, args...)
}
