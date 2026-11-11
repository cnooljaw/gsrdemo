package main

import (
	"math"
	"sort"
)

// decodeEqualGroup 要求理论奖池按 UserID 降序分配余数、收支守恒且每名参与者只出现一次。
func decodeEqualGroup(request map[string]any) (settlementBatch, error) {
	batch, err := decodeGroupFields(request)
	if err != nil {
		return batch, err
	}
	op := &batch.Operations[0]
	seen := map[uint64]bool{}
	var debit, credit int64
	winners := []silverChange{}
	losers := 0
	for _, c := range op.Changes {
		if seen[c.UserID] || c.Debit > 0 && c.Credit > 0 {
			return settlementBatch{}, errInvalidSettlement
		}
		seen[c.UserID] = true
		if c.Debit > 0 {
			if debit > math.MaxInt64-c.Debit {
				return settlementBatch{}, errInvalidSettlement
			}
			debit += c.Debit
			losers++
		} else {
			if credit > math.MaxInt64-c.Credit {
				return settlementBatch{}, errInvalidSettlement
			}
			credit += c.Credit
			winners = append(winners, c)
		}
	}
	if len(winners) == 0 || losers == 0 || debit != credit {
		return settlementBatch{}, errInvalidSettlement
	}
	sort.Slice(winners, func(i, j int) bool { return winners[i].UserID > winners[j].UserID })
	share, remainder := debit/int64(len(winners)), debit%int64(len(winners))
	for i, winner := range winners {
		want := share
		if int64(i) < remainder {
			want++
		}
		if winner.Credit != want {
			return settlementBatch{}, errInvalidSettlement
		}
	}
	op.Policy = "equal"
	batch.Kind = "group-equal"
	batch.ID = derivedBatchID(batch.Kind, op.ID)
	return boundedBatch(batch, request)
}

// decodeTransferGroup 接受去重的付款关系，并为事务派生参与者；付款明细本身参与幂等摘要。
func decodeTransferGroup(request map[string]any) (settlementBatch, error) {
	if !exactFields(request, "version", "operation_id", "business", "business_ref", "transfers", "detail") || !validVersion(request) {
		return settlementBatch{}, errInvalidSettlement
	}
	values, ok := request["transfers"].([]any)
	if !ok || len(values) == 0 || len(values) > maxSettlementChanges {
		return settlementBatch{}, errInvalidSettlement
	}
	transfers := make([]settlementTransfer, 0, len(values))
	totals := map[uint64]settlementActual{}
	seen := map[[2]uint64]bool{}
	var amount int64
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok || !exactFields(row, "from_user_id", "to_user_id", "amount") {
			return settlementBatch{}, errInvalidSettlement
		}
		from, fromOK := row["from_user_id"].(uint64)
		to, toOK := row["to_user_id"].(uint64)
		sum, sumOK := row["amount"].(int64)
		if !fromOK || !toOK || !sumOK || from == 0 || to == 0 || from == to || sum <= 0 || seen[[2]uint64{from, to}] || amount > math.MaxInt64-sum {
			return settlementBatch{}, errInvalidSettlement
		}
		seen[[2]uint64{from, to}] = true
		amount += sum
		a := totals[from]
		a.UserID = from
		if a.Debit > math.MaxInt64-sum {
			return settlementBatch{}, errInvalidSettlement
		}
		a.Debit += sum
		totals[from] = a
		a = totals[to]
		a.UserID = to
		if a.Credit > math.MaxInt64-sum {
			return settlementBatch{}, errInvalidSettlement
		}
		a.Credit += sum
		totals[to] = a
		transfers = append(transfers, settlementTransfer{FromUserID: from, ToUserID: to, Amount: sum})
	}
	if len(totals) > maxSettlementParticipants {
		return settlementBatch{}, errInvalidSettlement
	}
	sort.Slice(transfers, func(i, j int) bool {
		if transfers[i].FromUserID != transfers[j].FromUserID {
			return transfers[i].FromUserID < transfers[j].FromUserID
		}
		return transfers[i].ToUserID < transfers[j].ToUserID
	})
	changes := make([]any, 0, len(totals))
	for _, a := range totals {
		changes = append(changes, map[string]any{"user_id": a.UserID, "reason": "game_win_loss", "debit": a.Debit, "credit": a.Credit})
	}
	normalized := map[string]any{"version": request["version"], "operation_id": request["operation_id"], "business": request["business"], "business_ref": request["business_ref"], "detail": request["detail"], "changes": changes}
	batch, err := decodeGroupFields(normalized)
	if err != nil {
		return batch, err
	}
	op := &batch.Operations[0]
	op.Policy = "transfer"
	op.Transfers = transfers
	batch.Kind = "group-transfer"
	batch.ID = derivedBatchID(batch.Kind, op.ID)
	return boundedBatch(batch, request)
}
