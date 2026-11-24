package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
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

// 目录内部命令通过本地 ServiceID 校验来源。
const (
	tableSettledCommand runtime.CommandID = 11
	sweepTablesCommand  runtime.CommandID = 12
	tableEventCommand   runtime.CommandID = 13
)

const (
	maxTables        = 1000
	minimumBalance   = int64(3000)
	callTimeout      = 2 * time.Second
	rejoinGrace      = 30 * time.Second
	tableSweepPeriod = time.Second
)

var (
	errStaleLogin   = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.stale_login", "当前登录或座位无效")
	errTableBusy    = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.busy", "牌桌已开始或暂时没有空桌")
	errFunds        = runtime.NewStatusError(runtime.ErrorClassDomain, "dhmj.insufficient_silver", "银子不足，不能准备")
	errInvalidReply = errors.New("定海麻将收到无效的内部回包")
)

type tablePlayer struct {
	uid                   uint64
	login                 string
	access                cluster.NodeID
	agent                 cluster.ServiceName
	ready                 bool
	connected             bool
	offlineTrusteePending bool
}

type table struct {
	name      string
	players   [seatCount]tablePlayer
	revision  uint64 // 等待区序号，开局后由牌桌继续递增。
	service   runtime.ServiceID
	settledAt time.Time
}

type directory struct {
	tables     map[string]*table
	routes     map[uint64]*table
	nextTable  uint64
	dispatcher *gsr.Dispatcher
	rules      roomRules
	journal    settlementJournal
	pulse      runtime.ServiceID
}

var _ gsr.Service = (*directory)(nil)

func newDirectory(rules roomRules, journal settlementJournal) *directory {
	d := &directory{tables: make(map[string]*table), routes: make(map[uint64]*table), rules: rules, journal: journal}
	d.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		joinCommand: d.handleJoin, readyCommand: d.handleReady,
		snapshotCommand: d.handleSnapshot, leaveCommand: d.handleLeave,
		discardCommand: d.handleDiscard, respondCommand: d.handleRespond,
		selfWinCommand: d.handleSelfWin, kongCommand: d.handleKong,
		passSelfCommand: d.handlePassSelf, trusteeCommand: d.handleTrustee,
		tableSettledCommand: d.handleTableSettled, sweepTablesCommand: d.handleSweepTables,
		tableEventCommand: d.handleTableEvent,
	})
	return d
}

func (d *directory) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, &pulseService{target: self.Self(), command: sweepTablesCommand, period: tableSweepPeriod})
	d.pulse = id
	return err
}

func (*directory) OnStop(context.Context) error { return nil }

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
	_, _, currentLoginErr := d.currentSeat(uid, login)
	table, seat, joined, err := d.seatPlayer(uid, login)
	if err != nil {
		return nil, err
	}
	table.players[seat].access, table.players[seat].agent = cluster.NodeID(access), cluster.ServiceName(agent)
	if joined {
		table.pushSeat(self, seat, uid, true, false, uid)
	} else if currentLoginErr != nil && table.service != 0 && table.settledAt.IsZero() {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		_, err := self.Call(ctx, table.service, tableConnectionTrusteeCommand, seat, false)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	return d.snapshotFor(self, table, seat)
}

func (d *directory) seatPlayer(uid uint64, login string) (*table, int, bool, error) {
	if table := d.routes[uid]; table != nil {
		d.expireSeats(table, time.Now())
		for seat, member := range table.players {
			if member.uid == uid {
				table.players[seat].login = login
				table.players[seat].connected = true
				table.players[seat].offlineTrusteePending = false
				return table, seat, false, nil
			}
		}
	}
	for _, table := range d.tables {
		if table.service != 0 {
			continue
		}
		for seat, member := range table.players {
			if member.uid == 0 {
				table.players[seat] = tablePlayer{uid: uid, login: login, connected: true}
				d.routes[uid] = table
				return table, seat, true, nil
			}
		}
	}
	if len(d.tables) >= maxTables {
		return nil, -1, false, errTableBusy
	}
	d.nextTable++
	table := &table{name: fmt.Sprintf("dhmj-%d", d.nextTable)}
	table.players[0] = tablePlayer{uid: uid, login: login, connected: true}
	d.tables[table.name], d.routes[uid] = table, table
	return table, 0, true, nil
}

func (d *directory) currentSeat(uid uint64, login string) (*table, int, error) {
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
		return false, errTableBusy
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
			table.pushSeat(self, seat, uid, true, true, 0)
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
	service.name, service.directory = table.name, self.Self()
	// 预留最后一位玩家的准备事件，创建成功后才发布。
	service.game.revision = table.revision + 1
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
	table.pushSeat(self, seat, uid, true, true, 0)
	return true, nil
}

func (d *directory) handleSnapshot(self gsr.CommandContext, uid uint64, login string) (map[string]any, error) {
	table, seat, err := d.currentSeat(uid, login)
	if err != nil {
		return nil, err
	}
	return d.snapshotFor(self, table, seat)
}

func (d *directory) snapshotFor(self gsr.ServiceContext, table *table, seat int) (map[string]any, error) {
	members := make([]any, 0, seatCount)
	for i, player := range table.players {
		if player.uid != 0 {
			members = append(members, map[string]any{"user_id": player.uid, "seat": int64(i), "ready": player.ready})
		}
	}
	reply := map[string]any{"table_name": table.name, "seat": int64(seat), "players": members}
	if table.service == 0 {
		reply["phase"] = "waiting"
		reply["revision"] = table.revision
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
		reply["round_end"] = view.RoundEnd
	}
	if view.Settlement != nil {
		reply["settlement"] = view.Settlement
	}
	reply["hand"] = wireTiles(view.Hand)
	reply["dealer_seat"], reply["remaining"] = int64(view.Dealer), int64(view.Remaining)
	reply["active_seat"], reply["turn"], reply["window_id"] = int64(view.Active), view.Turn, view.WindowID
	reply["revision"] = view.Revision
	reply["server_ms"], reply["deadline_ms"] = time.Now().UnixMilli(), int64(0)
	if !view.Deadline.IsZero() {
		reply["deadline_ms"] = view.Deadline.UnixMilli()
	}
	discards, handCounts, trustees := make([]any, seatCount), make([]any, seatCount), make([]any, seatCount)
	seatTiles := make([]any, seatCount)
	for seat := range seatCount {
		discards[seat] = wireTiles(view.Out[seat])
		handCounts[seat] = int64(view.HandCounts[seat])
		trustees[seat] = view.Trustees[seat]
		melds := make([]any, 0, len(view.Melds[seat]))
		for _, meld := range view.Melds[seat] {
			melds = append(melds, meld.wire())
		}
		seatTiles[seat] = map[string]any{"melds": melds, "flowers": wireTiles(view.Flowers[seat])}
	}
	reply["discards"], reply["hand_counts"], reply["trustees"] = discards, handCounts, trustees
	reply["seat_tiles"] = seatTiles
	reply["window_tile"] = int64(view.WindowTile)
	reply["offers"] = wireOffers(view.Offer)
	reply["operation_phase"], reply["rob_kong"], reply["can_discard"] = view.OperationPhase, view.RobKong, view.CanDiscard
	reply["self_action"], reply["can_self_win"] = view.SelfAction, view.CanSelfWin
	reply["concealed_kongs"], reply["added_kongs"] = wireTiles(view.ConcealedKongs), wireTiles(view.AddedKongs)
	return reply, nil
}

func (m meld) wire() map[string]any {
	var kind string
	switch m.action {
	case eatRight, eatMiddle, eatLeft:
		kind = "eat"
	case pong:
		kind = "pong"
	case directKong:
		kind = "direct_kong"
	case concealedKong:
		kind = "concealed_kong"
	case addedKong:
		kind = "added_kong"
	}
	return map[string]any{"kind": kind, "source_seat": int64(m.source), "tiles": wireTiles(m.tiles)}
}

func wireTiles(tiles []byte) []any {
	result := make([]any, len(tiles))
	for i, tile := range tiles {
		result[i] = int64(tile)
	}
	return result
}

func (d *directory) handleLeave(self gsr.CommandContext, uid uint64, login string) (bool, error) {
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
		if !member.connected {
			return true, nil
		}
		if table.service != 0 && table.settledAt.IsZero() {
			table.players[seat].connected = false
			table.players[seat].access, table.players[seat].agent = "", ""
			table.sendOfflineTrustee(self, seat)
		} else {
			d.releaseSeat(table, seat)
			if table.service == 0 {
				table.pushSeat(self, seat, uid, false, false, 0)
			}
			d.closeEmptyTable(self, table)
		}
		return true, nil
	}
	return false, errStaleLogin
}

func (table *table) sendOfflineTrustee(self gsr.CommandContext, seat int) {
	err := self.Send(table.service, tableConnectionTrusteeCommand, seat, true)
	table.players[seat].offlineTrusteePending = errors.Is(err, runtime.ErrMailboxFull)
	if err != nil && !table.players[seat].offlineTrusteePending {
		_ = self.Logger().Error("定海麻将离线托管未接纳", "table_name", table.name, "seat", seat, "error", err.Error())
	}
}

func (table *table) pushSeat(self gsr.CommandContext, seat int, uid uint64, joined, ready bool, skipUID uint64) {
	table.revision++
	payload := map[string]any{
		"table_name": table.name, "revision": table.revision, "seat": int64(seat),
		"user_id": uid, "joined": joined, "ready": ready,
	}
	for _, player := range table.players {
		if !player.connected || player.uid == skipUID || player.access == "" || player.agent == "" {
			continue
		}
		if err := self.Cluster().Send(player.access, player.agent, accessagent.DhmjSeatPushCommand, player.login, payload); err != nil {
			_ = self.Logger().Error("定海麻将座位推送未接纳", "table_name", table.name, "revision", table.revision, "user_id", player.uid, "error", err.Error())
		}
	}
}

func (d *directory) handleTableEvent(self gsr.CommandContext, name string, event roundEvent) error {
	table := d.tables[name]
	if table == nil || table.service == 0 || table.service != self.Source() {
		return runtime.ErrUnknownCommand
	}
	var playerIDs []any
	if event.kind == dealEvent {
		playerIDs = make([]any, seatCount)
		for seat, player := range table.players {
			playerIDs[seat] = player.uid
		}
	}
	for seat, player := range table.players {
		if !player.connected || player.access == "" || player.agent == "" {
			continue
		}
		command, payload := event.wire(name, seat)
		if playerIDs != nil {
			payload["player_ids"] = playerIDs
		}
		if err := self.Cluster().Send(player.access, player.agent, command, player.login, payload); err != nil {
			_ = self.Logger().Error("定海麻将牌桌推送未接纳", "table_name", name, "revision", event.revision, "user_id", player.uid, "error", err.Error())
		}
	}
	return nil
}

func (d *directory) handleTableSettled(self gsr.CommandContext, name, operationID string, receipt [seatCount]roundSilver) error {
	table := d.tables[name]
	if table == nil || table.service == 0 || table.service != self.Source() {
		return runtime.ErrUnknownCommand
	}
	if !table.settledAt.IsZero() {
		return nil
	}
	table.settledAt = time.Now()
	for seat, player := range table.players {
		if !player.connected || player.access == "" || player.agent == "" || receipt[seat].UserID != player.uid {
			continue
		}
		payload := map[string]any{"table_name": name, "settlement": receipt[seat].wire(operationID)}
		if err := self.Cluster().Send(player.access, player.agent, accessagent.DhmjSettlementPushCommand, player.login, payload); err != nil {
			_ = self.Logger().Error("定海麻将结算推送未接纳", "operation_id", operationID, "user_id", player.uid, "error", err.Error())
		}
	}
	return nil
}

func (d *directory) handleSweepTables(self gsr.CommandContext) error {
	if d.pulse == 0 || self.Source() != d.pulse {
		return runtime.ErrUnknownCommand
	}
	now := time.Now()
	for _, table := range d.tables {
		if !table.settledAt.IsZero() {
			d.expireSeats(table, now)
			d.closeEmptyTable(self, table)
			continue
		}
		for seat, player := range table.players {
			if player.offlineTrusteePending {
				table.sendOfflineTrustee(self, seat)
			}
		}
	}
	return nil
}

func (d *directory) expireSeats(table *table, now time.Time) {
	if table.settledAt.IsZero() || now.Before(table.settledAt.Add(rejoinGrace)) {
		return
	}
	for seat, member := range table.players {
		if member.uid != 0 && !member.connected {
			d.releaseSeat(table, seat)
		}
	}
}

func (d *directory) releaseSeat(table *table, seat int) {
	delete(d.routes, table.players[seat].uid)
	table.players[seat] = tablePlayer{}
}

// closeEmptyTable 保留未接纳关闭命令的空桌，目录 Pulse 会再次尝试。
func (d *directory) closeEmptyTable(self gsr.CommandContext, table *table) {
	if table.players != [seatCount]tablePlayer{} {
		return
	}
	if table.service != 0 {
		err := self.Send(table.service, tableCloseCommand)
		if err != nil && !errors.Is(err, runtime.ErrServiceNotFound) && !errors.Is(err, runtime.ErrServiceClosed) &&
			!errors.Is(err, runtime.ErrServiceStopping) {
			if !errors.Is(err, runtime.ErrMailboxFull) {
				_ = self.Logger().Error("定海麻将牌桌关闭未接纳", "table_name", table.name, "error", err.Error())
			}
			return
		}
	}
	delete(d.tables, table.name)
}
