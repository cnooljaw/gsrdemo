package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

type remoteReservationContext struct{ *tableCleanupContext }

var _ gsr.CommandContext = (*remoteReservationContext)(nil)

func (*remoteReservationContext) Source() runtime.ServiceID { return 0 }

// TestReservationReconcileRejectsInvalidSourceOrRoster 保护零来源和重复成员快照不能驱动目录回收。
func TestReservationReconcileRejectsInvalidSourceOrRoster(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	service.tables["table"] = landlordTable{Count: 1, Pending: map[uint64]time.Time{1: time.Now().Add(-time.Second)}}
	service.routes[1] = landlordRoute{"table", "login", "member"}
	if _, err := service.handleReconcileReservations(&remoteReservationContext{}, "table", [3]landlordMember{}); err != runtime.ErrUnknownCommand {
		t.Fatalf("零来源未被拒绝: %v", err)
	}
	duplicate := landlordMember{1, "login", "member"}
	if _, err := service.handleReconcileReservations(&tableCleanupContext{}, "table", [3]landlordMember{duplicate, duplicate}); err != errLandlordInput {
		t.Fatalf("重复成员未被拒绝: %v", err)
	}
	if len(service.routes) != 1 || service.tables["table"].Count != 1 {
		t.Fatal("拒绝的核对仍然改变目录")
	}
}

// TestReservationReconcileFailureKeepsEmptyTable 保护目录核对失败时空桌保留原预留，积压 Tick 退避，成功确认无预留后才退出。
func TestReservationReconcileFailureKeepsEmptyTable(t *testing.T) {
	service := newAttackService("table", landlordJournal{}, nil, "settlement")
	directory := newLandlordService(landlordJournal{}, "settlement")
	directory.tables["table"] = landlordTable{Count: 1, Pending: map[uint64]time.Time{1: time.Now().Add(-time.Second)}}
	directory.routes[1] = landlordRoute{"table", "login", "member"}
	self := &tableCleanupContext{directory: directory, reconcileError: context.DeadlineExceeded}
	if err := service.handleTick(self); err != context.DeadlineExceeded || self.exited || len(directory.routes) != 1 {
		t.Fatalf("核对未知时提前退出或释放预留: %v", err)
	}
	for range 8 {
		if err := service.handleTick(self); err != nil {
			t.Fatal(err)
		}
	}
	if self.reconcileCalls != 1 {
		t.Fatal("积压 Tick 重复等待目录")
	}
	self.reconcileError = nil
	service.reservationCheckAt = time.Time{}
	if err := service.handleTick(self); err != nil || !self.exited || len(directory.routes) != 0 || self.reconcileCalls != 2 {
		t.Fatalf("目录恢复后空桌没有退出: %v", err)
	}
}

// TestUnjoinedReservationsExpire 保护未调用或明确拒绝 Join 的预留在三十秒到期后释放，重复核对不重复回收。
func TestUnjoinedReservationsExpire(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	service.tables["table"] = landlordTable{}
	for uid := uint64(1); uid <= 3; uid++ {
		if _, err := service.handleResolve(nil, uid, "login"); err != nil {
			t.Fatal(err)
		}
	}
	if !service.reconcileReservations("table", [3]landlordMember{}, time.Now().Add(29*time.Second)) || len(service.routes) != 3 {
		t.Fatal("未满三十秒的预留被回收")
	}
	for range 2 {
		if service.reconcileReservations("table", [3]landlordMember{}, time.Now().Add(31*time.Second)) || len(service.routes) != 0 || len(service.tables) != 0 {
			t.Fatal("到期的空桌预留没有被完整回收")
		}
	}
}

// TestReservationReconciliationPreservesMembers 保护已入座身份不被到期清理，重登未完成时先保护新预留、到期后恢复原成员路由。
func TestReservationReconciliationPreservesMembers(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	service.tables["table"] = landlordTable{}
	first, err := service.handleResolve(nil, 1, "login")
	if err != nil {
		t.Fatal(err)
	}
	old := landlordMember{1, "login", first["membership"].(string)}
	members := [3]landlordMember{old}
	if !service.reconcileReservations("table", members, time.Now().Add(31*time.Second)) || len(service.routes) != 1 || len(service.tables["table"].Pending) != 0 {
		t.Fatal("已经入座的成员仍会过期")
	}
	second, err := service.handleResolve(nil, 1, "new-login")
	if err != nil {
		t.Fatal(err)
	}
	deadline := service.tables["table"].Pending[1]
	if !service.reconcileReservations("table", members, deadline.Add(-time.Nanosecond)) || service.routes[1].Membership != second["membership"] {
		t.Fatal("仍有效的新登录预留被旧成员覆盖")
	}
	if !service.reconcileReservations("table", members, deadline) || service.routes[1] != (landlordRoute{"table", old.LoginID, old.Membership}) || service.tables["table"].Count != 1 {
		t.Fatal("重登预留过期后没有保留原成员")
	}
	if allowed, err := service.handleCheck(nil, 1, "new-login", "table", second["membership"].(string)); err != nil || allowed {
		t.Fatalf("迟到 Join 仍可使用过期预留: %v", err)
	}
}

// TestNewReservationSupersedesOldDeadline 保护新 Resolve 更新期限，旧快照核对不能按上一次到期时间清掉新尝试，过期旧 Join 立即拒绝。
func TestNewReservationSupersedesOldDeadline(t *testing.T) {
	service := newLandlordService(landlordJournal{}, "settlement")
	service.tables["table"] = landlordTable{}
	first, err := service.handleResolve(nil, 1, "old-login")
	if err != nil {
		t.Fatal(err)
	}
	table := service.tables["table"]
	table.Pending[1] = time.Now().Add(-time.Second)
	if allowed, err := service.handleCheck(nil, 1, "old-login", "table", first["membership"].(string)); err != nil || allowed {
		t.Fatal("过期 Join 未被拒绝")
	}
	second, err := service.handleResolve(nil, 1, "new-login")
	if err != nil {
		t.Fatal(err)
	}
	if !service.reconcileReservations("table", [3]landlordMember{}, time.Now()) || service.routes[1].Membership != second["membership"] || service.tables["table"].Count != 1 {
		t.Fatal("新期限没有保护当前预留")
	}
}

// TestReservationExpiryPreservesGameAndPendingSettlement 保护新登录预留过期时，进行中手牌及待结算三人身份不被回收。
func TestReservationExpiryPreservesGameAndPendingSettlement(t *testing.T) {
	for _, phase := range []string{"playing", "finished"} {
		t.Run(phase, func(t *testing.T) {
			service := newAttackService("table-test", landlordJournal{}, nil, "settlement")
			service.game = *testLandlordGame(t)
			service.game.Phase = phase
			if phase == "finished" {
				service.game.Winner = 0
				service.settlement.prepareOperation(service.game)
				service.settlement.retryAt = time.Now().Add(time.Hour)
			}
			directory := directoryForLandlordTables(service)
			original := directory.routes[1]
			if _, err := directory.handleResolve(nil, 1, "new-login"); err != nil {
				t.Fatal(err)
			}
			directory.tables[service.game.Name].Pending[1] = time.Now().Add(-time.Second)
			before := service.game.view(1)
			self := &tableCleanupContext{directory: directory}
			if err := service.handleTick(self); err != nil {
				t.Fatal(err)
			}
			if self.exited || self.releaseCalls != 0 || !reflect.DeepEqual(before, service.game.view(1)) || directory.routes[1] != original || directory.tables[service.game.Name].Count != 3 {
				t.Fatal("过期回收改变了真实成员、手牌或原目录")
			}
			if phase == "finished" && !service.settlement.pending() {
				t.Fatal("待结算身份被当作过期预留清理")
			}
		})
	}
}
