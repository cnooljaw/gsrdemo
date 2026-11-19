package main

// winContext 是随机房计台所需的牌桌状态。风位使用 0x31～0x34。
type winContext struct {
	kind                  endKind
	activeWind, roundWind byte
	flowers               []byte
	dealer, discarded     bool
	kongBloom, robbedKong bool
	lastTile              bool
}

type winPoints struct {
	handPoints
	flowers, selfDraw, kongBloom, robKong int
	lastTile, sky, ground, roundWind      int
	eightFlowers                          int
}

func (p winPoints) total() int {
	return p.handPoints.total() + p.flowers + p.selfDraw + p.kongBloom + p.robKong +
		p.lastTile + p.sky + p.ground + p.roundWind + p.eightFlowers
}

// scoreWin 在已判定胡牌后调用。hand 与 melds 都不含胡牌张。
func scoreWin(hand, melds []byte, winning byte, ctx winContext) winPoints {
	if ctx.kind == eightFlowers {
		return winPoints{eightFlowers: 4}
	}
	points := winPoints{
		handPoints: scoreHand(hand, melds, winning, ctx.activeWind, ctx.roundWind),
		flowers:    flowerPoints(ctx.flowers, ctx.activeWind),
	}
	if ctx.kongBloom || ctx.kind == kongFlower {
		points.kongBloom = 2
	} else if ctx.kind == selfHu {
		points.selfDraw = 1
	}
	if ctx.robbedKong {
		points.robKong = 1
	}
	if ctx.lastTile {
		points.lastTile = 4
	}
	if !ctx.discarded {
		if ctx.dealer {
			points.sky = 4
		} else {
			points.ground = 1
		}
	}
	// 原随机房用胡牌前的全牌统计圈风。门风只参与牌型判断，不另加台。
	if ctx.roundWind >= 0x31 && ctx.roundWind <= 0x34 {
		if tileCount(hand, ctx.roundWind)+tileCount(melds, ctx.roundWind) >= 3 {
			points.roundWind = 1
		}
	}
	return points
}

func tileCount(cards []byte, target byte) int {
	count := 0
	for _, card := range cards {
		if card == target {
			count++
		}
	}
	return count
}

func flowerPoints(flowers []byte, wind byte) int {
	if len(flowers) == 8 {
		return 8
	}
	var groupCount, matched [2]int
	for _, tile := range flowers {
		if tile < 0x41 || tile > 0x48 {
			continue
		}
		index := tile - 0x41
		group := index / 4
		groupCount[group]++
		if 0x31+index%4 == wind {
			matched[group]++
		}
	}
	for group, count := range groupCount {
		if count == 4 {
			matched[group] = 2
		}
	}
	return matched[0] + matched[1]
}
