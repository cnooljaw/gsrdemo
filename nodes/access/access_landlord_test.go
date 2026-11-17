package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
)

// TestAccessLandlordThreeClientsLiveMySQL 保护三个真实 TS 客户端跨接入随机同桌，完成叫抢、出牌及一次原子三人入账。
func TestAccessLandlordThreeClientsLiveMySQL(t *testing.T) {
	runAccessClientsLiveMySQL(t, 3, 2, "landlord")
}

type landlordCLIEvent struct {
	Event string          `json:"event"`
	Value json.RawMessage `json:"value"`
}
type landlordCLIView struct {
	Room    string   `json:"roomName"`
	Phase   string   `json:"phase"`
	Hand    []string `json:"hand"`
	Players []struct {
		UserID    string          `json:"userId"`
		Seat      string          `json:"seat"`
		Remaining string          `json:"remaining"`
		Hand      json.RawMessage `json:"hand"`
	} `json:"players"`
	Seat string `json:"seat"`
}
type landlordCLIReceipt struct {
	ID      string `json:"operationId"`
	Status  string `json:"status"`
	Debit   string `json:"debit"`
	Credit  string `json:"credit"`
	Balance string `json:"balance"`
	Version string `json:"version"`
}

func verifyLandlordCLI(t *testing.T, ctx context.Context, players []accessDemoPlayer, clientErrors []error, db *sql.DB, ids *[]string) {
	t.Helper()
	// 先登记本用例的局身份，任何后续断言失败也能清理已经执行的结算。
	rooms := map[string]bool{}
	for _, player := range players {
		for _, line := range bytes.Split(player.output.Bytes(), []byte{'\n'}) {
			var event landlordCLIEvent
			if json.Unmarshal(line, &event) != nil || event.Event != "landlord_enter" {
				continue
			}
			var view landlordCLIView
			if json.Unmarshal(event.Value, &view) == nil && view.Room != "" && !rooms[view.Room] {
				rooms[view.Room] = true
				*ids = append(*ids, "landlord-"+view.Room)
			}
		}
	}
	if len(rooms) != 1 {
		t.Fatalf("三个客户端未随机同桌: %v", rooms)
	}
	var totalDelta int64
	var operationID string
	var firstPlays [][]byte
	bidCount, playCount := 0, 0
	for index, player := range players {
		if clientErrors[index] != nil {
			t.Fatalf("斗地主客户端 %d: %v %s", index, clientErrors[index], player.output.Bytes())
		}
		var receipt landlordCLIReceipt
		var plays [][]byte
		ownActions, ownBroadcasts, deals := 0, 0, 0
		found, left := false, false
		for _, line := range bytes.Split(player.output.Bytes(), []byte{'\n'}) {
			var event landlordCLIEvent
			if json.Unmarshal(line, &event) != nil {
				continue
			}
			switch event.Event {
			case "landlord_enter", "landlord_snapshot":
				var view landlordCLIView
				if err := json.Unmarshal(event.Value, &view); err != nil {
					t.Fatal(err)
				}
				for _, other := range view.Players {
					if len(other.Hand) != 0 {
						t.Fatal("快照泄露其他玩家手牌")
					}
				}
				if view.Phase == "playing" {
					own := -1
					for _, other := range view.Players {
						if other.UserID == strconv.FormatUint(player.id, 10) {
							n, err := strconv.Atoi(other.Remaining)
							if err != nil {
								t.Fatal(err)
							}
							own = n
						}
					}
					if own != len(view.Hand) {
						t.Fatalf("本人手牌数量与快照不符: %s", event.Value)
					}
				}
			case "landlord_bid":
				bidCount++
			case "landlord_action":
				playCount++
				ownActions++
			case "landlord_event_deal":
				var deal struct {
					Hand []string `json:"hand"`
				}
				if err := json.Unmarshal(event.Value, &deal); err != nil || len(deal.Hand) != 17 {
					t.Fatalf("发牌事件没有本人手牌: %s", event.Value)
				}
				deals++
			case "landlord_event_play":
				var played struct {
					UserID string          `json:"userId"`
					Hand   json.RawMessage `json:"hand"`
				}
				if err := json.Unmarshal(event.Value, &played); err != nil || played.UserID == "" || len(played.Hand) != 0 {
					t.Fatalf("非法出牌广播: %s", event.Value)
				}
				if played.UserID == strconv.FormatUint(player.id, 10) {
					ownBroadcasts++
				}
				plays = append(plays, append([]byte(nil), event.Value...))
			case "landlord_settlement":
				if err := json.Unmarshal(event.Value, &receipt); err != nil {
					t.Fatal(err)
				}
				found = true
			case "landlord_operation":
				t.Fatal("客户端在财务终态后仍重复查账")
			case "landlord_leave":
				left = true
			}
		}
		if deals != 1 || ownBroadcasts != ownActions || len(plays) == 0 {
			t.Fatalf("广播不完整: deals=%d own actions=%d own broadcasts=%d all plays=%d", deals, ownActions, ownBroadcasts, len(plays))
		}
		if index == 0 {
			firstPlays = plays
		} else if len(plays) != len(firstPlays) {
			t.Fatalf("三人收到的出牌次数不同: %d != %d", len(plays), len(firstPlays))
		} else {
			for i := range plays {
				if !bytes.Equal(plays[i], firstPlays[i]) {
					t.Fatalf("出牌广播内容或顺序不一致: %s / %s", plays[i], firstPlays[i])
				}
			}
		}
		if !found || !left || receipt.Status != "committed" || receipt.ID == "" {
			t.Fatalf("未完成整局: %s", player.output.Bytes())
		}
		if operationID == "" {
			operationID = receipt.ID
		}
		if operationID != receipt.ID {
			t.Fatal("三个客户端未共享同一局结算")
		}
		debit, err := strconv.ParseInt(receipt.Debit, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		credit, err := strconv.ParseInt(receipt.Credit, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		var amount int64
		var version uint64
		if err := db.QueryRowContext(ctx, "SELECT amount,version FROM silver_balances WHERE user_id = ?", player.id).Scan(&amount, &version); err != nil {
			t.Fatal(err)
		}
		if amount != 10000-debit+credit || version != 2 || receipt.Balance != strconv.FormatInt(amount, 10) || receipt.Version != "2" {
			t.Fatalf("三人结算不一致: amount=%d version=%d receipt=%+v", amount, version, receipt)
		}
		totalDelta += credit - debit
		t.Logf("斗地主玩家 %d：扣 %d 返 %d，余额 %d，版本 %d", index+1, debit, credit, amount, version)
	}
	if totalDelta != 0 || bidCount != 4 || playCount == 0 {
		t.Fatalf("资金或叫抢流程错误: delta=%d bids=%d plays=%d", totalDelta, bidCount, playCount)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settlement_operations WHERE operation_id = ?", operationID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("多人结算不唯一: count=%d err=%v", count, err)
	}
	t.Logf("2 个 access、3 个 TS CLI：随机同桌，1 次叫地主 + 3 次抢地主，%d 次出牌/不出，1 笔三人原子结算", playCount)
}
