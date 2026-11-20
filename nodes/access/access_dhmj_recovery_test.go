package main

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// TestAccessDhmjSettlementOutageLiveMySQL 保护结算服中断期间重登，恢复后按同一 OperationID 向四人交付一次入账。
func TestAccessDhmjSettlementOutageLiveMySQL(t *testing.T) {
	runAccessClientsLiveMySQL(t, 4, 1, "dhmj", true)
}

func joinDhmjProcessTable(t *testing.T, gate string, player accessDemoPlayer) (*accessClient, *demopb.DhmjSnapshot) {
	t.Helper()
	client := dialAccess(t, gate)
	var login demopb.LoginResponse
	accessLoginResponse(t, client, player.username, player.password, &login)
	if login.GetUserId() != player.id {
		t.Fatalf("登录身份错误：%d", login.GetUserId())
	}
	var enter demopb.DhmjEnterResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_DHMJ_ENTER, demopb.MsgId_S2C_DHMJ_ENTER, &demopb.DhmjEnterRequest{}, &enter)
	if enter.GetSnapshot() == nil {
		t.Fatal("进桌回执缺少快照")
	}
	return client, enter.GetSnapshot()
}

func dhmjProcessSnapshot(t *testing.T, client *accessClient) *demopb.DhmjSnapshot {
	t.Helper()
	var response demopb.DhmjSnapshotResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_DHMJ_SNAPSHOT, demopb.MsgId_S2C_DHMJ_SNAPSHOT,
		&demopb.DhmjSnapshotRequest{}, &response)
	if response.GetSnapshot() == nil {
		t.Fatal("麻将回执缺少快照")
	}
	return response.GetSnapshot()
}

func dhmjProcessAction(t *testing.T, client *accessClient, id demopb.MsgId, request proto.Message) {
	t.Helper()
	accessProcessResponse(t, client, id, demopb.MsgId_S2C_ACTION_ACK, request, &demopb.ActionAck{})
}

func finishDhmjWithSettlementOffline(t *testing.T, ctx context.Context, seats [4]*accessClient) *demopb.DhmjSnapshot {
	t.Helper()
	for ctx.Err() == nil {
		view := dhmjProcessSnapshot(t, seats[0])
		if view.GetPhase() == "finished" {
			return view
		}
		if view.GetOperationPhase() == "respond" {
			for _, client := range seats {
				current := dhmjProcessSnapshot(t, client)
				if current.GetWindowId() != view.GetWindowId() || current.GetOperationPhase() != "respond" {
					break
				}
				if len(current.GetOffers()) == 0 {
					continue
				}
				action := int64(7) // 其余选项一律过，保留胡牌作为正常终局。
				for _, offer := range current.GetOffers() {
					if offer == 6 {
						action = offer
					}
				}
				dhmjProcessAction(t, client, demopb.MsgId_C2S_DHMJ_RESPOND, &demopb.DhmjRespondRequest{WindowId: current.GetWindowId(), Action: action})
			}
			continue
		}
		active := view.GetActiveSeat()
		if active < 0 || active >= int64(len(seats)) {
			t.Fatal("无效的麻将活动座位", active)
		}
		seat := seats[active]
		own := dhmjProcessSnapshot(t, seat)
		switch {
		case own.GetCanSelfWin():
			dhmjProcessAction(t, seat, demopb.MsgId_C2S_DHMJ_SELF_WIN, &demopb.DhmjSelfWinRequest{Turn: own.GetTurn()})
		case own.GetSelfAction():
			dhmjProcessAction(t, seat, demopb.MsgId_C2S_DHMJ_PASS_SELF, &demopb.DhmjPassSelfRequest{Turn: own.GetTurn()})
		case own.GetCanDiscard():
			var tile int64
			for _, card := range own.GetHand() {
				if card > 0 && card < 0x41 {
					tile = card
					break
				}
			}
			if tile == 0 {
				t.Fatal("可出牌时没有普通牌")
			}
			dhmjProcessAction(t, seat, demopb.MsgId_C2S_DHMJ_DISCARD, &demopb.DhmjDiscardRequest{Turn: own.GetTurn(), Tile: tile})
		default:
			t.Fatalf("无法推进麻将轮次：%s turn=%d", own.GetOperationPhase(), own.GetTurn())
		}
	}
	t.Fatal("等待麻将终局超时")
	return nil
}

func verifyDhmjSettlementOutage(t *testing.T, ctx context.Context, gate string, players []accessDemoPlayer, db *sql.DB,
	ids *[]string, settlement accessNodeProcess, redisAddress string) {
	t.Helper()
	var seats [4]*accessClient
	var members [4]accessDemoPlayer
	var table string
	for i, player := range players {
		client, view := joinDhmjProcessTable(t, gate, player)
		if i == 0 {
			table = view.GetTableName()
		}
		seat := view.GetSeat()
		if table == "" || view.GetTableName() != table || seat < 0 || seat >= int64(len(seats)) || seats[seat] != nil {
			t.Fatalf("四人未正确入桌：%v", view)
		}
		seats[seat] = client
		members[seat] = player
		dhmjProcessAction(t, client, demopb.MsgId_C2S_DHMJ_READY, &demopb.DhmjReadyRequest{})
	}
	if err := settlement.process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitAccessProcessListenerClosed(t, settlement.address)
	finished := finishDhmjWithSettlementOffline(t, ctx, seats)
	if finished.GetRoundEnd() == nil {
		t.Fatal("结算服停机影响了麻将裁决")
	}

	var originalID string
	deadline := time.Now().Add(5 * time.Second)
	for originalID == "" {
		originalID = string(accessProcessRedis(t, ctx, redisAddress, "HKEYS", accessRedisKey))
		if originalID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("结算服离线时未保存原请求")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if count := accessProcessRedis(t, ctx, redisAddress, "HLEN", accessRedisKey); string(count) != "1" {
		t.Fatalf("预期只有一笔麻将待结算，实际 %s", count)
	}
	*ids = append(*ids, originalID)
	saved := accessProcessRedis(t, ctx, redisAddress, "HGET", accessRedisKey, originalID)
	var operations int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id=?", originalID).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("结算服离线时已有账务记录：%d %v", operations, err)
	}

	// 同一账号换连接。停机期间只恢复牌桌、终局和待确认状态。
	const seat = 0
	if err := seats[seat].Close(); err != nil {
		t.Fatal(err)
	}
	rejoined, restored := joinDhmjProcessTable(t, gate, members[seat])
	seats[seat] = rejoined
	if restored.GetTableName() != table || restored.GetSeat() != seat || !proto.Equal(finished.GetRoundEnd(), restored.GetRoundEnd()) ||
		restored.GetSettlement().GetStatus() != "pending" || restored.GetSettlement().GetOperationId() != originalID {
		t.Fatalf("重登没有恢复原局待结算：%v", restored)
	}
	if again := accessProcessRedis(t, ctx, redisAddress, "HGET", accessRedisKey, originalID); !bytes.Equal(saved, again) || len(saved) == 0 {
		t.Fatal("重登改变了原结算请求")
	}

	startAccessProcess(t, settlement.binary, []string{"-config", settlement.config}, settlement.address)
	receipts := make(map[uint64]*demopb.DhmjSettlement, len(players))
	for seat, client := range seats {
		for {
			envelope := readAccessEnvelope(t, client)
			if envelope.MsgId != uint32(demopb.MsgId_S2C_DHMJ_SETTLEMENT_PUSH) {
				continue
			}
			var push demopb.DhmjSettlementPush
			if err := proto.Unmarshal(envelope.Payload, &push); err != nil {
				t.Fatal(err)
			}
			item := push.GetSettlement()
			if push.GetTableName() != table || item.GetOperationId() != originalID || item.GetStatus() != "committed" || item.GetUserId() != members[seat].id {
				t.Fatalf("回执错桌、换身份或重复：%v", &push)
			}
			receipts[item.GetUserId()] = item
			break
		}
	}
	var paid, received, fee int64
	for _, player := range players {
		item := receipts[player.id]
		var balance int64
		var version uint64
		err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id=?", player.id).Scan(&balance, &version)
		if item == nil || err != nil || item.GetBalance() != balance || item.GetVersion() != version || version != 2 ||
			item.GetFee() != 50 || balance != 10000-item.GetFee()-item.GetPaid()+item.GetReceived() {
			t.Fatalf("用户 %d 结算未单次入账：%v wallet=%d/%d err=%v", player.id, item, balance, version, err)
		}
		paid, received, fee = paid+item.GetPaid(), received+item.GetReceived(), fee+item.GetFee()
	}
	var recordedDebit, recordedCredit int64
	err := db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(debit),0), COALESCE(SUM(credit),0) FROM settlement_operations WHERE operation_id=?", originalID).
		Scan(&operations, &recordedDebit, &recordedCredit)
	if err != nil || operations != 1 || paid != received || recordedDebit != paid+fee || recordedCredit != received {
		t.Fatalf("原操作入账不唯一或实际扣返不符：%d %d/%d %v", operations, recordedDebit, recordedCredit, err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for string(accessProcessRedis(t, ctx, redisAddress, "HLEN", accessRedisKey)) != "0" {
		if time.Now().After(deadline) {
			t.Fatal("确认入账后 Redis 待记录未清理")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, client := range seats {
		dhmjProcessAction(t, client, demopb.MsgId_C2S_DHMJ_LEAVE, &demopb.DhmjLeaveRequest{})
	}
}
