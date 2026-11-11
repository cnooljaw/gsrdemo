package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	landlordrpc "github.com/cnooljaw/gsrdemo/common/rpc/landlord"
)

type tableCleanupContext struct {
	gsr.CommandContext
	directory *landlordService
	exited    bool
}

var _ gsr.CommandContext = (*tableCleanupContext)(nil)

func (*tableCleanupContext) Source() runtime.ServiceID { return 1 }
func (self *tableCleanupContext) Exit()                { self.exited = true }
func (self *tableCleanupContext) CallName(_ context.Context, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	if name != landlordrpc.ServiceName || command != landlordrpc.ReleaseCommand {
		return nil, runtime.ErrUnknownCommand
	}
	return self.directory.Handle(self, command, args...)
}

func detachedTable(t *testing.T) (*attackService, *tableCleanupContext) {
	t.Helper()
	service := newAttackService("table-cleanup", landlordJournal{}, nil, "settlement")
	service.game.Phase, service.game.Landlord = "playing", 0
	directory := newLandlordService(landlordJournal{}, "settlement")
	directory.tables[service.game.Name] = landlordTable{Count: 3, Sealed: true}
	self := &tableCleanupContext{directory: directory}
	for i := range service.game.Players {
		uid, membership := uint64(i+1), fmt.Sprintf("member-%d", i)
		service.game.Players[i] = landlordPlayer{UserID: uid, LoginID: "login", AccessNode: "access", AgentName: "agent", Membership: membership}
		directory.routes[uid] = landlordRoute{service.game.Name, "login", membership}
		if left, err := service.handleLeave(self, uid, "login"); err != nil || !left {
			t.Fatalf("leave: %v %v", left, err)
		}
	}
	return service, self
}

// TestDetachedTableReleasesMembersAtTerminal 保护进行中离开的成员在结算完成或全不叫回到等待后释放目录和空桌。
func TestDetachedTableReleasesMembersAtTerminal(t *testing.T) {
	for _, phase := range []string{"finished", "waiting"} {
		t.Run(phase, func(t *testing.T) {
			service, self := detachedTable(t)
			service.game.Phase = phase
			if phase == "finished" {
				service.game.Winner = 0
				service.settlement.prepareOperation(service.game)
				service.settlement.operation.Status = "committed"
			}
			if err := service.handleTick(self); err != nil {
				t.Fatal(err)
			}
			if !self.exited || len(self.directory.routes) != 0 || len(self.directory.tables) != 0 {
				t.Fatalf("空桌未回收: exited=%v routes=%v tables=%v", self.exited, self.directory.routes, self.directory.tables)
			}
		})
	}
}

// TestDetachedMemberWaitsForNewJoin 保护新登录已经预留但尚未 Join 时，旧成员回收不删除新座位或提前退出桌。
func TestDetachedMemberWaitsForNewJoin(t *testing.T) {
	service, self := detachedTable(t)
	service.game.Phase, service.game.Winner = "finished", 0
	service.settlement.prepareOperation(service.game)
	service.settlement.operation.Status = "committed"
	self.directory.routes[1] = landlordRoute{service.game.Name, "new-login", "new-membership"}
	if err := service.handleTick(self); err != nil {
		t.Fatal(err)
	}
	if self.exited || service.game.Players[0].UserID != 1 || self.directory.routes[1].Membership != "new-membership" || len(self.directory.routes) != 1 {
		t.Fatal("旧清理影响了待加入的新成员")
	}
}

// TestDetachedTableKeepsPendingParticipants 保护成员提前离开后仍保留待确认结算的三人身份，确认后才能释放。
func TestDetachedTableKeepsPendingParticipants(t *testing.T) {
	service, self := detachedTable(t)
	service.game.Phase, service.game.Winner = "finished", 0
	service.settlement.prepareOperation(service.game)
	service.settlement.retryAt = time.Now().Add(time.Hour)
	if err := service.handleTick(self); err != nil {
		t.Fatal(err)
	}
	if self.exited || len(self.directory.routes) != 3 || service.game.Players[0].UserID != 1 {
		t.Fatal("待确认结算的成员被提前回收")
	}
}

// TestEmptyReservedTableWaitsForFirstJoin 保护刚创建但尚未收到 Join 的空桌不会被 Pulse 提前回收。
func TestEmptyReservedTableWaitsForFirstJoin(t *testing.T) {
	service := newAttackService("new-table", landlordJournal{}, nil, "settlement")
	self := &tableCleanupContext{}
	if err := service.handleTick(self); err != nil || self.exited {
		t.Fatalf("首次 Join 前桌已退出: %v", err)
	}
}
