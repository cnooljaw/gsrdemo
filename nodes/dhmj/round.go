package dhmj

import (
	"crypto/rand"
	"errors"
	"math/big"
	"slices"
	"time"
)

const wallSize = 144
const openingHandSize = 13

var errInvalidDeal = errors.New("定海麻将开局参数无效")

// round 持有单局牌墙和各座暗手，由牌桌串行推进。
type round struct {
	wall         []byte
	nextTile     int
	dealer       int
	active       int
	winds        [seatCount]byte
	roundWind    byte
	hands        [seatCount][]byte
	flowers      [seatCount][]byte
	end          *roundEnd
	out          [seatCount][]byte
	melds        [seatCount][]meld
	drawn        [seatCount]bool
	winDetail    [seatCount]winPoints
	turn         uint64
	windowID     uint64
	window       *discardWindow
	deadline     time.Time
	lastDraw     byte
	selfPending  bool
	kongBloom    bool
	kongProvider int
}

type flowerReplacement struct {
	flower, replacement byte // 结局时没有补牌，replacement 为 0
}

type drawResult struct {
	drawn, playable byte // 摸花时两者不同，终局时 playable 为 0
	flowers         []flowerReplacement
}

func fullWall() []byte {
	wall := make([]byte, 0, wallSize)
	for suit := range 4 {
		last := 9
		if suit == 3 {
			last = 7
		}
		for rank := 1; rank <= last; rank++ {
			for range 4 {
				wall = append(wall, byte(suit<<4|rank))
			}
		}
	}
	for flower := byte(0x41); flower <= 0x48; flower++ {
		wall = append(wall, flower)
	}
	return wall
}

func shuffledWall() ([]byte, error) {
	wall := fullWall()
	for i := len(wall) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return nil, err
		}
		wall[i], wall[j.Int64()] = wall[j.Int64()], wall[i]
	}
	return wall, nil
}

// dealRound 按原版连发每座 13 张。起手花留到对应座位首次处理时替换。
func dealRound(wall []byte, dealer int, roundWind byte) (round, error) {
	if len(wall) != wallSize || !validSeat(dealer) || roundWind < 0x31 || roundWind > 0x34 {
		return round{}, errInvalidDeal
	}
	var stock [256]uint8
	for _, tile := range fullWall() {
		stock[tile]++
	}
	for _, tile := range wall {
		if stock[tile] == 0 {
			return round{}, errInvalidDeal
		}
		stock[tile]--
	}

	g := round{wall: slices.Clone(wall), dealer: dealer, active: dealer, roundWind: roundWind}
	for seat := range seatCount {
		g.winds[seat] = byte(0x31 + (seat+seatCount-dealer)%seatCount)
		g.hands[seat] = slices.Clone(g.wall[g.nextTile : g.nextTile+openingHandSize])
		g.nextTile += openingHandSize
	}
	return g, nil
}

func (g *round) remaining() int {
	return len(g.wall) - g.nextTile
}

func (g *round) takeTile() byte {
	if g.remaining() == 0 {
		g.end = &roundEnd{kind: draw, dealer: g.dealer}
		return 0
	}
	tile := g.wall[g.nextTile]
	g.nextTile++
	return tile
}

func (g *round) replaceInitialFlowers(seat int) []flowerReplacement {
	var changes []flowerReplacement
	for index := range g.hands[seat] {
		if g.end != nil {
			break
		}
		changes = g.replaceFlowers(seat, index, changes)
	}
	return changes
}

func (g *round) drawActive() drawResult {
	seat := g.active
	if g.end != nil || len(g.hands[seat])%3 != 1 {
		return drawResult{}
	}
	drawn := g.takeTile()
	if drawn == 0 {
		return drawResult{}
	}
	g.drawn[seat] = true
	index := len(g.hands[seat])
	g.hands[seat] = append(g.hands[seat], drawn)
	changes := g.replaceFlowers(seat, index, nil)
	result := drawResult{drawn: drawn, flowers: changes}
	if g.end == nil {
		result.playable = g.hands[seat][index]
	}
	return result
}

func (g *round) replaceFlowers(seat, index int, changes []flowerReplacement) []flowerReplacement {
	for {
		tile := g.hands[seat][index]
		if tile < 0x41 || tile > 0x48 {
			return changes
		}
		g.flowers[seat] = append(g.flowers[seat], tile)
		if len(g.flowers[seat]) == 8 {
			g.end = &roundEnd{kind: eightFlowers, dealer: g.dealer, winners: []hu{{seat, 4}}, claims: g.claimCounts()}
			g.winDetail[seat] = scoreWin(nil, nil, 0, winContext{kind: eightFlowers})
		}
		var replacement byte
		if g.end == nil {
			replacement = g.takeTile()
		}
		changes = append(changes, flowerReplacement{tile, replacement})
		if g.end != nil {
			g.hands[seat] = slices.Delete(g.hands[seat], index, index+1)
			return changes
		}
		g.hands[seat][index] = replacement
	}
}
