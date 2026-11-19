package main

import (
	"math"
	"math/bits"
	"sort"
)

type settlementActual struct {
	UserID uint64 `json:"user_id"`
	Debit  int64  `json:"debit"`
	Credit int64  `json:"credit"`
}

type settlementFee struct {
	UserID uint64 `json:"user_id"`
	Amount int64  `json:"amount"`
}

type settlementTransfer struct {
	FromUserID uint64 `json:"from_user_id"`
	ToUserID   uint64 `json:"to_user_id"`
	Amount     int64  `json:"amount"`
}

// applyActual 在所有钱包通过金额、版本检查后才统一更新，余额不足不影响其他付款人。
func applyActual(wallets map[uint64]walletBalance, totals map[uint64]settlementActual) ([]settlementActual, string) {
	ids := make([]uint64, 0, len(totals))
	for id := range totals {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		a := totals[id]
		b, ok := wallets[id]
		if !ok {
			return nil, "wallet_not_found"
		}
		if a.Debit < 0 || a.Credit < 0 || b.Amount < a.Debit {
			return nil, "amount_overflow"
		}
		if b.Amount-a.Debit > math.MaxInt64-a.Credit {
			return nil, "balance_overflow"
		}
		if (a.Debit != 0 || a.Credit != 0) && b.Version == math.MaxUint64 {
			return nil, "version_overflow"
		}
	}
	actual := make([]settlementActual, 0, len(ids))
	for _, id := range ids {
		a := totals[id]
		b := wallets[id]
		if a.Debit != 0 || a.Credit != 0 {
			b.Amount = b.Amount - a.Debit + a.Credit
			b.Version++
			wallets[id] = b
		}
		actual = append(actual, a)
	}
	return actual, ""
}

// settleEqualGroup 输方独立封顶后，赢家均分实际奖池；余数按赢家 UserID 降序分配。
func settleEqualGroup(wallets map[uint64]walletBalance, changes []silverChange) ([]settlementActual, string) {
	totals := make(map[uint64]settlementActual, len(changes))
	winners := []uint64{}
	var paid int64
	for _, c := range changes {
		b, ok := wallets[c.UserID]
		if !ok {
			return nil, "wallet_not_found"
		}
		a := settlementActual{UserID: c.UserID}
		if c.Debit > 0 {
			a.Debit = min(c.Debit, b.Amount)
			if paid > math.MaxInt64-a.Debit {
				return nil, "amount_overflow"
			}
			paid += a.Debit
		} else {
			winners = append(winners, c.UserID)
		}
		totals[c.UserID] = a
	}
	if len(winners) == 0 {
		return nil, "invalid_request"
	}
	sort.Slice(winners, func(i, j int) bool { return winners[i] > winners[j] })
	share, extra := paid/int64(len(winners)), paid%int64(len(winners))
	for i, id := range winners {
		a := totals[id]
		a.Credit = share
		if int64(i) < extra {
			a.Credit++
		}
		totals[id] = a
	}
	return applyActual(wallets, totals)
}

// settleTransferGroup 按每个付款人的事务前余额封顶，并按各收款明细比例分配，余数按收款 UserID 降序分配。
func settleTransferGroup(wallets map[uint64]walletBalance, transfers []settlementTransfer) ([]settlementActual, []settlementTransfer, string) {
	totals := map[uint64]settlementActual{}
	edges, reason := allocateTransfers(wallets, transfers, totals)
	if reason != "" {
		return nil, nil, reason
	}
	actual, reason := applyActual(wallets, totals)
	return actual, edges, reason
}

func settleTransferWithFees(wallets map[uint64]walletBalance, transfers []settlementTransfer, fees []settlementFee) ([]settlementActual, []settlementTransfer, []settlementFee, string) {
	totals := make(map[uint64]settlementActual, len(fees))
	charged := make([]settlementFee, 0, len(fees))
	for _, fee := range fees {
		balance, ok := wallets[fee.UserID]
		if !ok {
			return nil, nil, nil, "wallet_not_found"
		}
		amount := min(fee.Amount, balance.Amount)
		totals[fee.UserID] = settlementActual{UserID: fee.UserID, Debit: amount}
		charged = append(charged, settlementFee{UserID: fee.UserID, Amount: amount})
	}
	edges, reason := allocateTransfers(wallets, transfers, totals)
	if reason != "" {
		return nil, nil, nil, reason
	}
	actual, reason := applyActual(wallets, totals)
	if reason != "" {
		return nil, nil, nil, reason
	}
	return actual, edges, charged, ""
}

// allocateTransfers 以扣除已分配费用后的事务前余额为付款上限。
func allocateTransfers(wallets map[uint64]walletBalance, transfers []settlementTransfer, totals map[uint64]settlementActual) ([]settlementTransfer, string) {
	byPayer := map[uint64][]int{}
	requested := map[uint64]int64{}
	actualTransfers := append([]settlementTransfer(nil), transfers...)
	for i, e := range transfers {
		if _, ok := wallets[e.FromUserID]; !ok {
			return nil, "wallet_not_found"
		}
		if _, ok := wallets[e.ToUserID]; !ok {
			return nil, "wallet_not_found"
		}
		if e.Amount <= 0 || requested[e.FromUserID] > math.MaxInt64-e.Amount {
			return nil, "amount_overflow"
		}
		requested[e.FromUserID] += e.Amount
		byPayer[e.FromUserID] = append(byPayer[e.FromUserID], i)
		for _, uid := range [...]uint64{e.FromUserID, e.ToUserID} {
			if _, ok := totals[uid]; !ok {
				totals[uid] = settlementActual{UserID: uid}
			}
		}
	}
	for payer, indices := range byPayer {
		due := requested[payer]
		paid := min(due, wallets[payer].Amount-totals[payer].Debit)
		allocated := int64(0)
		for _, i := range indices {
			hi, lo := bits.Mul64(uint64(paid), uint64(transfers[i].Amount))
			q, _ := bits.Div64(hi, lo, uint64(due))
			actualTransfers[i].Amount = int64(q)
			allocated += int64(q)
		}
		sort.Slice(indices, func(i, j int) bool { return transfers[indices[i]].ToUserID > transfers[indices[j]].ToUserID })
		for i := int64(0); i < paid-allocated; i++ {
			actualTransfers[indices[i]].Amount++
		}
		a := totals[payer]
		a.Debit += paid
		totals[payer] = a
	}
	for _, e := range actualTransfers {
		a := totals[e.ToUserID]
		if a.Credit > math.MaxInt64-e.Amount {
			return nil, "amount_overflow"
		}
		a.Credit += e.Amount
		totals[e.ToUserID] = a
	}
	return actualTransfers, ""
}
