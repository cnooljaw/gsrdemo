package dhmj

// 定海麻将沿用高四位花色、低四位牌值的编码。花牌已在补花时移出暗手。
type handCounts [4][10]uint8

// canHu 判断包含胡牌张的暗手。副露已从暗手移除，因此允许只剩一对。
func canHu(cards []byte) bool {
	if len(cards) > 14 || len(cards)%3 != 2 {
		return false
	}
	counts, ok := countHand(cards)
	return ok && hasPairAndMelds(counts)
}

func countHand(cards []byte) (handCounts, bool) {
	var counts handCounts
	for _, card := range cards {
		suit, rank := card>>4, card&15
		if suit >= 4 || rank == 0 || rank > 9 || (suit == 3 && rank > 7) {
			return handCounts{}, false
		}
		counts[suit][rank]++
		if counts[suit][rank] > 4 {
			return handCounts{}, false
		}
	}
	return counts, true
}

func hasPairAndMelds(counts handCounts) bool {
	for suit, ranks := range counts {
		for rank, count := range ranks {
			if count < 2 {
				continue
			}
			rest := counts
			rest[suit][rank] -= 2
			if allMelds(rest) {
				return true
			}
		}
	}
	return false
}

// allMelds 沿用共用 C 算法的拆牌次序：字牌先拆刻子，数牌从小到大优先拆刻子。
func allMelds(counts handCounts) bool {
	for _, count := range counts[3] {
		if count%3 != 0 {
			return false
		}
	}
	for suit := range 3 {
		for rank := 1; rank <= 9; rank++ {
			if counts[suit][rank] >= 3 {
				counts[suit][rank] -= 3
			}
			for counts[suit][rank] > 0 {
				if rank > 7 || counts[suit][rank+1] == 0 || counts[suit][rank+2] == 0 {
					return false
				}
				counts[suit][rank]--
				counts[suit][rank+1]--
				counts[suit][rank+2]--
			}
		}
	}
	return true
}
