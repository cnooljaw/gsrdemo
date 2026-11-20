package main

import (
	"maps"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	dhmjrpc "github.com/cnooljaw/gsrdemo/common/rpc/dhmj"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

const dhmjNode = "dhmj"

// comDhmj 属于当前 Agent，目录继续持有实际座位和登录路由。
type comDhmj struct {
	deps        componentDependencies
	table       string
	seat        int64
	cleanupJoin bool // Join 可能已执行，但没有取得可用快照。
}

var _ gameComponent = (*comDhmj)(nil)

func newComDhmj(deps componentDependencies) *comDhmj {
	return &comDhmj{deps: deps}
}

func (*comDhmj) Name() string { return dhmjNode }

func (game *comDhmj) Entered() bool { return game.table != "" }

func (game *comDhmj) ProtocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_DHMJ_ENTER):     game.onEnter,
		uint32(demopb.MsgId_C2S_DHMJ_READY):     game.onReady,
		uint32(demopb.MsgId_C2S_DHMJ_SNAPSHOT):  game.onSnapshot,
		uint32(demopb.MsgId_C2S_DHMJ_LEAVE):     game.onLeave,
		uint32(demopb.MsgId_C2S_DHMJ_DISCARD):   game.onDiscard,
		uint32(demopb.MsgId_C2S_DHMJ_RESPOND):   game.onRespond,
		uint32(demopb.MsgId_C2S_DHMJ_SELF_WIN):  game.onSelfWin,
		uint32(demopb.MsgId_C2S_DHMJ_KONG):      game.onKong,
		uint32(demopb.MsgId_C2S_DHMJ_PASS_SELF): game.onPassSelf,
		uint32(demopb.MsgId_C2S_DHMJ_TRUSTEE):   game.onTrustee,
	}
}

func (game *comDhmj) CommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		dhmjSettlementPushCommand: game.handleSettlementPush,
		dhmjDiscardPushCommand:    game.handleDiscardPush,
		dhmjDrawPushCommand:       game.handleDrawPush,
		dhmjFlowerPushCommand:     game.handleFlowerPush,
		dhmjDealPushCommand:       game.handleDealPush,
		dhmjTurnPushCommand:       game.handleTurnPush,
		dhmjMeldPushCommand:       game.handleMeldPush,
		dhmjRoundEndPushCommand:   game.handleRoundEndPush,
	}
}

func (game *comDhmj) matchesEvent(login string, payload map[string]any) (bool, error) {
	identity := game.deps.Identity()
	if identity.UserID == 0 || identity.LoginID != login || !game.Entered() {
		return false, nil
	}
	table, ok := payload["table_name"].(string)
	if !ok || table == "" {
		return false, errInvalidBackendReply
	}
	return table == game.table, nil
}

func (game *comDhmj) handleDiscardPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	revision, revisionOK := payload["revision"].(uint64)
	seat, seatOK := payload["seat"].(int64)
	turn, turnOK := payload["turn"].(uint64)
	tile, tileOK := payload["tile"].(int64)
	if !revisionOK || revision == 0 || !seatOK || seat < 0 || seat >= 4 ||
		!turnOK || turn == 0 || !tileOK || tile <= 0 || tile > 0x37 {
		return errInvalidBackendReply
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_DISCARD_PUSH), &demopb.DhmjDiscardPush{
		TableName: game.table, Revision: revision, Seat: seat, Turn: turn, Tile: tile,
	})
}

func (game *comDhmj) handleSettlementPush(_ gsr.CommandContext, login string, payload map[string]any) error {
	if matches, err := game.matchesEvent(login, payload); !matches || err != nil {
		return err
	}
	identity := game.deps.Identity()
	settlement, ok := payload["settlement"].(map[string]any)
	uid, uidOK := settlement["user_id"].(uint64)
	if !ok || !uidOK || uid == 0 {
		return errInvalidBackendReply
	}
	if uid != identity.UserID {
		return nil
	}
	operationID, idOK := settlement["operation_id"].(string)
	version, versionOK := settlement["version"].(uint64)
	if !idOK || operationID == "" || settlement["status"] != "committed" || !versionOK || version == 0 {
		return errInvalidBackendReply
	}
	for _, field := range [...]string{"fee", "paid", "received", "balance"} {
		if amount, ok := settlement[field].(int64); !ok || amount < 0 {
			return errInvalidBackendReply
		}
	}
	message := &demopb.DhmjSettlementPush{}
	if err := clientMessage(payload, message); err != nil {
		return err
	}
	return game.deps.Push(uint32(demopb.MsgId_S2C_DHMJ_SETTLEMENT_PUSH), message)
}

func (game *comDhmj) onEnter(self gsr.CommandContext, reply protocol.Context, _ *demopb.DhmjEnterRequest) error {
	if err := game.deps.AllowEnter(game.Name()); err != nil {
		return err
	}
	identity := game.deps.Identity()
	value, err := game.deps.Call(self, dhmjNode, dhmjrpc.ServiceName, dhmjrpc.JoinCommand,
		identity.UserID, identity.LoginID, string(identity.AccessNode), string(identity.AgentName))
	if err != nil {
		if enterOutcomeUnknown(err) {
			game.cleanupJoin = true
		}
		return enterFailure(err)
	}
	game.cleanupJoin = true
	snapshot, err := dhmjSnapshot(value, identity.UserID)
	if err != nil {
		return err
	}
	game.table, game.seat, game.cleanupJoin = snapshot.TableName, snapshot.Seat, false
	return reply.Response(uint32(demopb.MsgId_S2C_DHMJ_ENTER), &demopb.DhmjEnterResponse{Snapshot: snapshot})
}

func (game *comDhmj) onReady(self gsr.CommandContext, reply protocol.Context, _ *demopb.DhmjReadyRequest) error {
	return game.replyAction(self, reply, dhmjrpc.ReadyCommand)
}

func (game *comDhmj) onDiscard(self gsr.CommandContext, reply protocol.Context, request *demopb.DhmjDiscardRequest) error {
	return game.replyAction(self, reply, dhmjrpc.DiscardCommand, request.Turn, request.Tile)
}

func (game *comDhmj) onRespond(self gsr.CommandContext, reply protocol.Context, request *demopb.DhmjRespondRequest) error {
	return game.replyAction(self, reply, dhmjrpc.RespondCommand, request.WindowId, request.Action)
}

func (game *comDhmj) onSelfWin(self gsr.CommandContext, reply protocol.Context, request *demopb.DhmjSelfWinRequest) error {
	return game.replyAction(self, reply, dhmjrpc.SelfWinCommand, request.Turn)
}

func (game *comDhmj) onKong(self gsr.CommandContext, reply protocol.Context, request *demopb.DhmjKongRequest) error {
	return game.replyAction(self, reply, dhmjrpc.KongCommand, request.Turn, request.Tile)
}

func (game *comDhmj) onPassSelf(self gsr.CommandContext, reply protocol.Context, request *demopb.DhmjPassSelfRequest) error {
	return game.replyAction(self, reply, dhmjrpc.PassSelfCommand, request.Turn)
}

func (game *comDhmj) onTrustee(self gsr.CommandContext, reply protocol.Context, request *demopb.DhmjTrusteeRequest) error {
	return game.replyAction(self, reply, dhmjrpc.TrusteeCommand, request.Enabled)
}

func (game *comDhmj) onSnapshot(self gsr.CommandContext, reply protocol.Context, _ *demopb.DhmjSnapshotRequest) error {
	identity := game.deps.Identity()
	value, err := game.callTable(self, identity, dhmjrpc.SnapshotCommand)
	if err != nil {
		return err
	}
	snapshot, err := dhmjSnapshot(value, identity.UserID)
	if err != nil || snapshot.TableName != game.table || snapshot.Seat != game.seat {
		return errInvalidBackendReply
	}
	return reply.Response(uint32(demopb.MsgId_S2C_DHMJ_SNAPSHOT), &demopb.DhmjSnapshotResponse{Snapshot: snapshot})
}

func (game *comDhmj) onLeave(self gsr.CommandContext, reply protocol.Context, _ *demopb.DhmjLeaveRequest) error {
	if err := game.callAction(self, dhmjrpc.LeaveCommand); err != nil {
		return err
	}
	game.table = ""
	return reply.Response(uint32(demopb.MsgId_S2C_ACTION_ACK), &demopb.ActionAck{})
}

func (game *comDhmj) replyAction(self gsr.CommandContext, reply protocol.Context, cmd runtime.CommandID, args ...any) error {
	if err := game.callAction(self, cmd, args...); err != nil {
		return err
	}
	return reply.Response(uint32(demopb.MsgId_S2C_ACTION_ACK), &demopb.ActionAck{})
}

func (game *comDhmj) callAction(self gsr.CommandContext, cmd runtime.CommandID, args ...any) error {
	value, err := game.callTable(self, game.deps.Identity(), cmd, args...)
	if err != nil {
		return err
	}
	if value != true {
		return errInvalidBackendReply
	}
	return nil
}

func (game *comDhmj) callTable(self gsr.CommandContext, identity agentIdentity, cmd runtime.CommandID, args ...any) (any, error) {
	if err := identity.RequireLogin(); err != nil {
		return nil, err
	}
	if !game.Entered() {
		return nil, errEnterRequired
	}
	return game.deps.Call(self, dhmjNode, dhmjrpc.ServiceName, cmd, append([]any{identity.UserID, identity.LoginID}, args...)...)
}

func (game *comDhmj) Disconnect(self gsr.ServiceContext) error {
	if !game.Entered() && !game.cleanupJoin {
		return nil
	}
	game.table, game.cleanupJoin = "", false
	identity := game.deps.Identity()
	_, err := game.deps.Call(self, dhmjNode, dhmjrpc.ServiceName, dhmjrpc.LeaveCommand, identity.UserID, identity.LoginID)
	return err
}

func dhmjSnapshot(value any, userID uint64) (*demopb.DhmjSnapshot, error) {
	fields, ok := value.(map[string]any)
	seat, hasSeat := fields["seat"].(int64)
	if !ok || !hasSeat || seat < 0 || seat >= 4 {
		return nil, errInvalidBackendReply
	}
	// Cluster 使用每个座位一个牌列表，Protobuf 用消息包住每组牌。
	if fields["phase"] == "playing" || fields["phase"] == "finished" {
		lists, ok := fields["discards"].([]any)
		if !ok || len(lists) != 4 {
			return nil, errInvalidBackendReply
		}
		discards := make([]any, len(lists))
		for seat, tiles := range lists {
			list, ok := tiles.([]any)
			if !ok {
				return nil, errInvalidBackendReply
			}
			discards[seat] = map[string]any{"tiles": list}
		}
		fields = maps.Clone(fields)
		fields["discards"] = discards
	}
	snapshot := &demopb.DhmjSnapshot{}
	if err := clientMessage(fields, snapshot); err != nil {
		return nil, err
	}
	switch snapshot.Phase {
	case "waiting", "playing", "finished":
	default:
		return nil, errInvalidBackendReply
	}
	if snapshot.TableName == "" {
		return nil, errInvalidBackendReply
	}
	if snapshot.RoundEnd != nil && (snapshot.Phase != "finished" || !validDhmjRoundEnd(snapshot.RoundEnd)) {
		return nil, errInvalidBackendReply
	}
	if settlement := snapshot.Settlement; settlement != nil && settlement.UserId != 0 && settlement.UserId != userID {
		return nil, errInvalidBackendReply
	}
	for _, player := range snapshot.Players {
		if player.GetSeat() == seat && player.GetUserId() == userID {
			return snapshot, nil
		}
	}
	return nil, errInvalidBackendReply
}
