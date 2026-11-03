package main

import "testing"

// TestRoundChanges 保护不同地主与胜方按当前倍率结算，三人变化守恒，异常金额和倍率不能持久化。
func TestRoundChanges(t *testing.T) {
	for landlord := 0; landlord < 3; landlord++ {
		for winner := 0; winner < 3; winner++ {
			g := testLandlordGame(t)
			g.Landlord, g.Winner, g.Multiplier = landlord, winner, 8
			result := resultForGame(*g)
			if !result.valid() {
				t.Fatal("合法结果被拒绝")
			}
			var total int64
			for i, c := range result.Changes {
				total += c.Credit - c.Debit
				amount := int64(80)
				if i == landlord {
					amount = 160
				}
				if c.Credit+c.Debit != amount {
					t.Fatal("倍率或地主结算错误")
				}
			}
			if total != 0 {
				t.Fatal("银子不守恒")
			}
			result.Changes[0].Credit++
			if result.valid() {
				t.Fatal("篡改金额被接受")
			}
		}
	}
}
