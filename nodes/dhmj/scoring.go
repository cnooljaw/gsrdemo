package main

import (
	"errors"
	"math"
)

const seatCount = 4

type endKind uint8

const (
	draw endKind = iota
	discardHu
	selfHu
	kongFlower // 直杠后开花，保留供杠座位
	eightFlowers
)

type roomScore struct {
	tier int
	base int64
}

type hu struct {
	seat  int
	point int
}

type roundEnd struct {
	kind       endKind
	dealer     int
	source     int // 点炮、被抢补杠或供直杠的座位
	robbedKong bool
	winners    []hu
	claims     [seatCount][seatCount]int // claims[A][B] 是 A 从 B 取得的副露数
}

type payment struct {
	from, to int
	amount   int64
}

// 下标为台数。第一个表用于点炮，第二个表用于自摸。
type scoreTier struct {
	discard [6]int64
	self    [6]int64
}

var scoreTiers = map[int]scoreTier{
	5:  {discard: [6]int64{3, 4, 5, 7, 10, 20}, self: [6]int64{0, 2, 3, 4, 5, 10}},
	10: {discard: [6]int64{6, 8, 10, 14, 20, 40}, self: [6]int64{0, 4, 6, 8, 10, 20}},
	25: {discard: [6]int64{10, 15, 25, 40, 50, 100}, self: [6]int64{0, 10, 15, 20, 25, 50}},
	50: {discard: [6]int64{30, 40, 50, 70, 100, 200}, self: [6]int64{0, 20, 30, 40, 50, 100}},
}

var errInvalidScore = errors.New("定海麻将结算参数无效")

func validSeat(seat int) bool {
	return seat >= 0 && seat < seatCount
}

// roundPayments 计算理论付款。余额、手续费和实扣由结算服务处理。
func roundPayments(room roomScore, end roundEnd) ([]payment, error) {
	tier, ok := scoreTiers[room.tier]
	if !ok || room.base <= 0 || end.kind > eightFlowers {
		return nil, errInvalidScore
	}
	if end.kind == draw {
		if !validSeat(end.dealer) || len(end.winners) != 0 || room.base > math.MaxInt64/3 {
			return nil, errInvalidScore
		}
		var result []payment
		for seat := range seatCount {
			if seat != end.dealer {
				result = append(result, payment{end.dealer, seat, room.base})
			}
		}
		return result, nil
	}
	if len(end.winners) == 0 || (end.kind != discardHu && len(end.winners) != 1) ||
		((end.kind == discardHu || end.kind == kongFlower) && !validSeat(end.source)) ||
		(end.robbedKong && end.kind != discardHu) {
		return nil, errInvalidScore
	}

	var amounts [seatCount][seatCount]int64
	var seen [seatCount]bool
	var total int64
	for _, winner := range end.winners {
		if !validSeat(winner.seat) || seen[winner.seat] || winner.point < 0 ||
			((end.kind == discardHu || end.kind == kongFlower) && winner.seat == end.source) {
			return nil, errInvalidScore
		}
		seen[winner.seat] = true
		point := min(winner.point, 4)
		if end.kind == eightFlowers {
			point = 4
		}
		multiplier := tier.self[point]
		if end.kind == discardHu {
			multiplier = tier.discard[point]
		}
		if multiplier == 0 {
			return nil, errInvalidScore
		}
		for payer := range seatCount {
			if payer == winner.seat {
				continue
			}
			factor := multiplier * paymentFactor(end, payer, winner.seat)
			if factor == 0 {
				continue
			}
			if room.base > (math.MaxInt64-total)/factor {
				return nil, errInvalidScore
			}
			amount := room.base * factor
			total += amount
			amounts[payer][winner.seat] = amount
		}
	}

	var result []payment
	for payer, recipients := range amounts {
		for recipient, amount := range recipients {
			if amount > 0 {
				result = append(result, payment{payer, recipient, amount})
			}
		}
	}
	return result, nil
}

func paymentFactor(end roundEnd, payer, winner int) int64 {
	bond := 0
	if end.claims[payer][winner] >= 3 {
		bond++
	}
	if end.claims[winner][payer] >= 3 {
		bond++
	}
	if end.kind == discardHu {
		factor := [3]int64{0, 1, 2}[bond]
		if payer == end.source {
			factor = [3]int64{1, 2, 4}[bond]
		}
		if end.robbedKong {
			factor *= 3
		}
		return factor
	}
	factor := [3]int64{1, 3, 6}[bond]
	if end.kind == kongFlower {
		if payer == end.source {
			return 5 * factor
		}
		if bond == 0 {
			return 0
		}
	}
	return factor
}
