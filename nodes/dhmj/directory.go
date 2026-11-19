package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
)

//rpc:service dhmj public name=directoryName
const (
	directoryName                     = "dhmj"
	joinCommand     runtime.CommandID = 1
	readyCommand    runtime.CommandID = 2
	snapshotCommand runtime.CommandID = 3
	leaveCommand    runtime.CommandID = 4
	discardCommand  runtime.CommandID = 5
	respondCommand  runtime.CommandID = 6
	selfWinCommand  runtime.CommandID = 7
	kongCommand     runtime.CommandID = 8
	passSelfCommand runtime.CommandID = 9
	trusteeCommand  runtime.CommandID = 10
)

const (
	maxRooms       = 1000
	minimumBalance = int64(3000)
	callTimeout    = 2 * time.Second
)

var (
	errStaleLogin   = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.stale_login", "当前登录或座位无效")
	errRoomBusy     = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.busy", "牌桌已开始或暂时没有空桌")
	errFunds        = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.insufficient_silver", "银子不足，不能准备")
	errInvalidReply = errors.New("定海麻将收到无效的内部回包")
)

type roomPlayer struct {
	uid       uint64
	login     string
	ready     bool
	connected bool
}

type room struct {
	name    string
	players [seatCount]roomPlayer
	service runtime.ServiceID
}

type directory struct {
	rooms      map[string]*room
	routes     map[uint64]*room
	nextRoom   uint64
	dispatcher *gsr.Dispatcher
	rules      roomRules
	journal    settlementJournal
}

var _ gsr.Service = (*directory)(nil)

func newDirectory(rules roomRules, journal settlementJournal) *directory {
	d := &directory{rooms: make(map[string]*room), routes: make(map[uint64]*room), rules: rules, journal: journal}
	d.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		joinCommand: d.handleJoin, readyCommand: d.handleReady,
		snapshotCommand: d.handleSnapshot, leaveCommand: d.handleLeave,
		discardCommand: d.handleDiscard, respondCommand: d.handleRespond,
		selfWinCommand: d.handleSelfWin, kongCommand: d.handleKong,
		passSelfCommand: d.handlePassSelf, trusteeCommand: d.handleTrustee,
	})
	return d
}

func (*directory) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*directory) OnStop(context.Context) error                      { return nil }

func (d *directory) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	return d.dispatcher.Handle(self, cmd, args...)
}

func (d *directory) handleJoin(self gsr.CommandContext, uid uint64, login, access, agent string) (map[string]any, error) {
	if uid == 0 || login == "" || access == "" || agent == "" {
		return nil, errStaleLogin
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	value, err := self.Cluster().Call(ctx, "online", onlinerpc.ServiceName, onlinerpc.LookupCommand, uid)
	cancel()
	if err != nil {
		return nil, err
	}
	location, ok := value.(map[string]any)
	if !ok || location["login_id"] != login || location["access_node"] != access || location["agent_name"] != agent {
		return nil, errStaleLogin
	}
	table, seat, err := d.seatPlayer(uid, login)
	if err != nil {
		return nil, err
	}
	return d.snapshotFor(self, table, seat)
}

func (d *directory) seatPlayer(uid uint64, login string) (*room, int, error) {
	if table := d.routes[uid]; table != nil {
		for seat, member := range table.players {
			if member.uid == uid {
				table.players[seat].login = login
				table.players[seat].connected = true
				return table, seat, nil
			}
		}
	}
	for _, table := range d.rooms {
		if table.service != 0 {
			continue
		}
		for seat, member := range table.players {
			if member.uid == 0 {
				table.players[seat] = roomPlayer{uid: uid, login: login, connected: true}
				d.routes[uid] = table
				return table, seat, nil
			}
		}
	}
	if len(d.rooms) >= maxRooms {
		return nil, -1, errRoomBusy
	}
	d.nextRoom++
	table := &room{name: fmt.Sprintf("dhmj-%d", d.nextRoom)}
	table.players[0] = roomPlayer{uid: uid, login: login, connected: true}
	d.rooms[table.name], d.routes[uid] = table, table
	return table, 0, nil
}

func (d *directory) currentSeat(uid uint64, login string) (*room, int, error) {
	table := d.routes[uid]
	if table != nil {
		for seat, member := range table.players {
			if member.uid == uid && member.login == login && member.connected {
				return table, seat, nil
			}
		}
	}
	return nil, -1, errStaleLogin
}

func (d *directory) handleReady(self gsr.CommandContext, uid uint64, login string) (bool, error) {
	table, seat, err := d.currentSeat(uid, login)
	if err != nil {
		return false, err
	}
	if table.service != 0 {
		return false, errRoomBusy
	}
	if table.players[seat].ready {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	value, err := self.Cluster().Call(ctx, "settlement", settlementread.ServiceName, settlementread.GetBalanceCommand, uid)
	cancel()
	if err != nil {
		return false, err
	}
	wallet, ok := value.(map[string]any)
	amount, valid := wallet["amount"].(int64)
	if !ok || !valid || wallet["user_id"] != uid {
		return false, errInvalidReply
	}
	if amount < minimumBalance {
		return false, errFunds
	}
	for i, player := range table.players {
		if i != seat && !player.ready {
			table.players[seat].ready = true
			return true, nil
		}
	}
	wall, err := shuffledWall()
	if err != nil {
		return false, err
	}
	service, err := newAttackService(wall, 0, 0x31)
	if err != nil {
		return false, err
	}
	service.rules, service.journal = d.rules, d.journal
	for i, player := range table.players {
		service.users[i] = player.uid
	}
	ctx, cancel = context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	id, err := self.CreateService(ctx, service)
	if err != nil {
		return false, err
	}
	table.players[seat].ready = true
	table.service = id
	return true, nil
}

func (d *directory) handleSnapshot(self gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	table, seat, err := d.currentSeat(uid, login)
	if err != nil {
		return nil, err
	}
	return d.snapshotFor(self, table, seat)
}

func (d *directory) snapshotFor(self gsr.ServiceContext, table *room, seat int) (map[string]any, error) {
	members := make([]any, 0, seatCount)
	for i, player := range table.players {
		if player.uid != 0 {
			members = append(members, map[string]any{"user_id": player.uid, "seat": int64(i), "ready": player.ready})
		}
	}
	reply := map[string]any{"room_name": table.name, "seat": int64(seat), "players": members}
	if table.service == 0 {
		reply["phase"] = "waiting"
		return reply, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	value, err := self.Call(ctx, table.service, tableSnapshotCommand, seat)
	if err != nil {
		return nil, err
	}
	view, ok := value.(tableView)
	if !ok {
		return nil, errInvalidReply
	}
	reply["phase"] = "playing"
	if view.Finished {
		reply["phase"] = "finished"
	}
	if view.Settlement != nil {
		reply["settlement"] = view.Settlement
	}
	reply["hand"] = wireTiles(view.Hand)
	reply["active_seat"], reply["turn"], reply["window_id"] = int64(view.Active), view.Turn, view.WindowID
	reply["server_ms"], reply["deadline_ms"] = time.Now().UnixMilli(), int64(0)
	if !view.Deadline.IsZero() {
		reply["deadline_ms"] = view.Deadline.UnixMilli()
	}
	discards, handCounts, trustees := make([]any, seatCount), make([]any, seatCount), make([]any, seatCount)
	for seat := range seatCount {
		discards[seat] = wireTiles(view.Out[seat])
		handCounts[seat] = int64(view.HandCounts[seat])
		trustees[seat] = view.Trustees[seat]
	}
	reply["discards"], reply["hand_counts"], reply["trustees"] = discards, handCounts, trustees
	reply["window_tile"] = int64(view.WindowTile)
	offers := make([]any, 0)
	for action := eatRight; action <= claimHu; action++ {
		if view.Offer.has(action) {
			offers = append(offers, int64(action))
		}
	}
	reply["offers"] = offers
	reply["self_action"], reply["can_self_win"] = view.SelfAction, view.CanSelfWin
	reply["concealed_kongs"], reply["added_kongs"] = wireTiles(view.ConcealedKongs), wireTiles(view.AddedKongs)
	return reply, nil
}

func wireTiles(tiles []byte) []any {
	result := make([]any, len(tiles))
	for i, tile := range tiles {
		result[i] = int64(tile)
	}
	return result
}

func (d *directory) handleLeave(_ gsr.CommandContext, uid uint64, login string) (bool, error) {
	table := d.routes[uid]
	if table == nil {
		return true, nil
	}
	for seat, member := range table.players {
		if member.uid != uid {
			continue
		}
		if member.login != login {
			return false, errStaleLogin
		}
		if table.service != 0 {
			table.players[seat].connected = false
		} else {
			table.players[seat] = roomPlayer{}
			delete(d.routes, uid)
			if table.players == [seatCount]roomPlayer{} {
				delete(d.rooms, table.name)
			}
		}
		return true, nil
	}
	return false, errStaleLogin
}
