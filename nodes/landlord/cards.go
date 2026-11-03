package main

import (
	"crypto/rand"
	"math/big"
	"slices"
)

// 牌编码沿用参考实现：0x01..0x3d 四种花色，0x4e/0x4f 小王/大王。
func cardRank(card int64) int {
	if card == 0x4e {
		return 16
	}
	if card == 0x4f {
		return 17
	}
	if card < 1 || card > 0x3d || card&15 < 1 || card&15 > 13 {
		return 0
	}
	rank := int(card & 15)
	if rank < 3 {
		rank += 13
	}
	return rank
}

type cardPattern struct {
	Kind        string
	High, Count int
}
type rankGroup struct{ Rank, Count int }

// classify 只实现 Hlddz_GetNoFavusCardType；不修改输入，不引入 C/Lua 运行依赖。
func classify(cards []int64) cardPattern {
	n := len(cards)
	invalid := cardPattern{}
	if n == 0 || n > 20 {
		return invalid
	}
	counts := map[int]int{}
	seen := map[int64]bool{}
	for _, card := range cards {
		r := cardRank(card)
		if r == 0 || seen[card] {
			return invalid
		}
		seen[card] = true
		counts[r]++
	}
	groups := make([]rankGroup, 0, len(counts))
	for r, c := range counts {
		groups = append(groups, rankGroup{r, c})
	}
	slices.SortFunc(groups, func(a, b rankGroup) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return b.Rank - a.Rank
	})
	result := func(kind string) cardPattern { return cardPattern{kind, groups[0].Rank, n} }
	if n == 1 {
		return result("single")
	}
	if n == 2 && counts[16] == 1 && counts[17] == 1 {
		return result("rocket")
	}
	if n == 4 && groups[0].Count == 4 {
		return result("bomb")
	}
	if n == 8 && groups[0].Count == 4 && evenGroups(groups[1:]) {
		return result("four_pair")
	}
	if n == 6 && groups[0].Count == 4 {
		return result("four_single")
	}
	// 与参考算法相同：按张数/点数降序确定飞机主体，四张不能拆成主体的三张。
	for _, wing := range []struct {
		size int
		kind string
	}{{5, "plane_pair"}, {4, "plane_single"}} {
		m := n / wing.size
		if n%wing.size == 0 && m >= 2 && len(groups) >= m && consecutive(groups[:m], 3) {
			if wing.size == 4 || evenGroups(groups[m:]) {
				return result(wing.kind)
			}
		}
	}
	for _, line := range []struct {
		copies, min int
		kind        string
	}{{3, 6, "triples"}, {2, 6, "pairs"}, {1, 5, "straight"}} {
		if n >= line.min && n%line.copies == 0 && consecutive(groups, line.copies) {
			return result(line.kind)
		}
	}
	if n == 5 && groups[0].Count == 3 && groups[1].Count == 2 {
		return result("triple_pair")
	}
	if n == 4 && groups[0].Count == 3 {
		return result("triple_single")
	}
	if n == 3 && groups[0].Count == 3 {
		return result("triple")
	}
	if n == 2 && groups[0].Count == 2 {
		return result("pair")
	}
	return invalid
}

func evenGroups(groups []rankGroup) bool {
	for _, g := range groups {
		if g.Count%2 != 0 {
			return false
		}
	}
	return true
}

func consecutive(groups []rankGroup, copies int) bool {
	if len(groups) == 0 || groups[0].Rank > 14 {
		return false
	}
	for i, g := range groups {
		if g.Count != copies || g.Rank != groups[0].Rank-i {
			return false
		}
	}
	return true
}

func beats(next, last cardPattern) bool {
	if next.Kind == "" {
		return false
	}
	if last.Kind == "" {
		return true
	}
	if last.Kind == "rocket" {
		return false
	}
	if next.Kind == "rocket" {
		return true
	}
	if next.Kind == "bomb" && last.Kind != "bomb" {
		return true
	}
	return next.Kind == last.Kind && next.Count == last.Count && next.High > last.High
}

func shuffledDeck() ([]int64, error) {
	cards := make([]int64, 0, 54)
	for suit := int64(0); suit < 4; suit++ {
		for rank := int64(1); rank <= 13; rank++ {
			cards = append(cards, suit*16+rank)
		}
	}
	cards = append(cards, 0x4e, 0x4f)
	for i := len(cards) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, err
		}
		cards[i], cards[j.Int64()] = cards[j.Int64()], cards[i]
	}
	return cards, nil
}

func sortHand(cards []int64) {
	slices.SortFunc(cards, func(a, b int64) int {
		if d := cardRank(a) - cardRank(b); d != 0 {
			return d
		}
		return int(a - b)
	})
}
