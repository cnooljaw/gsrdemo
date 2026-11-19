package main

type handPoints struct {
	singleWait, clipWait, edgeWait, doubleWait        int
	dragons, pureSuit, mixedSuit, pingHu, allTriplets int
}

func (p handPoints) total() int {
	return p.singleWait + p.clipWait + p.edgeWait + p.doubleWait +
		p.dragons + p.pureSuit + p.mixedSuit + p.pingHu + p.allTriplets
}

// scoreHand 计算牌形台数。hand 不含胡牌张，melds 是所有副露的牌。
func scoreHand(hand, melds []byte, winning, seatWind, roundWind byte) handPoints {
	hidden, ok := countHand(hand)
	extra, valid := countHand(melds)
	if !ok || !valid || len(hand) > 13 || len(hand)%3 != 1 {
		return handPoints{}
	}
	if _, valid := countHand([]byte{winning}); !valid {
		return handPoints{}
	}
	suit, rank := winning>>4, winning&15
	hidden[suit][rank]++
	all := hidden
	for s := range all {
		for r := range all[s] {
			all[s][r] += extra[s][r]
			if all[s][r] > 4 {
				return handPoints{}
			}
		}
	}
	var points handPoints
	switch {
	case suit < 3 && rank > 1 && rank < 9 && sequenceWait(hidden, suit, rank-1):
		points.clipWait = 1
	case suit < 3 && ((rank == 3 && sequenceWait(hidden, suit, 1)) ||
		(rank == 7 && sequenceWait(hidden, suit, 7))):
		points.edgeWait = 1
	default:
		rest := hidden
		if rest[suit][rank] >= 2 {
			rest[suit][rank] -= 2
			if allMelds(rest) {
				points.singleWait = 1
			}
		}
		if points.singleWait == 0 && hidden[suit][rank] >= 3 {
			rest := hidden
			rest[suit][rank] -= 3
			if hasPairAndMelds(rest) {
				points.doubleWait = 1
			}
		}
	}
	for dragon := 5; dragon <= 7; dragon++ {
		if all[3][dragon] >= 3 {
			points.dragons++
		}
	}
	var suits int
	for _, counts := range all[:3] {
		if counts != [10]uint8{} {
			suits++
		}
	}
	if suits == 1 && all[3] == [10]uint8{} {
		points.pureSuit = 4
	} else if suits <= 1 {
		// 参考算法也把纯字牌计作混一色。
		points.mixedSuit = 2
	}
	if points.clipWait+points.edgeWait+points.singleWait == 0 && isPingHu(all, seatWind, roundWind) {
		points.pingHu = 1
	}

	pairs, cardsInTriplets := 0, 0
	for _, counts := range hidden {
		for _, count := range counts {
			switch count {
			case 2:
				pairs++
			case 3:
				cardsInTriplets += 3
			}
		}
	}
	// 原版的对对胡台数只检查暗手，包括胡牌张。
	if pairs == 1 && cardsInTriplets+2 == len(hand)+1 {
		points.allTriplets = 2
	}
	return points
}

func sequenceWait(counts handCounts, suit, start byte) bool {
	for rank := start; rank < start+3; rank++ {
		if counts[suit][rank] == 0 {
			return false
		}
		counts[suit][rank]--
	}
	return hasPairAndMelds(counts)
}

// 排胡的将牌不能是门风、圈风或中发白，余牌必须至少有一组顺子。
func isPingHu(counts handCounts, seatWind, roundWind byte) bool {
	for suit, ranks := range counts {
		for rank, count := range ranks {
			card := byte(suit<<4 | rank)
			if count < 2 || card == seatWind || card == roundWind || card >= 0x35 {
				continue
			}
			rest := counts
			rest[suit][rank] -= 2
			if allSequences(rest) {
				return true
			}
		}
	}
	return false
}

func allSequences(counts handCounts) bool {
	if counts[3] != [10]uint8{} {
		return false
	}
	used := false
	for suit := range 3 {
		for rank := 1; rank <= 9; rank++ {
			for counts[suit][rank] > 0 {
				if rank > 7 || counts[suit][rank+1] == 0 || counts[suit][rank+2] == 0 {
					return false
				}
				counts[suit][rank]--
				counts[suit][rank+1]--
				counts[suit][rank+2]--
				used = true
			}
		}
	}
	return used
}
