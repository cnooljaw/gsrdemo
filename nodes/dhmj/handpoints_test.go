package dhmj

import "testing"

// TestHandPoints 保护各项牌型台数和原版的等待牌型互斥顺序。
func TestHandPoints(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hand, melds []byte
		winning     byte
		want        handPoints
	}{
		{"嵌倒优先于边倒", []byte{1, 2, 2, 3, 4, 7, 7}, nil, 3, handPoints{clipWait: 1, pureSuit: 4}},
		{"边倒", []byte{1, 2, 7, 7}, nil, 3, handPoints{edgeWait: 1, pureSuit: 4}},
		{"单吊", []byte{7}, []byte{0x35, 0x35, 0x35}, 7, handPoints{singleWait: 1, dragons: 1, mixedSuit: 2, allTriplets: 2}},
		{"对倒和混一色", []byte{7, 7, 0x35, 0x35}, nil, 0x35, handPoints{doubleWait: 1, dragons: 1, mixedSuit: 2, allTriplets: 2}},
		{"排胡", []byte{5, 6, 7, 7}, nil, 4, handPoints{pureSuit: 4, pingHu: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreHand(tc.hand, tc.melds, tc.winning, 0x31, 0x32); got != tc.want {
				t.Fatalf("计台 = %v，预期 %v", got, tc.want)
			}
		})
	}
}
