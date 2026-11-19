package main

import (
	"slices"
	"testing"
)

// TestRoundPayments 保护原随机房的理论金额和各笔付款关系。
func TestRoundPayments(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  roundEnd
		want []payment
	}{
		{"流局庄家付三家", roundEnd{kind: draw, dealer: 1}, []payment{{1, 0, 100}, {1, 2, 100}, {1, 3, 100}}},
		{"零台点炮", roundEnd{kind: discardHu, source: 0, winners: []hu{{1, 0}}}, []payment{{0, 1, 300}}},
		{"一炮多响各查自己的台数", roundEnd{kind: discardHu, source: 0, winners: []hu{{2, 7}, {1, 1}}}, []payment{{0, 1, 400}, {0, 2, 1000}}},
		{"双向承包且抢杠", roundEnd{kind: discardHu, source: 0, robbedKong: true, winners: []hu{{1, 1}}, claims: [4][4]int{0: {1: 3}, 1: {0: 3, 2: 3}}}, []payment{{0, 1, 4800}, {2, 1, 1200}}},
		{"自摸", roundEnd{kind: selfHu, winners: []hu{{2, 1}}}, []payment{{0, 2, 200}, {1, 2, 200}, {3, 2, 200}}},
		{"直杠开花", roundEnd{kind: kongFlower, source: 0, winners: []hu{{1, 2}}, claims: [4][4]int{1: {2: 3}}}, []payment{{0, 1, 1500}, {2, 1, 900}}},
		{"八花按自摸四台", roundEnd{kind: eightFlowers, winners: []hu{{0, 0}}}, []payment{{1, 0, 500}, {2, 0, 500}, {3, 0, 500}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roundPayments(roomScore{tier: 5, base: 100}, tc.end)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("得到 %v, %v，预期 %v", got, err, tc.want)
			}
		})
	}
}

// TestScoreTierAndBaseAreIndependent 保护四张倍率表独立于房间底分。
func TestScoreTierAndBaseAreIndependent(t *testing.T) {
	end := roundEnd{kind: discardHu, source: 0, winners: []hu{{1, 2}}}
	for tier, want := range map[int]int64{5: 35, 10: 70, 25: 175, 50: 350} {
		got, err := roundPayments(roomScore{tier: tier, base: 7}, end)
		if err != nil || !slices.Equal(got, []payment{{0, 1, want}}) {
			t.Fatalf("档位 %d: 得到 %v, %v，预期 %d", tier, got, err, want)
		}
	}
}
