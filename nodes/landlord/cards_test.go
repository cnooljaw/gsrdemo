package main

import "testing"

// TestCardPatterns 保护普通随机房的所有牌型、牌编码及不合法组合，规则参考无癞子 C 算法。
func TestCardPatterns(t *testing.T) {
	cases := []struct {
		name  string
		cards []int64
		kind  string
	}{
		{"single", []int64{3}, "single"}, {"pair", []int64{3, 0x13}, "pair"},
		{"triple", []int64{3, 0x13, 0x23}, "triple"},
		{"triple_single", []int64{3, 0x13, 0x23, 4}, "triple_single"},
		{"triple_pair", []int64{3, 0x13, 0x23, 4, 0x14}, "triple_pair"},
		{"straight", []int64{10, 11, 12, 13, 1}, "straight"},
		{"pairs", []int64{3, 0x13, 4, 0x14, 5, 0x15}, "pairs"},
		{"triples", []int64{3, 0x13, 0x23, 4, 0x14, 0x24}, "triples"},
		{"plane_single", []int64{3, 0x13, 0x23, 4, 0x14, 0x24, 5, 0x15}, "plane_single"},
		{"plane_pair", []int64{3, 0x13, 0x23, 4, 0x14, 0x24, 5, 0x15, 6, 0x16}, "plane_pair"},
		{"four_single", []int64{3, 0x13, 0x23, 0x33, 5, 0x15}, "four_single"},
		{"four_pair", []int64{3, 0x13, 0x23, 0x33, 5, 0x15, 6, 0x16}, "four_pair"},
		{"two_quads_as_four_pair", []int64{3, 0x13, 0x23, 0x33, 5, 0x15, 0x25, 0x35}, "four_pair"},
		{"bomb", []int64{2, 0x12, 0x22, 0x32}, "bomb"}, {"rocket", []int64{0x4e, 0x4f}, "rocket"},
		{"duplicate", []int64{3, 3}, ""}, {"invalid_card", []int64{0x43}, ""},
		{"short_straight", []int64{3, 4, 5, 6}, ""}, {"two_in_straight", []int64{11, 12, 13, 1, 2}, ""},
		{"broken_pairs", []int64{3, 0x13, 4, 0x14, 6, 0x16}, ""},
		{"quad_plane_body", []int64{3, 0x13, 0x23, 0x33, 4, 0x14, 0x24, 5}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.cards); got.Kind != c.kind {
				t.Fatalf("got=%+v want=%s", got, c.kind)
			}
		})
	}
}

// TestCardComparison 保护同型同长度比较、炸弹特权与王炸最大，不按花色比较大小。
func TestCardComparison(t *testing.T) {
	for _, c := range []struct {
		next, last []int64
		want       bool
	}{
		{[]int64{1}, []int64{13}, true}, {[]int64{2}, []int64{1}, true},
		{[]int64{0x13}, []int64{3}, false}, {[]int64{4, 0x14}, []int64{3}, false},
		{[]int64{3, 0x13, 0x23, 0x33}, []int64{0x4f}, true},
		{[]int64{0x4e, 0x4f}, []int64{2, 0x12, 0x22, 0x32}, true},
		{[]int64{2, 0x12, 0x22, 0x32}, []int64{0x4e, 0x4f}, false},
		{[]int64{4, 5, 6, 7, 8, 9}, []int64{3, 4, 5, 6, 7}, false},
	} {
		if got := beats(classify(c.next), classify(c.last)); got != c.want {
			t.Fatalf("%v > %v: %v", c.next, c.last, got)
		}
	}
}
