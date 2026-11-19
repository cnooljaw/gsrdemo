package dhmj

import (
	"slices"
	"testing"
)

// TestCanHu 保护原版判胡、剩余暗手长度和牌值边界。
func TestCanHu(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cards []byte
		want  bool
	}{
		{"原 C 用例的八张暗手", []byte{0x12, 0x15, 0x16, 0x17, 0x27, 0x28, 0x29, 0x12}, true},
		{"顺子和字牌刻子", []byte{0x04, 0x05, 0x06, 0x24, 0x24, 0x35, 0x35, 0x35}, true},
		{"十四张暗手可胡", []byte{6, 8, 3, 2, 8, 50, 20, 21, 5, 50, 22, 7, 8, 1}, true},
		{"整副暗手缺一组", []byte{6, 8, 3, 2, 8, 50, 20, 21, 5, 50, 22, 7, 8, 9}, false},
		{"副露后只剩一对", []byte{0x37, 0x37}, true},
		{"字牌不能组顺", []byte{0x31, 0x32, 0x33, 7, 7}, false},
		{"万筒不能混顺", []byte{8, 9, 0x11, 7, 7}, false},
		{"花牌不用于拆手牌", []byte{0x41, 0x41}, false},
		{"同牌最多四张", []byte{7, 7, 7, 7, 7}, false},
		{"不完整的暗手", []byte{1, 1, 1}, false},
		{"空手", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.cards)
			if got := canHu(tc.cards); got != tc.want {
				t.Fatalf("牌 %x: 得到 %v，预期 %v", tc.cards, got, tc.want)
			}
			if !slices.Equal(before, tc.cards) {
				t.Fatal("判胡修改了手牌")
			}
		})
	}
}
