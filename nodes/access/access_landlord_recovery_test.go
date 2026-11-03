package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os/exec"
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
		accessProcessResponse(t, client, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: player.username, Password: player.password}, &login)
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
	for _, alreadyCommitted := range []bool{false, true} {
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
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", operationID).Scan(&count); err != nil || count != 0 {
			t.Fatal("settlement down but operation was stored", err)
		}
		if err := landlord.process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitAccessProcessListenerClosed(t, landlord.address)
		settlement.process = startAccessProcess(t, settlement.binary, []string{"-config", settlement.config}, settlement.address)
		if alreadyCommitted {
			changes := []any{}
			for _, c := range saved.Changes {
				changes = append(changes, map[string]any{"user_id": c.UserID, "reason": "game_win_loss", "debit": c.Debit, "credit": c.Credit})
			}
			// 使用真实原请求构造已提交/游戏未确认边界，不直接修改账本。
			accessSettlementWrite(t, ctx, settlement.address, operationID, 7, map[string]any{
				"version": uint64(2), "operation_id": operationID, "business": "landlord", "business_ref": operationID, "changes": changes,
				"detail": map[string]any{"rule": "landlord-demo-v1", "base": int64(10), "multiplier": saved.Multiplier, "landlord_user_id": saved.Changes[saved.Landlord].UserID, "winner_user_id": saved.Changes[saved.Winner].UserID},
			})
		}
		landlord.process = startAccessProcess(t, landlord.binary, []string{"-config", landlord.config}, landlord.address)
		for _, client := range clients {
			deadline = time.Now().Add(5 * time.Second)
			found := false
			for time.Now().Before(deadline) {
				var receipt demopb.LandlordOperationResponse
				accessProcessResponse(t, client, demopb.MsgId_C2S_LANDLORD_OPERATION, &demopb.LandlordOperationRequest{OperationId: operationID}, &receipt)
				if receipt.Found {
					if receipt.Status != "committed" {
						t.Fatalf("recovery rejected: %v", &receipt)
					}
					found = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !found {
				t.Fatal("saved landlord result not recovered")
			}
		}
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
			if amount != b.amount-c.Debit+c.Credit || version != b.version+1 {
				t.Fatalf("recovery repeated or lost funds: uid=%d amount=%d version=%d", c.UserID, amount, version)
			}
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", operationID).Scan(&count); err != nil || count != 1 {
			t.Fatal("recovered group operation not unique", err)
		}
		for _, client := range clients {
			_ = client.Close()
		}
		t.Logf("斗地主恢复：已保存三人局末结果，SQL 预先提交=%v；节点重启后原 ID 唯一入账，余额/版本各变化一次，Redis pending 清除", alreadyCommitted)
	}
}
