package main

import (
	"errors"
	"testing"
)

func readyMole() *moleService {
	service := newMoleService()
	service.accounting.ready = true
	return service
}

func enterMemory(t *testing.T, service *moleService, uid uint64, login string) map[string]any {
	t.Helper()
	view, err := service.enter(uid, login, "access-01", "agent", silverBalance{Amount: 10000, Version: 1}.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// TestMoleEnterCommitsTogether 保护缺余额和非法余额不留下组、成员或账户，成功后一次返回入座与余额。
func TestMoleEnterCommitsTogether(t *testing.T) {
	service := readyMole()
	for _, seed := range []map[string]any{nil, {"amount": int64(-1), "version": uint64(1)}} {
		if _, err := service.enter(1, "login", "access-01", "agent", seed); err == nil {
			t.Fatal("无有效余额却入座")
		}
		if len(service.groups) != 0 || len(service.users) != 0 || len(service.accounting.accounts) != 0 {
			t.Fatal("失败留下中间状态")
		}
	}
	view := enterMemory(t, service, 1, "login")
	if len(service.groups) != 1 || len(service.users) != 1 || len(service.accounting.accounts) != 1 || view["silver"] == nil {
		t.Fatal("入场与余额没有一起完成")
	}
}

// TestMoleGroupsFillAndLeaveAtomically 保护按三人补组、离开直接释放座位和闲置余额，空组直接回收。
func TestMoleGroupsFillAndLeaveAtomically(t *testing.T) {
	service := readyMole()
	first := enterMemory(t, service, 1, "one")
	for _, uid := range []uint64{2, 3} {
		if enterMemory(t, service, uid, "other")["attack_name"] != first["attack_name"] {
			t.Fatal("未补满原组")
		}
	}
	fourth := enterMemory(t, service, 4, "four")
	if fourth["attack_name"] == first["attack_name"] {
		t.Fatal("超过三人")
	}
	if _, err := service.leave(1, "one"); err != nil {
		t.Fatal(err)
	}
	if service.accounting.accounts[1] != nil || service.users[1] != "" {
		t.Fatal("离开未直接回收")
	}
	if enterMemory(t, service, 5, "five")["attack_name"] != first["attack_name"] {
		t.Fatal("未补空位")
	}
	if _, err := service.leave(4, "four"); err != nil {
		t.Fatal(err)
	}
	if service.groups[fourth["attack_name"].(string)] != nil {
		t.Fatal("空组未回收")
	}
}

// TestMoleReenterKeepsSeatAndCurrentLedger 保护重进保留原组座位、不用查询快照覆盖待扣，旧登录无法离开新身份。
func TestMoleReenterKeepsSeatAndCurrentLedger(t *testing.T) {
	service := readyMole()
	first := enterMemory(t, service, 7, "old")
	if _, err := service.accounting.accept(exampleHit("pending", 7), false); err != nil {
		t.Fatal(err)
	}
	again, err := service.enter(7, "new", "access-02", "agent", silverBalance{Amount: 99999, Version: 9}.snapshot())
	if err != nil || again["attack_name"] != first["attack_name"] || again["seat"] != first["seat"] {
		t.Fatal("重进改变组或座位", err)
	}
	if again["silver"].(map[string]any)["available"] != int64(9900) {
		t.Fatal("查询覆盖待扣")
	}
	if _, err := service.leave(7, "old"); !errors.Is(err, errMoleStaleLogin) {
		t.Fatal("旧身份移除了新成员", err)
	}
	if _, err := service.leave(7, "new"); err != nil {
		t.Fatal(err)
	}
	if service.accounting.accounts[7] == nil || !service.accounting.hasPending(7) {
		t.Fatal("离开丢失待结算操作")
	}
	service.accounting.applyReceipt(batchReceipt{BatchID: "batch", Results: []operationResult{{HitID: "pending", Status: "committed", OriginBatch: "batch"}}, Balances: map[uint64]silverBalance{7: {Amount: 9900, Version: 2}}})
	service.releaseAccount(7)
	if service.accounting.accounts[7] != nil {
		t.Fatal("结清后未释放离开账户")
	}
}

// TestMoleLeaveWithoutGroupIsIdempotent 保护固定入口按用户和登录身份离开，无需 access 已知组名。
func TestMoleLeaveWithoutGroupIsIdempotent(t *testing.T) {
	service := readyMole()
	if _, err := service.leave(7, "login"); err != nil {
		t.Fatal(err)
	}
	enterMemory(t, service, 7, "login")
	for range 2 {
		if _, err := service.leave(7, "login"); err != nil {
			t.Fatal(err)
		}
	}
	if len(service.users) != 0 || len(service.groups) != 0 {
		t.Fatal("离开残留状态")
	}
}
