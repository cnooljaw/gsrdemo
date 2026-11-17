package main

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// BenchmarkMoleEnterFullGroups 比较满组积累对入场再离开的影响，仅测内存路径，不包含通信、广播或存储。
func BenchmarkMoleEnterFullGroups(b *testing.B) {
	// 连同待测入场用户，样本最多 12,000 人。
	for _, count := range []int{0, 100, 1000, (12000 - 1) / attackSeats} {
		b.Run(fmt.Sprintf("groups=%d", count), func(b *testing.B) {
			service := readyMole()
			now := time.Now()
			// 直接建立完整的满组夹具，避免把建组过程算进待测入场时间。
			for index := 0; index < count; index++ {
				name := fmt.Sprintf("attack-seeded-%06d", index)
				group := newMoleGroup(name, now)
				for seat := 0; seat < attackSeats; seat++ {
					uid := uint64(index*attackSeats + seat + 1)
					group.members[uid] = moleMember{UserID: uid, LoginID: "seeded", AccessNode: "access-01", AgentName: "agent", Seat: int64(seat)}
					service.users[uid] = name
					service.accounting.updateBalance(uid, silverBalance{Amount: 10000, Version: 1})
				}
				service.addGroup(group)
			}
			uid := uint64(count*attackSeats + 1)
			seed := silverBalance{Amount: 10000, Version: 1}.snapshot()
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := service.enter(uid, "login", "access-01", "agent", seed); err != nil {
					b.Fatal(err)
				}
				if _, err := service.leave(uid, "login"); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if len(service.groups) != count || len(service.users) != count*attackSeats || service.accounting.accounts[uid] != nil {
				b.Fatal("入场离开残留状态")
			}
		})
	}
}
