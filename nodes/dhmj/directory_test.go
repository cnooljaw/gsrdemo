package main

import "testing"

// TestDirectoryKeepsSeatOnRejoin 保护四人同桌，重登更新身份但保留座位，旧登录不能再操作。
func TestDirectoryKeepsSeatOnRejoin(t *testing.T) {
	d := newDirectory(defaultRoomRules, settlementJournal{})
	var table *room
	for uid := uint64(1); uid <= 4; uid++ {
		joined, seat, err := d.seatPlayer(uid, "login")
		if err != nil || seat != int(uid-1) || (table != nil && joined != table) {
			t.Fatalf("四人未正确凑桌：%v %d %v", joined, seat, err)
		}
		table = joined
	}
	table.players[1].ready = true
	joined, seat, err := d.seatPlayer(2, "new")
	if err != nil || joined != table || seat != 1 || !joined.players[1].ready {
		t.Fatal("重登改变了座位或准备状态")
	}
	if _, _, err := d.currentSeat(2, "login"); err == nil {
		t.Fatal("旧登录仍可操作新成员")
	}
	if next, _, err := d.seatPlayer(5, "login"); err != nil || next == table {
		t.Fatal("满桌仍被选入")
	}
}

// TestWaitingLeaveReleasesSeat 保护等候中离开立即放位，整桌空后删除目录。
func TestWaitingLeaveReleasesSeat(t *testing.T) {
	d := newDirectory(defaultRoomRules, settlementJournal{})
	table, _, _ := d.seatPlayer(1, "old")
	if _, _, err := d.seatPlayer(1, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.handleLeave(nil, 1, "old"); err == nil {
		t.Fatal("旧登录删除了当前座位")
	}
	if left, err := d.handleLeave(nil, 1, "new"); !left || err != nil || len(d.rooms) != 0 || len(d.routes) != 0 {
		t.Fatalf("空桌未释放：%s %v", table.name, err)
	}
}
