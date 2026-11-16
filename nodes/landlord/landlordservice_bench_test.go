package main

import (
	"fmt"
	"testing"
)

// BenchmarkLandlordResolveFullTables 比较满桌数量对预留再释放的影响，不包含牌桌创建、通信或存储。
func BenchmarkLandlordResolveFullTables(b *testing.B) {
	for _, count := range []int{0, 100, 500, 999} {
		b.Run(fmt.Sprintf("tables=%d", count), func(b *testing.B) {
			service := newLandlordService(landlordJournal{}, "settlement")
			for index := 0; index < count; index++ {
				service.setTable(fmt.Sprintf("full-%d", index), landlordTable{Count: 3})
			}
			service.setTable("available", landlordTable{Count: 2})
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				route, err := service.handleResolve(nil, 1, "login")
				if err != nil {
					b.Fatal(err)
				}
				if route["room_name"] != "available" {
					b.Fatal("没有补可加入桌")
				}
				released, err := service.handleRelease(nil, 1, "login", "available", route["membership"].(string), true)
				if err != nil || !released {
					b.Fatal(err)
				}
			}
		})
	}
}
