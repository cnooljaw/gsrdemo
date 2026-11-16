package main

import (
	"testing"
	"time"
)

// TestLandlordFillsAvailableTables 保护优先填满可入座桌、跳过封桌及重登保留原桌的凑桌规则。
func TestLandlordFillsAvailableTables(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	for room, table := range map[string]landlordTable{
		"one":    {Count: 1},
		"two":    {Count: 2},
		"sealed": {Count: 2, Sealed: true},
		"full":   {Count: 3},
	} {
		service.setTable(room, table)
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
	if released, err := service.handleRelease(nil, 1, "first-login", "two", membership, true); err != nil || released {
		t.Fatalf("旧成员被允许移除: %v %v", released, err)
	}
	if service.tables["two"].Count != 3 || service.routes[1].LoginID != "new-login" {
		t.Fatal("旧预留释放了新登录的座位")
	}
	next, err := service.handleResolve(nil, 2, "another-login")
	if err != nil || next["room_name"] != "one" {
		t.Fatalf("新成员没有跳过满桌和封桌: route=%v err=%v", next, err)
	}
}

// TestLandlordReleaseIsIdempotent 保护目录已释放后的重试不重复减人数，用户转到其他桌也不受旧清理影响。
func TestLandlordReleaseIsIdempotent(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	service.routes[1] = landlordRoute{"table", "login", "member"}
	service.setTable("table", landlordTable{Count: 2})
	for range 2 {
		if released, err := service.handleRelease(nil, 1, "login", "table", "member", true); err != nil || !released {
			t.Fatalf("release: %v %v", released, err)
		}
		if service.tables["table"].Count != 1 {
			t.Fatal("重复释放改变了其他成员人数")
		}
	}
	service.routes[1] = landlordRoute{"another-table", "new-login", "new-member"}
	if released, err := service.handleRelease(nil, 1, "login", "table", "member", false); err != nil || !released || service.routes[1].Room != "another-table" {
		t.Fatal("旧桌回收影响了其他桌的新成员")
	}
}

// TestLandlordAvailabilityTransitions 保护封桌不参与补位、解封后可补满、释放和预留到期后重新补位，删除桌不残留索引。
func TestLandlordAvailabilityTransitions(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	service.setTable("table", landlordTable{})
	reserve := func(uid uint64) map[string]any {
		t.Helper()
		route, err := service.handleResolve(nil, uid, "login")
		if err != nil {
			t.Fatal(err)
		}
		return route
	}
	first := reserve(1)
	if err := service.handleSeal(nil, "table", true); err != nil {
		t.Fatal(err)
	}
	if name := service.availableTable(); name != "" {
		t.Fatal("封桌仍可被选中", name)
	}
	if err := service.handleSeal(nil, "table", false); err != nil {
		t.Fatal(err)
	}
	reserve(2)
	reserve(3)
	if name := service.availableTable(); name != "" {
		t.Fatal("满桌仍可被选中", name)
	}
	if released, err := service.handleRelease(nil, 1, "login", "table", first["membership"].(string), true); err != nil || !released {
		t.Fatal(err)
	}
	if name := service.availableTable(); name != "table" {
		t.Fatal("释放后未重新补位", name)
	}
	if err := service.handleSeal(nil, "table", true); err != nil {
		t.Fatal(err)
	}
	// 只让第二名用户的预留过期，第三名用户的预留仍有效；到期不能意外解除封桌。
	service.tables["table"].Pending[2] = time.Now().Add(-time.Second)
	if !service.reconcileReservations("table", [3]landlordMember{}, time.Now()) {
		t.Fatal("误删仍有有效预留的桌")
	}
	if service.tables["table"].Count != 1 || service.availableTable() != "" {
		t.Fatal("到期后没有更新补位人数")
	}
	if err := service.handleSeal(nil, "table", false); err != nil {
		t.Fatal(err)
	}
	if service.availableTable() != "table" {
		t.Fatal("解封后未恢复补位")
	}
	reserve(4)
	if service.tables["table"].Count != 2 {
		t.Fatal("没有复用释放的预留")
	}
	if err := service.handleDiscard(nil, "table"); err != nil {
		t.Fatal(err)
	}
	if service.availableTable() != "" || len(service.routes) != 0 {
		t.Fatal("删除桌后残留可加入索引或路由")
	}
	service.setTable("expired", landlordTable{})
	reserve(5)
	if service.reconcileReservations("expired", [3]landlordMember{}, time.Now().Add(31*time.Second)) || service.availableTable() != "" {
		t.Fatal("到期空桌没有清理索引")
	}
}
