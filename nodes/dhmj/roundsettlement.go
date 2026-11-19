package main

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
)

const (
	journalTimeout       = 250 * time.Millisecond
	settlementRetryDelay = time.Second
)

type roomRules struct {
	Tier int   `yaml:"tier"`
	Base int64 `yaml:"base"`
	Fee  int64 `yaml:"fee"`
}

var defaultRoomRules = roomRules{Tier: 5, Base: 100, Fee: 50}

func (r roomRules) valid() bool {
	_, hasTier := scoreTiers[r.Tier]
	// 以最多 12 条付款、单条最大倍率 200×30 限制配置，确保费用与付款总额可入账。
	return hasTier && r.Base > 0 && r.Fee >= 0 && r.Fee <= math.MaxInt64/seatCount &&
		r.Base <= (math.MaxInt64-r.Fee*seatCount)/(seatCount*(seatCount-1)*200*30)
}

type roundOperation struct {
	ID        string            `json:"operation_id"`
	Users     [seatCount]uint64 `json:"users"`
	Rules     roomRules         `json:"rules"`
	Transfers []roundTransfer   `json:"transfers"`
}

type roundTransfer struct {
	From   int   `json:"from"`
	To     int   `json:"to"`
	Amount int64 `json:"amount"`
}

func newRoundOperation(users [seatCount]uint64, rules roomRules, end roundEnd) (roundOperation, error) {
	payments, err := roundPayments(roomScore{tier: rules.Tier, base: rules.Base}, end)
	if err != nil {
		return roundOperation{}, err
	}
	operation := roundOperation{ID: "dhmj-" + rand.Text(), Users: users, Rules: rules}
	for _, p := range payments {
		operation.Transfers = append(operation.Transfers, roundTransfer{p.from, p.to, p.amount})
	}
	if !operation.valid() {
		return roundOperation{}, errInvalidScore
	}
	return operation, nil
}

func (op roundOperation) valid() bool {
	if len(op.ID) == 0 || len(op.ID) > 128 || !op.Rules.valid() {
		return false
	}
	users := map[uint64]bool{}
	for _, uid := range op.Users {
		if uid == 0 || users[uid] {
			return false
		}
		users[uid] = true
	}
	total := op.Rules.Fee * seatCount
	var seen [seatCount][seatCount]bool
	for _, p := range op.Transfers {
		if !validSeat(p.From) || !validSeat(p.To) || p.From == p.To || seen[p.From][p.To] || p.Amount <= 0 || total > math.MaxInt64-p.Amount {
			return false
		}
		seen[p.From][p.To] = true
		total += p.Amount
	}
	return true
}

func (op roundOperation) wire() map[string]any {
	fees := make([]any, 0, seatCount)
	for _, user := range op.Users {
		fees = append(fees, map[string]any{"user_id": user, "amount": op.Rules.Fee})
	}
	transfers := make([]any, 0, len(op.Transfers))
	for _, p := range op.Transfers {
		transfers = append(transfers, map[string]any{"from_user_id": op.Users[p.From], "to_user_id": op.Users[p.To], "amount": p.Amount})
	}
	return map[string]any{
		"version": uint64(2), "operation_id": op.ID, "business": "dhmj", "business_ref": op.ID,
		"fees": fees, "transfers": transfers, "detail": map[string]any{"tier": int64(op.Rules.Tier), "base": op.Rules.Base},
	}
}

type roundSilver struct {
	UserID                       uint64
	Fee, Paid, Received, Balance int64
	Version                      uint64
}

func (s roundSilver) wire() map[string]any {
	return map[string]any{"user_id": s.UserID, "fee": s.Fee, "paid": s.Paid, "received": s.Received, "balance": s.Balance, "version": s.Version}
}

func (op roundOperation) receipt(value any) ([seatCount]roundSilver, error) {
	var result [seatCount]roundSilver
	fields, _ := value.(map[string]any)
	items, _ := fields["results"].([]any)
	if len(items) != 1 {
		return result, errInvalidReply
	}
	item, _ := items[0].(map[string]any)
	if item["operation_id"] != op.ID || item["status"] != "committed" || item["reason"] != "" {
		return result, errInvalidReply
	}
	seats := make(map[uint64]int, seatCount)
	for i, uid := range op.Users {
		seats[uid] = i
		result[i].UserID = uid
	}
	if !op.readTransfers(item["actual_transfers"], seats, &result) {
		return result, errInvalidReply
	}
	for _, rows := range []struct {
		value any
		apply func(map[string]any, *roundSilver) bool
	}{
		{item["actual_fees"], func(row map[string]any, player *roundSilver) bool {
			fee, ok := row["amount"].(int64)
			player.Fee = fee
			return ok && fee >= 0 && fee <= op.Rules.Fee
		}},
		{item["actual_changes"], func(row map[string]any, player *roundSilver) bool {
			return row["debit"] == player.Fee+player.Paid && row["credit"] == player.Received
		}},
		{fields["balances"], func(row map[string]any, player *roundSilver) bool {
			balance, amountOK := row["amount"].(int64)
			version, versionOK := row["version"].(uint64)
			player.Balance, player.Version = balance, version
			return amountOK && balance >= 0 && versionOK && version > 0
		}},
	} {
		list, _ := rows.value.([]any)
		if len(list) != seatCount {
			return result, errInvalidReply
		}
		var seen [seatCount]bool
		for _, value := range list {
			row, _ := value.(map[string]any)
			uid, _ := row["user_id"].(uint64)
			seat, ok := seats[uid]
			if !ok || seen[seat] || !rows.apply(row, &result[seat]) {
				return result, errInvalidReply
			}
			seen[seat] = true
		}
	}
	return result, nil
}

func (op roundOperation) readTransfers(value any, seats map[uint64]int, result *[seatCount]roundSilver) bool {
	edges, ok := value.([]any)
	if !ok || len(edges) != len(op.Transfers) {
		return false
	}
	var due [seatCount][seatCount]int64
	for _, p := range op.Transfers {
		due[p.From][p.To] = p.Amount
	}
	for _, value := range edges {
		row, _ := value.(map[string]any)
		from, _ := row["from_user_id"].(uint64)
		to, _ := row["to_user_id"].(uint64)
		a, payer := seats[from]
		b, recipient := seats[to]
		amount, valid := row["amount"].(int64)
		if !payer || !recipient || !valid || amount < 0 || due[a][b] == 0 || amount > due[a][b] {
			return false
		}
		due[a][b] = 0
		result[a].Paid += amount
		result[b].Received += amount
	}
	return true
}

type roundSettlement struct {
	operation roundOperation
	receipt   *[seatCount]roundSilver
	retryAt   time.Time
	cleared   bool
	paused    bool
	saved     bool
}

// advance 由牌桌或启动恢复 Service 串行调用。拿到回执后只重试清理。
func (s *roundSettlement) advance(self gsr.ServiceContext, journal settlementJournal) {
	if s.cleared || s.paused || time.Now().Before(s.retryAt) {
		return
	}
	if err := s.deliver(self, journal); err != nil {
		s.retryAt = time.Now().Add(settlementRetryDelay)
		var status *runtime.StatusError
		s.paused = errors.As(err, &status) && status.Class == runtime.ErrorClassDomain && status.Code != "settlement.store_unavailable" ||
			errors.Is(err, runtime.ErrUnknownCommand) || errors.Is(err, runtime.ErrInvalidCommandArguments)
		_ = self.Logger().Error("定海麻将结算未完成", "operation_id", s.operation.ID, "paused", s.paused, "error", err.Error())
	}
}

func (s *roundSettlement) deliver(self gsr.ServiceContext, journal settlementJournal) error {
	if s.receipt == nil {
		if !s.saved {
			ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
			err := journal.save(ctx, s.operation)
			cancel()
			if err != nil {
				return err
			}
			s.saved = true
		}
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		value, err := self.Cluster().Call(ctx, "settlement", settlementwrite.ServiceName, settlementwrite.SettleTransferGroupWithFeesCommand, s.operation.wire())
		cancel()
		if err != nil {
			return err
		}
		result, err := s.operation.receipt(value)
		if err != nil {
			return err
		}
		s.receipt = &result
	}
	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()
	if err := journal.remove(ctx, s.operation.ID); err != nil {
		return err
	}
	s.cleared = true
	return nil
}
