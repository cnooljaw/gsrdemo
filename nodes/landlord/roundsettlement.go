package main

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// roundSettlement 只拥有本桌资金请求和提交进度，由 AttackService 串行调用。
type roundSettlement struct {
	node           cluster.NodeID
	journal        landlordJournal
	result         *landlordResult
	replyName      cluster.ServiceName
	retryAt        time.Time
	fault          error
	cleanupPending bool // 终态已确认但 Redis 待记录尚未删除，不影响客户端结果。
}

func (round *roundSettlement) prepare(game attackGame) {
	if game.Phase == "finished" && game.Winner >= 0 && round.result == nil {
		round.result = resultForGame(game)
	}
}

func (round *roundSettlement) pending() bool {
	return round.result != nil && round.result.Status == "pending"
}

func (round *roundSettlement) view(uid uint64) map[string]any {
	if round.result == nil {
		return nil
	}
	return round.result.view(uid)
}

type landlordChange struct {
	UserID uint64 `json:"user_id"`
	Debit  int64  `json:"debit"`
	Credit int64  `json:"credit"`
}
type landlordBalance struct {
	Amount  int64
	Version uint64
}

// landlordResult 是局末不可变的资金请求和确认结果；落 Redis 只保存请求，不保存连接地址或手牌。
type landlordResult struct {
	ID         string                     `json:"operation_id"`
	Multiplier int64                      `json:"multiplier"`
	Landlord   int                        `json:"landlord_seat"`
	Winner     int                        `json:"winner_seat"`
	Changes    [3]landlordChange          `json:"changes"`
	Status     string                     `json:"-"`
	Reason     string                     `json:"-"`
	Balances   map[uint64]landlordBalance `json:"-"`
}

func resultForGame(game attackGame) *landlordResult {
	result := &landlordResult{ID: "landlord-" + game.Name, Status: "pending", Multiplier: game.Multiplier, Landlord: game.Landlord, Winner: game.Winner}
	for i, p := range game.Players {
		result.Changes[i] = result.change(i, p.UserID)
	}
	return result
}

func (result *landlordResult) change(seat int, uid uint64) landlordChange {
	change := landlordChange{UserID: uid}
	amount := baseSilver * result.Multiplier
	if seat == result.Landlord {
		amount *= 2
	}
	if (seat == result.Landlord) == (result.Winner == result.Landlord) {
		change.Credit = amount
	} else {
		change.Debit = amount
	}
	return change
}

func (result *landlordResult) valid() bool {
	if !validIdentity(result.ID) || result.Landlord < 0 || result.Landlord > 2 || result.Winner < 0 || result.Winner > 2 ||
		result.Multiplier < 1 || result.Multiplier > maxMultiplier || result.Multiplier&(result.Multiplier-1) != 0 {
		return false
	}
	seen := map[uint64]bool{}
	for i, c := range result.Changes {
		if c.UserID == 0 || seen[c.UserID] || c != result.change(i, c.UserID) {
			return false
		}
		seen[c.UserID] = true
	}
	return true
}

func (result *landlordResult) wire() map[string]any {
	changes := make([]any, 0, 3)
	for _, c := range result.Changes {
		changes = append(changes, map[string]any{"user_id": c.UserID, "reason": "game_win_loss", "debit": c.Debit, "credit": c.Credit})
	}
	return map[string]any{"version": uint64(2), "operation_id": result.ID, "business": "landlord", "business_ref": result.ID,
		"changes": changes, "detail": map[string]any{"rule": "landlord-demo-v1", "base": baseSilver, "multiplier": result.Multiplier, "landlord_user_id": result.Changes[result.Landlord].UserID, "winner_user_id": result.Changes[result.Winner].UserID}}
}

func (result *landlordResult) view(uid uint64) map[string]any {
	view := map[string]any{"operation_id": result.ID, "status": result.Status, "reason": result.Reason}
	for _, c := range result.Changes {
		if c.UserID == uid {
			view["debit"], view["credit"] = c.Debit, c.Credit
		}
	}
	if balance, ok := result.Balances[uid]; ok {
		view["balance"], view["version"] = balance.Amount, balance.Version
	}
	return view
}

func (round *roundSettlement) settle(self gsr.ServiceContext) error {
	if round.fault != nil || time.Now().Before(round.retryAt) {
		return nil
	}
	// Redis 或本地发送失败也限频；SQL 结果未知时仍保留原请求。
	round.retryAt = time.Now().Add(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	err := round.journal.save(ctx, round.result)
	cancel()
	if err != nil {
		return err
	}
	if err := self.Cluster().Send(round.node, "settlement-submit", 7, round.result.wire(), "landlord", string(round.replyName)); err != nil {
		return err
	}
	round.retryAt = time.Now().Add(5*time.Second + rand.N(500*time.Millisecond))
	return nil
}

func (round *roundSettlement) accept(id string, fields map[string]any, code string) (bool, error) {
	if !round.pending() || round.result.ID != id || round.fault != nil {
		return false, nil
	}
	if code != "" {
		err := runtime.NewStatusError(runtime.ErrorClassDomain, runtime.ErrorCode(code), "settlement request did not complete")
		if code != "settlement.busy" && code != "settlement.store_unavailable" {
			round.fault = err
		}
		return false, err
	}
	result := round.result
	results, ok := fields["results"].([]any)
	if !ok || len(results) != 1 {
		return false, errLandlordInput
	}
	item, ok := results[0].(map[string]any)
	if !ok || item["operation_id"] != result.ID {
		return false, errLandlordInput
	}
	status, ok := item["status"].(string)
	if !ok || (status != "committed" && status != "rejected") {
		return false, errLandlordInput
	}
	reason, ok := item["reason"].(string)
	if !ok {
		return false, errLandlordInput
	}
	values, ok := fields["balances"].([]any)
	if !ok || len(values) != 3 {
		return false, errLandlordInput
	}
	balances := map[uint64]landlordBalance{}
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok {
			return false, errLandlordInput
		}
		uid, uidOK := row["user_id"].(uint64)
		amount, amountOK := row["amount"].(int64)
		version, versionOK := row["version"].(uint64)
		expected := false
		for _, c := range result.Changes {
			expected = expected || c.UserID == uid
		}
		if !uidOK || !amountOK || !versionOK || amount < 0 || !expected {
			return false, errLandlordInput
		}
		if _, duplicate := balances[uid]; duplicate {
			return false, errLandlordInput
		}
		balances[uid] = landlordBalance{amount, version}
	}
	result.Status, result.Reason, result.Balances = status, reason, balances
	round.cleanupPending = true
	return true, nil
}

func (round *roundSettlement) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := round.journal.client.HDel(ctx, round.journal.key, round.result.ID).Err(); err != nil {
		return err
	}
	round.cleanupPending = false
	return nil
}
