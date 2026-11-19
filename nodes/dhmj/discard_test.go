package dhmj

import (
	"slices"
	"testing"
)

// TestDiscardOffers 保护只允许下家吃、三张同牌可碰可直杠，以及暗手的点炮判定。
func TestDiscardOffers(t *testing.T) {
	hands := [seatCount][]byte{
		{3, 4, 6, 7}, {3, 4, 6, 7},
		{5, 5, 5, 8}, {3, 4, 7, 7},
	}
	w := newDiscardWindow(hands, 3, 5)
	for seat, want := range [][]responseAction{
		{eatRight, eatMiddle, eatLeft},
		nil,
		{directKong, pong},
		nil,
	} {
		var got []responseAction
		for action := eatRight; action <= claimHu; action++ {
			if w.offers[seat].has(action) {
				got = append(got, action)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("座位 %d 动作 %v，预期 %v", seat, got, want)
		}
	}
	w = newDiscardWindow(hands, 0, 5)
	if !w.offers[3].has(claimHu) || w.offers[3].has(eatRight) {
		t.Fatal("非下家可胡，但不可吃")
	}
}

// TestDiscardWindowWaitsForEveryPotentialWinner 保护胡优先于碰，且所有可能胡的人答复后才结算。
func TestDiscardWindowWaitsForEveryPotentialWinner(t *testing.T) {
	w := newDiscardWindow([seatCount][]byte{{}, {5, 5, 6, 8}, {3, 4, 7, 7}, {6, 7, 9, 9}}, 0, 5)
	if !w.submit(1, pong) || !w.submit(3, claimHu) {
		t.Fatal("未接受已提供的动作")
	}
	if got, ready := w.resolve(); ready {
		t.Fatalf("仍有人可胡时提前裁决：%v", got)
	}
	if w.submit(1, pass) || w.submit(2, directKong) || w.submit(0, pass) {
		t.Fatal("重复、未提供或出牌者的动作不应接纳")
	}
	if !w.submit(2, claimHu) {
		t.Fatal("未接纳第二个胡")
	}
	got, ready := w.resolve()
	if !ready || !slices.Equal(got, []response{{2, claimHu}, {3, claimHu}}) {
		t.Fatalf("一炮多响错误：%v %v", got, ready)
	}
}

// TestDiscardWindowUsesReferencePriority 保护碰优先于直杠，吃优先级和同级座位顺序独立于请求到达时间。
func TestDiscardWindowUsesReferencePriority(t *testing.T) {
	w := discardWindow{source: 3}
	w.offers[0].add(eatLeft)
	w.offers[1].add(directKong)
	w.offers[2].add(pong)
	if !w.submit(1, directKong) || !w.submit(2, pong) {
		t.Fatal("无法选择已提供的动作")
	}
	got, ready := w.resolve()
	if !ready || !slices.Equal(got, []response{{2, pong}}) {
		t.Fatalf("碰应优先于直杠：%v %v", got, ready)
	}

	w = discardWindow{source: 3}
	w.offers[0].add(pong)
	w.offers[2].add(pong)
	w.submit(2, pong)
	if got, ready = w.resolve(); ready {
		t.Fatalf("近座未答复时提前选远座：%v", got)
	}
	w.submit(0, pass)
	got, ready = w.resolve()
	if !ready || !slices.Equal(got, []response{{2, pong}}) {
		t.Fatalf("近座过后应接纳远座：%v %v", got, ready)
	}
}

// TestDiscardWindowAllPass 保护无可选动作或全过时推进到下一家。
func TestDiscardWindowAllPass(t *testing.T) {
	for _, option := range []bool{false, true} {
		w := discardWindow{source: 0}
		if option {
			w.offers[1].add(claimHu)
			w.submit(1, pass)
		}
		got, ready := w.resolve()
		if !ready || len(got) != 0 {
			t.Fatalf("全过应推进下一家：%v %v", got, ready)
		}
	}
}
