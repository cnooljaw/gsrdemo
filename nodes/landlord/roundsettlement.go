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
	operation      *landlordOperation
	replyName      cluster.ServiceName
	retryAt        time.Time
	fault          error
	cleanupPending bool // 终态已确认但 Redis 待记录尚未删除，不影响客户端结果。
}

func (round *roundSettlement) prepareOperation(game attackGame) {
	if game.Phase == "finished" && game.Winner >= 0 && round.operation == nil {
		round.operation = operationForGame(game)
	}
}

func (round *roundSettlement) pending() bool {
	return round.operation != nil && round.operation.Status == "pending"
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

// landlordOperation 保存本局固定的资金请求和后续确认结果；Redis 只保存请求字段。
type landlordOperation struct {
	ID         string                     `json:"operation_id"`
	Multiplier int64                      `json:"multiplier"`
	Landlord   int                        `json:"landlord_seat"`
	Winner     int                        `json:"winner_seat"`
	Changes    [3]landlordChange          `json:"changes"`
	Status     string                     `json:"-"`
	Reason     string                     `json:"-"`
	Balances   map[uint64]landlordBalance `json:"-"`
}

func operationForGame(game attackGame) *landlordOperation {
	operation := &landlordOperation{ID: "landlord-" + game.Name, Status: "pending", Multiplier: game.Multiplier, Landlord: game.Landlord, Winner: game.Winner}
	for i, p := range game.Players {
		operation.Changes[i] = operation.change(i, p.UserID)
	}
	return operation
}

func (operation *landlordOperation) change(seat int, uid uint64) landlordChange {
	change := landlordChange{UserID: uid}
	amount := baseSilver * operation.Multiplier
	if seat == operation.Landlord {
		amount *= 2
	}
	if (seat == operation.Landlord) == (operation.Winner == operation.Landlord) {
		change.Credit = amount
	} else {
		change.Debit = amount
	}
	return change
}

func (operation *landlordOperation) valid() bool {
	if !validIdentity(operation.ID) || operation.Landlord < 0 || operation.Landlord > 2 || operation.Winner < 0 || operation.Winner > 2 ||
		operation.Multiplier < 1 || operation.Multiplier > maxMultiplier || operation.Multiplier&(operation.Multiplier-1) != 0 {
		return false
	}
	seen := map[uint64]bool{}
	for i, c := range operation.Changes {
		if c.UserID == 0 || seen[c.UserID] || c != operation.change(i, c.UserID) {
			return false
		}
		seen[c.UserID] = true
	}
	return true
}

func (operation *landlordOperation) wire() map[string]any {
	changes := make([]any, 0, 3)
	for _, c := range operation.Changes {
		changes = append(changes, map[string]any{"user_id": c.UserID, "reason": "game_win_loss", "debit": c.Debit, "credit": c.Credit})
	}
	return map[string]any{"version": uint64(2), "operation_id": operation.ID, "business": "landlord", "business_ref": operation.ID,
		"changes": changes, "detail": map[string]any{"rule": "landlord-demo-v1", "base": baseSilver, "multiplier": operation.Multiplier, "landlord_user_id": operation.Changes[operation.Landlord].UserID, "winner_user_id": operation.Changes[operation.Winner].UserID}}
}

// snapshotFor 返回玩家自己的结算快照，不暴露可变的内部记录。
func (round *roundSettlement) snapshotFor(uid uint64) map[string]any {
	operation := round.operation
	if operation == nil {
		return nil
	}
	view := map[string]any{"operation_id": operation.ID, "status": operation.Status, "reason": operation.Reason}
	for _, c := range operation.Changes {
		if c.UserID == uid {
			view["debit"], view["credit"] = c.Debit, c.Credit
		}
	}
	if balance, ok := operation.Balances[uid]; ok {
		view["balance"], view["version"] = balance.Amount, balance.Version
	}
	return view
}

func (round *roundSettlement) submit(self gsr.ServiceContext) error {
	if round.fault != nil || time.Now().Before(round.retryAt) {
		return nil
	}
	// Redis 或本地发送失败也限频；SQL 结果未知时仍保留原请求。
	round.retryAt = time.Now().Add(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	err := round.journal.save(ctx, round.operation)
	cancel()
	if err != nil {
		return err
	}
	if err := self.Cluster().Send(round.node, "settlement-submit", 7, round.operation.wire(), "landlord", string(round.replyName)); err != nil {
		return err
	}
	round.retryAt = time.Now().Add(5*time.Second + rand.N(500*time.Millisecond))
	return nil
}

// applyReceipt 只应用当前操作的完整终态；重复或其他牌局回执不改变状态。
func (round *roundSettlement) applyReceipt(id string, fields map[string]any, code string) (bool, error) {
	if !round.pending() || round.operation.ID != id || round.fault != nil {
		return false, nil
	}
	if code != "" {
		err := runtime.NewStatusError(runtime.ErrorClassDomain, runtime.ErrorCode(code), "settlement request did not complete")
		if code != "settlement.busy" && code != "settlement.store_unavailable" {
			round.fault = err
		}
		return false, err
	}
	operation := round.operation
	results, ok := fields["results"].([]any)
	if !ok || len(results) != 1 {
		return false, errLandlordInput
	}
	item, ok := results[0].(map[string]any)
	if !ok || item["operation_id"] != operation.ID {
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
		for _, c := range operation.Changes {
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
	operation.Status, operation.Reason, operation.Balances = status, reason, balances
	round.cleanupPending = true
	round.retryAt = time.Time{}
	return true, nil
}

func (round *roundSettlement) deletePending() error {
	if time.Now().Before(round.retryAt) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if err := round.journal.client.HDel(ctx, round.journal.key, round.operation.ID).Err(); err != nil {
		// 从本次等待结束后退避，队列中积压的 Tick 只检查时间，不再次等待 Redis。
		round.retryAt = time.Now().Add(time.Second)
		return err
	}
	round.cleanupPending = false
	round.retryAt = time.Time{}
	return nil
}
