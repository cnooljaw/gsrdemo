package main

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
)

const (
	settlementCallTimeout  = 2 * time.Second
	settlementRedisTimeout = 250 * time.Millisecond
	settlementRetryDelay   = time.Second
)

// roundSettlement 只拥有本桌资金请求和提交进度，由 AttackService 串行调用。
type roundSettlement struct {
	node           cluster.NodeID
	journal        landlordJournal
	operation      *landlordOperation
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
	Actual     map[uint64]landlordChange  `json:"-"`
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
	view["debit"], view["credit"] = int64(0), int64(0)
	if change, ok := operation.Actual[uid]; ok {
		view["debit"], view["credit"] = change.Debit, change.Credit
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
	// 从本次等待结束后退避，积压的 Pulse 不得立即重提未知结果。
	defer func() {
		if round.pending() {
			round.retryAt = time.Now().Add(settlementRetryDelay)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), settlementRedisTimeout)
	err := round.journal.save(ctx, round.operation)
	cancel()
	if err != nil {
		round.pauseOnPermanentError(err)
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), settlementCallTimeout)
	value, err := self.Cluster().Call(ctx, round.node, settlementwrite.ServiceName, settlementwrite.SettleEqualGroupCommand, round.operation.wire())
	cancel()
	if err != nil {
		round.pauseOnPermanentError(err)
		return err
	}
	return round.applyReceipt(value)
}

// pauseOnPermanentError 只用于保存和 Call 报错；坏回执仍属于未确认结果，必须允许重提原请求。
func (round *roundSettlement) pauseOnPermanentError(err error) {
	var status *runtime.StatusError
	permanentDomain := errors.As(err, &status) && status.Class == runtime.ErrorClassDomain && status.Code != "settlement.store_unavailable"
	if permanentDomain || errors.Is(err, runtime.ErrUnknownCommand) || errors.Is(err, runtime.ErrInvalidCommandArguments) {
		round.fault = err
	}
}

// applyReceipt 只接受当前操作的完整终态；格式错误时保留原请求，不假报已完成。
func (round *roundSettlement) applyReceipt(value any) error {
	if !round.pending() || round.fault != nil {
		return nil
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return errLandlordInput
	}
	operation := round.operation
	results, ok := fields["results"].([]any)
	if !ok || len(results) != 1 {
		return errLandlordInput
	}
	item, ok := results[0].(map[string]any)
	if !ok || item["operation_id"] != operation.ID {
		return errLandlordInput
	}
	status, ok := item["status"].(string)
	if !ok || status != "committed" {
		return errLandlordInput
	}
	reason, ok := item["reason"].(string)
	if !ok || reason != "" {
		return errLandlordInput
	}
	changes, ok := item["actual_changes"].([]any)
	if !ok || len(changes) != len(operation.Changes) {
		return errLandlordInput
	}
	actual := make(map[uint64]landlordChange, len(changes))
	var debitTotal, creditTotal int64
	for _, value := range changes {
		row, ok := value.(map[string]any)
		if !ok {
			return errLandlordInput
		}
		uid, uidOK := row["user_id"].(uint64)
		debit, debitOK := row["debit"].(int64)
		credit, creditOK := row["credit"].(int64)
		if !uidOK || !debitOK || !creditOK || debit < 0 || credit < 0 || debitTotal > math.MaxInt64-debit || creditTotal > math.MaxInt64-credit {
			return errLandlordInput
		}
		var requested landlordChange
		for _, original := range operation.Changes {
			if original.UserID == uid {
				requested = original
				break
			}
		}
		if requested.UserID == 0 || actual[uid].UserID != 0 || debit > requested.Debit || credit > requested.Credit {
			return errLandlordInput
		}
		actual[uid] = landlordChange{UserID: uid, Debit: debit, Credit: credit}
		debitTotal += debit
		creditTotal += credit
	}
	if debitTotal != creditTotal {
		return errLandlordInput
	}
	values, ok := fields["balances"].([]any)
	if !ok || len(values) != 3 {
		return errLandlordInput
	}
	balances := map[uint64]landlordBalance{}
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok {
			return errLandlordInput
		}
		uid, uidOK := row["user_id"].(uint64)
		amount, amountOK := row["amount"].(int64)
		version, versionOK := row["version"].(uint64)
		expected := false
		for _, c := range operation.Changes {
			expected = expected || c.UserID == uid
		}
		if !uidOK || !amountOK || !versionOK || amount < 0 || !expected {
			return errLandlordInput
		}
		if _, duplicate := balances[uid]; duplicate {
			return errLandlordInput
		}
		balances[uid] = landlordBalance{amount, version}
	}
	operation.Status, operation.Reason, operation.Balances, operation.Actual = status, reason, balances, actual
	round.cleanupPending = true
	round.retryAt = time.Time{}
	return nil
}

func (round *roundSettlement) deletePending() error {
	if time.Now().Before(round.retryAt) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), settlementRedisTimeout)
	defer cancel()
	if err := round.journal.client.HDel(ctx, round.journal.key, round.operation.ID).Err(); err != nil {
		// 从本次等待结束后退避，队列中积压的 Tick 只检查时间，不再次等待 Redis。
		round.retryAt = time.Now().Add(settlementRetryDelay)
		return err
	}
	round.cleanupPending = false
	round.retryAt = time.Time{}
	return nil
}
