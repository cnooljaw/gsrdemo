package main

import "testing"

// TestLandlordFillsAvailableTables 保护优先填满可入座桌、跳过封桌及重登保留原桌的凑桌规则。
func TestLandlordFillsAvailableTables(t *testing.T) {
	service := newLandlordService(landlordJournal{})
	service.tables = map[string]landlordTable{
		"one":    {Count: 1},
		"two":    {Count: 2},
		"sealed": {Count: 2, Sealed: true},
		"full":   {Count: 3},
	}
	// 所有请求均应复用已有桌，不需要创建 Service 或启动 Runtime。
	route, err := service.handleResolve(nil, 1, "first-login")
	if err != nil || route["room_name"] != "two" || service.tables["two"].Count != 3 {
		t.Fatalf("没有优先填满两人桌: route=%v err=%v", route, err)
	}
	membership := route["membership"].(string)
	rejoined, err := service.handleResolve(nil, 1, "new-login")
	if err != nil || rejoined["room_name"] != "two" || rejoined["membership"] == membership || service.tables["two"].Count != 3 {
		t.Fatalf("重登没有保留原桌或重复占座: route=%v err=%v", rejoined, err)
	}
	if err := service.handleRelease(nil, 1, "first-login", "two", membership, true); err != nil {
		t.Fatal(err)
	}
	if service.tables["two"].Count != 3 || service.routes[1].LoginID != "new-login" {
		t.Fatal("旧预留释放了新登录的座位")
	}
	next, err := service.handleResolve(nil, 2, "another-login")
	if err != nil || next["room_name"] != "one" {
		t.Fatalf("新成员没有跳过满桌和封桌: route=%v err=%v", next, err)
	}
}
