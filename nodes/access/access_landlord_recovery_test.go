package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os/exec"
	"sort"
	"testing"
	"time"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

type landlordSavedResult struct {
	ID         string `json:"operation_id"`
	Multiplier int64  `json:"multiplier"`
	Landlord   int    `json:"landlord_seat"`
	Winner     int    `json:"winner_seat"`
	Changes    []struct {
		UserID uint64 `json:"user_id"`
		Debit  int64  `json:"debit"`
		Credit int64  `json:"credit"`
	} `json:"changes"`
}

func landlordProcessView(t *testing.T, client *accessClient, id demopb.MsgId, request proto.Message) *demopb.LandlordSnapshot {
	t.Helper()
	var response interface {
		proto.Message
		GetSnapshot() *demopb.LandlordSnapshot
	}
	switch id {
	case demopb.MsgId_C2S_LANDLORD_ENTER:
		response = &demopb.LandlordEnterResponse{}
	case demopb.MsgId_C2S_LANDLORD_READY:
		response = &demopb.LandlordReadyResponse{}
	case demopb.MsgId_C2S_LANDLORD_PLAY:
		response = &demopb.LandlordPlayResponse{}
	case demopb.MsgId_C2S_LANDLORD_CALL:
		response = &demopb.LandlordCallResponse{}
	case demopb.MsgId_C2S_LANDLORD_ROB:
		response = &demopb.LandlordRobResponse{}
	case demopb.MsgId_C2S_LANDLORD_SNAPSHOT:
		response = &demopb.LandlordSnapshotResponse{}
	default:
		t.Fatal("unsupported landlord response")
	}
	accessProcessResponse(t, client, id, request, response)
	if response.GetSnapshot() == nil {
		t.Fatal("missing landlord snapshot")
	}
	return response.GetSnapshot()
}

func landlordRedis(t *testing.T, ctx context.Context, address string, args ...string) []byte {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	command := exec.CommandContext(wait, "redis-cli", append([]string{"-h", host, "-p", port, "--raw"}, args...)...)
	data, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("test Redis: %v %s", err, data)
	}
	return bytes.TrimSpace(data)
}

func landlordDemoRank(card int64) int64 {
	rank := card & 15
	if rank >= 14 {
		return rank + 2
	}
	if rank < 3 {
		return rank + 13
	}
	return rank
}

// playAccessLandlordRound 通过真实 MAC 客户端跑完一局；结算中断前先完成准入和叫抢。
func playAccessLandlordRound(t *testing.T, gates []string, players []accessDemoPlayer, settlement accessNodeProcess) ([]*accessClient, *demopb.LandlordSnapshot) {
	t.Helper()
	clients := make([]*accessClient, 3)
	bySeat := map[int64]*accessClient{}
	room := ""
	for i, player := range players {
		client := dialAccess(t, gates[i%len(gates)])
		clients[i] = client
		var login demopb.LoginResponse
		accessLoginResponse(t, client, player.username, player.password, &login)
		if login.UserId != player.id {
			t.Fatal("landlord login identity changed")
		}
		view := landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_ENTER, &demopb.LandlordEnterRequest{})
		if room == "" {
			room = view.RoomName
		}
		if room != view.RoomName {
			t.Fatal("recovery users not seated together")
		}
		bySeat[view.Seat] = client
	}
	var view *demopb.LandlordSnapshot
	for _, client := range clients {
		view = landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_READY, &demopb.LandlordReadyRequest{})
	}
	// 错误玩家提交当前轮次也不能改变叫抢状态。
	wrong := accessRequest(t, bySeat[(view.TurnSeat+1)%3], demopb.MsgId_C2S_LANDLORD_CALL, &demopb.LandlordCallRequest{Turn: view.Turn, Yes: true})
	if wrong.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatal("wrong user called landlord")
	}
	after := landlordProcessView(t, clients[0], demopb.MsgId_C2S_LANDLORD_SNAPSHOT, &demopb.LandlordSnapshotRequest{})
	if after.Turn != view.Turn || after.Multiplier != view.Multiplier {
		t.Fatal("illegal call changed bidding")
	}
	for view.Phase == "calling" || view.Phase == "robbing" {
		client := bySeat[view.TurnSeat]
		if view.Phase == "calling" {
			view = landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_CALL, &demopb.LandlordCallRequest{Turn: view.Turn, Yes: true})
		} else {
			view = landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_ROB, &demopb.LandlordRobRequest{Turn: view.Turn, Yes: true})
		}
	}
	if err := settlement.process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitAccessProcessListenerClosed(t, settlement.address)
	for actions := 0; view.Phase == "playing" && actions < 400; actions++ {
		client := bySeat[view.TurnSeat]
		view = landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_SNAPSHOT, &demopb.LandlordSnapshotRequest{})
		cards := []int64{}
		for _, card := range view.Hand {
			if len(view.LastCards) == 0 || len(view.LastCards) == 1 && landlordDemoRank(card) > landlordDemoRank(view.LastCards[0]) {
				cards = []int64{card}
				break
			}
		}
		view = landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_PLAY, &demopb.LandlordPlayRequest{Turn: view.Turn, Cards: cards})
	}
	if view.Phase != "finished" || view.GetSettlement().GetStatus() != "pending" {
		t.Fatalf("round did not reach pending settlement: %v", view)
	}
	return clients, view
}

// verifyLandlordRecovery 覆盖真实局末保存后重启，以及 SQL 已提交而游戏未确认两种路径，不重发出牌。
func verifyLandlordRecovery(t *testing.T, ctx context.Context, gates []string, players []accessDemoPlayer, db *sql.DB,
	ids *[]string, landlord, settlement accessNodeProcess, redisAddress string) {
	t.Helper()
	for _, scenario := range []struct {
		name                                           string
		restartLandlord, alreadyCommitted, capBalances bool
	}{
		{"结算节点中断后恢复", false, false, false},
		{"地主节点重启补交", true, false, false},
		{"已入账未确认后地主重启", true, true, false},
		{"余额不足封顶并向客户端返回实际扣返", false, false, true},
	} {
		before := map[uint64]struct {
			amount  int64
			version uint64
		}{}
		for _, p := range players {
			var b struct {
				amount  int64
				version uint64
			}
			if err := db.QueryRowContext(ctx, "SELECT amount,version FROM silver_balances WHERE user_id = ?", p.id).Scan(&b.amount, &b.version); err != nil {
				t.Fatal(err)
			}
			before[p.id] = b
		}
		clients, view := playAccessLandlordRound(t, gates, players, settlement)
		operationID := view.Settlement.OperationId
		*ids = append(*ids, operationID)
		var saved landlordSavedResult
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			data := landlordRedis(t, ctx, redisAddress, "HGET", accessRedisKey, operationID)
			if len(data) > 0 {
				if err := json.Unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if saved.ID != operationID || len(saved.Changes) != 3 || saved.Multiplier != 8 {
			t.Fatalf("pending result not saved: %+v", saved)
		}
		if scenario.capBalances {
			// 等结算服停止且原请求已保存后，模拟其他业务扣款，构造局末余额不足。
			for _, change := range saved.Changes {
				if change.Debit == 0 {
					continue
				}
				if _, err := db.ExecContext(ctx, "UPDATE silver_balances SET amount=1, version=version+1 WHERE user_id=?", change.UserID); err != nil {
					t.Fatal(err)
				}
				balance := before[change.UserID]
				balance.amount = 1
				balance.version++
				before[change.UserID] = balance
			}
		}
		type actualChange struct{ debit, credit int64 }
		actual := map[uint64]actualChange{}
		var paid int64
		winners := []uint64{}
		for _, change := range saved.Changes {
			if change.Debit > 0 {
				debit := min(change.Debit, before[change.UserID].amount)
				actual[change.UserID] = actualChange{debit: debit}
				paid += debit
			} else {
				winners = append(winners, change.UserID)
			}
		}
		sort.Slice(winners, func(i, j int) bool { return winners[i] > winners[j] })
		for i, uid := range winners {
			credit := paid / int64(len(winners))
			if int64(i) < paid%int64(len(winners)) {
				credit++
			}
			actual[uid] = actualChange{credit: credit}
		}
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", operationID).Scan(&count); err != nil || count != 0 {
			t.Fatal("settlement down but operation was stored", err)
		}
		if scenario.restartLandlord {
			if err := landlord.process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitAccessProcessListenerClosed(t, landlord.address)
		}
		settlement.process = startAccessProcess(t, settlement.binary, []string{"-config", settlement.config}, settlement.address)
		if scenario.alreadyCommitted {
			changes := []any{}
			for _, c := range saved.Changes {
				changes = append(changes, map[string]any{"user_id": c.UserID, "reason": "game_win_loss", "debit": c.Debit, "credit": c.Credit})
			}
			// 使用真实原请求构造已提交/游戏未确认边界，不直接修改账本。
			accessSettlementWrite(t, ctx, settlement.address, 7, map[string]any{
				"version": uint64(2), "operation_id": operationID, "business": "landlord", "business_ref": operationID, "changes": changes,
				"detail": map[string]any{"rule": "landlord-demo-v1", "base": int64(10), "multiplier": saved.Multiplier, "landlord_user_id": saved.Changes[saved.Landlord].UserID, "winner_user_id": saved.Changes[saved.Winner].UserID},
			})
		}
		if scenario.restartLandlord {
			landlord.process = startAccessProcess(t, landlord.binary, []string{"-config", landlord.config}, landlord.address)
		}
		// 受控验收集中查 SQL，客户端不通过公开协议逐人查账。
		accessWaitCommitted(t, ctx, db, operationID)
		deadline = time.Now().Add(5 * time.Second)
		for len(landlordRedis(t, ctx, redisAddress, "HGET", accessRedisKey, operationID)) > 0 {
			if time.Now().After(deadline) {
				t.Fatal("confirmed landlord pending was not cleared")
			}
			time.Sleep(20 * time.Millisecond)
		}
		for _, c := range saved.Changes {
			var amount int64
			var version uint64
			if err := db.QueryRowContext(ctx, "SELECT amount,version FROM silver_balances WHERE user_id = ?", c.UserID).Scan(&amount, &version); err != nil {
				t.Fatal(err)
			}
			b := before[c.UserID]
			change := actual[c.UserID]
			wantVersion := b.version
			if change.debit != 0 || change.credit != 0 {
				wantVersion++
			}
			if amount != b.amount-change.debit+change.credit || version != wantVersion {
				t.Fatalf("recovery repeated or lost funds: uid=%d amount=%d version=%d", c.UserID, amount, version)
			}
		}
		if scenario.capBalances {
			for index, client := range clients {
				view := landlordProcessView(t, client, demopb.MsgId_C2S_LANDLORD_SNAPSHOT, &demopb.LandlordSnapshotRequest{})
				uid := players[index].id
				change, balance := actual[uid], before[uid]
				version := balance.version
				if change.debit != 0 || change.credit != 0 {
					version++
				}
				if view.Settlement == nil || view.Settlement.Status != "committed" || view.Settlement.Debit != change.debit || view.Settlement.Credit != change.credit || view.Settlement.Balance != balance.amount-change.debit+change.credit || view.Settlement.Version != version {
					t.Fatalf("客户端实际扣返不正确: %+v", view.Settlement)
				}
			}
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", operationID).Scan(&count); err != nil || count != 1 {
			t.Fatal("recovered group operation not unique", err)
		}
		for _, client := range clients {
			if !scenario.restartLandlord {
				accessProcessResponse(t, client, demopb.MsgId_C2S_LANDLORD_LEAVE, &demopb.LandlordLeaveRequest{}, &demopb.LandlordLeaveResponse{})
			}
			_ = client.Close()
		}
		t.Logf("斗地主恢复：%s；原 ID 唯一入账，实际扣返、余额与版本正确，Redis pending 清除", scenario.name)
	}
}
