package dhmj

import (
	"slices"
	"testing"
)

// TestOpeningDeal 保护原随机房按座位连发 13 张、庄家先补花再首摸。
func TestOpeningDeal(t *testing.T) {
	wall := wallWith(t, map[int]byte{13: 0x41, 0: 0x48, 52: 0x42, 53: 0x34, 54: 0x33})
	g, err := dealRound(wall, 1, 0x32)
	if err != nil {
		t.Fatal(err)
	}
	if g.active != 1 || g.roundWind != 0x32 || g.winds != [4]byte{0x34, 0x31, 0x32, 0x33} || g.remaining() != 92 {
		t.Fatalf("开局状态错误：%+v", g)
	}
	for seat, hand := range g.hands {
		if !slices.Equal(hand, wall[seat*13:(seat+1)*13]) {
			t.Fatalf("座位 %d 发牌顺序错误", seat)
		}
	}
	if got := g.replaceInitialFlowers(1); !slices.Equal(got, []flowerReplacement{{0x41, 0x42}, {0x42, 0x34}}) {
		t.Fatalf("起手花连续替换错误：%v", got)
	}
	got := g.drawActive()
	if got.drawn != 0x33 || got.playable != 0x33 || len(g.hands[1]) != 14 || g.remaining() != 89 {
		t.Fatalf("庄家首摸错误：%+v，余牌 %d", got, g.remaining())
	}
	if g.hands[0][0] != 0x48 || len(g.flowers[0]) != 0 {
		t.Fatal("其他座位的起手花被提前替换")
	}
}

// TestDrawFlowersAndRoundEnd 保护摸花到普通牌的顺序、八花直接结束及牌墙用尽才流局。
func TestDrawFlowersAndRoundEnd(t *testing.T) {
	t.Run("摸花连续替换", func(t *testing.T) {
		wall := wallWith(t, map[int]byte{52: 0x41, 53: 0x42, 54: 0x37})
		g, err := dealRound(wall, 0, 0x31)
		if err != nil {
			t.Fatal(err)
		}
		got := g.drawActive()
		if got.drawn != 0x41 || got.playable != 0x37 || !slices.Equal(got.flowers, []flowerReplacement{{0x41, 0x42}, {0x42, 0x37}}) {
			t.Fatalf("摸花顺序错误：%+v", got)
		}
	})
	t.Run("八花不再摸牌", func(t *testing.T) {
		layout := make(map[int]byte)
		for i := range 7 {
			layout[i] = byte(0x41 + i)
		}
		layout[59] = 0x48
		wall := wallWith(t, layout)
		g, err := dealRound(wall, 0, 0x31)
		if err != nil {
			t.Fatal(err)
		}
		g.replaceInitialFlowers(0)
		got := g.drawActive()
		if got.playable != 0 || g.end == nil || g.end.kind != eightFlowers ||
			!slices.Equal(g.end.winners, []hu{{0, 4}}) || g.remaining() != 84 || len(g.flowers[0]) != 8 {
			t.Fatalf("未按八花终止：%+v，余牌 %d", got, g.remaining())
		}
	})
	t.Run("最后一张仍可操作", func(t *testing.T) {
		wall := wallWith(t, map[int]byte{143: 0x37})
		g, err := dealRound(wall, 2, 0x31)
		if err != nil {
			t.Fatal(err)
		}
		g.nextTile = len(wall) - 1
		if got := g.drawActive(); got.playable != 0x37 || g.end != nil || g.remaining() != 0 {
			t.Fatalf("最后一张提前流局：%+v", got)
		}
		// 模拟该玩家已打牌，下一位再尝试摸牌。
		g.active = 3
		if got := g.drawActive(); got.playable != 0 || g.end == nil || g.end.kind != draw || g.end.dealer != 2 {
			t.Fatalf("牌墙耗尽后未流局：%+v", got)
		}
	})
	t.Run("最后一张是花时无法继续补牌", func(t *testing.T) {
		wall := wallWith(t, map[int]byte{143: 0x41})
		g, err := dealRound(wall, 0, 0x31)
		if err != nil {
			t.Fatal(err)
		}
		g.nextTile = len(wall) - 1
		got := g.drawActive()
		if got.drawn != 0x41 || got.playable != 0 || !slices.Equal(got.flowers, []flowerReplacement{{0x41, 0}}) ||
			g.end == nil || g.end.kind != draw || len(g.hands[0]) != 13 {
			t.Fatalf("末张花补牌耗尽未正确流局：%+v", got)
		}
	})
}

// TestDealRejectsInvalidWall 保护牌墙构成及桌的风位、庄家边界。
func TestDealRejectsInvalidWall(t *testing.T) {
	wall := fullWall()
	for _, tc := range []struct {
		wall   []byte
		dealer int
		wind   byte
	}{
		{wall[:143], 0, 0x31},
		{wall, 4, 0x31},
		{wall, 0, 0x35},
	} {
		if _, err := dealRound(tc.wall, tc.dealer, tc.wind); err == nil {
			t.Fatal("接受了非法牌墙或座位")
		}
	}
	wall[0] = 0x48
	if _, err := dealRound(wall, 0, 0x31); err == nil {
		t.Fatal("接受了多出的花牌")
	}
}

func wallWith(t *testing.T, tiles map[int]byte) []byte {
	t.Helper()
	wall := fullWall()
	for index := 0; index < len(wall); index++ {
		tile, fixed := tiles[index]
		if !fixed {
			continue
		}
		for source, card := range wall {
			if _, fixed := tiles[source]; source < index && fixed {
				continue
			}
			if card == tile {
				wall[index], wall[source] = wall[source], wall[index]
				break
			}
		}
		if wall[index] != tile {
			t.Fatalf("无法在 %d 放入 %x", index, tile)
		}
	}
	return wall
}
