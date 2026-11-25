package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// TestAccessFourDhmjClientsLiveMySQL 保护四个真实 CLI 从发牌到本人结算，核对实际钱包和唯一账务。
func TestAccessFourDhmjClientsLiveMySQL(t *testing.T) {
	runAccessClientsLiveMySQL(t, 4, 1, "dhmj", dhmjOutageNone)
}

func verifyDhmjCLI(t *testing.T, ctx context.Context, players []accessDemoPlayer, clientErrors []error, db *sql.DB, operations *[]string) {
	t.Helper()
	var paidTotal, receivedTotal, feeTotal int64
	ids := map[string]bool{}
	for i := range players {
		player := &players[i]
		var receipt struct {
			ID       string `json:"operationId"`
			UserID   uint64 `json:"userId,string"`
			Status   string `json:"status"`
			Fee      int64  `json:"fee,string"`
			Paid     int64  `json:"paid,string"`
			Received int64  `json:"received,string"`
			Balance  int64  `json:"balance,string"`
			Version  uint64 `json:"version,string"`
		}
		events := map[string]int{}
		for _, line := range bytes.Split(player.output.Bytes(), []byte{'\n'}) {
			var event struct {
				Event string
				Value json.RawMessage
			}
			if json.Unmarshal(line, &event) != nil {
				continue
			}
			events[event.Event]++
			if event.Event == "dhmj_settlement" {
				if err := json.Unmarshal(event.Value, &receipt); err != nil {
					t.Error(err)
				}
				if strings.HasPrefix(receipt.ID, "dhmj-") && !ids[receipt.ID] {
					ids[receipt.ID] = true
					*operations = append(*operations, receipt.ID)
				}
			}
		}
		if clientErrors[i] != nil || events["dhmj_event_deal"] != 1 || events["dhmj_event_round_end"] != 1 ||
			events["dhmj_settlement"] != 1 || events["dhmj_leave"] != 1 || receipt.Status != "committed" || receipt.UserID != player.id {
			output := player.output.String()
			t.Errorf("客户端 %d 未完成麻将：%v，事件 %v，末尾 %s", i, clientErrors[i], events, output[max(0, len(output)-1200):])
			continue
		}
		var amount int64
		var version uint64
		err := db.QueryRowContext(ctx, "SELECT amount, version FROM silver_balances WHERE user_id=?", player.id).Scan(&amount, &version)
		if err != nil || amount != receipt.Balance || version != receipt.Version || receipt.Fee != 50 ||
			amount != 10000-receipt.Fee-receipt.Paid+receipt.Received {
			t.Errorf("客户端 %d 的回执与钱包不一致：%+v，%d/%d %v", i, receipt, amount, version, err)
		}
		paidTotal += receipt.Paid
		receivedTotal += receipt.Received
		feeTotal += receipt.Fee
	}
	if t.Failed() {
		return
	}
	if len(ids) != 1 || paidTotal != receivedTotal || feeTotal != 200 {
		t.Fatalf("四人未完成同一笔守恒结算：%v，实付 %d，实收 %d，费用 %d", ids, paidTotal, receivedTotal, feeTotal)
	}
	for id := range ids {
		var debit, credit int64
		err := db.QueryRowContext(ctx, "SELECT debit,credit FROM settlement_operations WHERE operation_id=?", id).Scan(&debit, &credit)
		if err != nil || debit != paidTotal+feeTotal || credit != receivedTotal {
			t.Fatalf("唯一流水与回执不一致：%s %d/%d %v", id, debit, credit, err)
		}
	}
}
