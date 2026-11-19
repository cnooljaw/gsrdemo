package dhmj

import "testing"

// TestRoundPoints 保护随机房的局况加台、互斥关系与八花独立计分。
func TestRoundPoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  winContext
		want winPoints
	}{
		{"庄家未出牌自摸", winContext{kind: selfHu, dealer: true}, winPoints{selfDraw: 1, sky: 4}},
		{"闲家未出牌点炮", winContext{kind: discardHu}, winPoints{ground: 1}},
		{"已出牌后无天地胡", winContext{kind: discardHu, discarded: true}, winPoints{}},
		{"杠上开花替代自摸一台", winContext{kind: kongFlower, discarded: true}, winPoints{kongBloom: 2}},
		{"自杠开花", winContext{kind: selfHu, kongBloom: true, discarded: true}, winPoints{kongBloom: 2}},
		{"最后一张抢杠", winContext{kind: discardHu, robbedKong: true, lastTile: true, discarded: true}, winPoints{robKong: 1, lastTile: 4}},
		{"八花不叠加其他台数", winContext{kind: eightFlowers, dealer: true, lastTile: true}, winPoints{eightFlowers: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreWin(nil, nil, 0, tc.ctx); got != tc.want {
				t.Fatalf("局况台数 = %+v，预期 %+v", got, tc.want)
			}
		})
	}
}

// TestRoundWindScoring 保护圈风从胡牌前的暗手和副露统计，门风不另外加台。
func TestRoundWindScoring(t *testing.T) {
	ctx := winContext{kind: discardHu, discarded: true, activeWind: 0x31, roundWind: 0x32}
	for _, tc := range []struct {
		hand, melds []byte
		winning     byte
		want        int
	}{
		{[]byte{7, 7, 0x32, 0x32}, nil, 0x32, 0},
		{[]byte{7}, []byte{0x32, 0x32, 0x32}, 7, 1},
		{[]byte{7}, []byte{0x31, 0x31, 0x31}, 7, 0},
	} {
		got := scoreWin(tc.hand, tc.melds, tc.winning, ctx)
		if got.roundWind != tc.want || got.total() != got.handPoints.total()+tc.want {
			t.Fatalf("计台 = %+v，预期在牌型台数之外只加 %d 台", got, tc.want)
		}
	}
}

// TestFlowerPoints 保护对位花、成套花及全部花牌的原版计台。
func TestFlowerPoints(t *testing.T) {
	for _, tc := range []struct {
		flowers []byte
		want    int
	}{
		{[]byte{0x41, 0x42, 0x45}, 2},
		{[]byte{0x41, 0x42, 0x43, 0x44}, 2},
		{[]byte{0x41, 0x42, 0x43, 0x44, 0x45}, 3},
		{[]byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48}, 8},
	} {
		got := scoreWin(nil, nil, 0, winContext{kind: discardHu, discarded: true, activeWind: 0x31, flowers: tc.flowers})
		if got.flowers != tc.want || got.total() != tc.want {
			t.Fatalf("花牌 %x 得到 %+v，预期 %d 台", tc.flowers, got, tc.want)
		}
	}
}
