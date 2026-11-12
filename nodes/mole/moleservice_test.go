package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type cacheForget struct {
	userID     uint64
	membership string
}

// forgetSink 仅注入回收通知的接纳结果，不复制 GSR 邮箱；日志入口禁用输出。
type forgetSink struct {
	gsr.CommandContext
	reject   bool
	attempts int
	notices  []cacheForget
}

func (*forgetSink) Source() runtime.ServiceID {
	return 1
}

func (*forgetSink) Logger() *logging.ServiceLogger {
	return nil
}

func (sink *forgetSink) SendName(name cluster.ServiceName, command runtime.CommandID, args ...any) error {
	if name != moleAccountingName || command != accountingForgetCommand || len(args) != 2 {
		return errInvalidMoleInput
	}
	sink.attempts++
	if sink.reject {
		return runtime.ErrMailboxFull
	}
	sink.notices = append(sink.notices, cacheForget{userID: args[0].(uint64), membership: args[1].(string)})
	return nil
}

// TestMoleDirectoryRetriesCacheCleanup 保护离开通知在邮箱满时不丢失，后续周期补投一次，恢复后不重复发送。
func TestMoleDirectoryRetriesCacheCleanup(t *testing.T) {
	service := newMoleService()
	service.routes[7] = moleRoute{Attack: "attack", LoginID: "login", Membership: "member"}
	service.groups["attack"] = 1
	sink := &forgetSink{reject: true}
	if err := service.handleDeparted(sink, 7, "login", "attack", "member"); err != nil {
		t.Fatal(err)
	}
	if len(service.routes) != 0 || len(service.groups) != 0 {
		t.Fatal("回收背压阻止了目录离开")
	}
	for range 3 {
		if err := service.handleHealth(sink); err != nil {
			t.Fatal(err)
		}
	}
	if sink.attempts != 4 || len(sink.notices) != 0 {
		t.Fatalf("满邮箱应每次只尝试一条并保留通知: attempts=%d notices=%v", sink.attempts, sink.notices)
	}
	sink.reject = false
	for range 2 {
		if err := service.handleHealth(sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.notices) != 1 || sink.notices[0] != (cacheForget{7, "member"}) {
		t.Fatalf("补投丢失、重复或身份错误: %v", sink.notices)
	}
}

// TestMoleDiscardedGroupCleansOnlyItsAccounts 保护整组清理回收闲置缓存、保留待结算扣款，且不影响其他组。
func TestMoleDiscardedGroupCleansOnlyItsAccounts(t *testing.T) {
	accounting := fundedAccounting(t, 1000)
	event := exampleHit("pending", 7)
	if _, err := accounting.accept(event, false); err != nil {
		t.Fatal(err)
	}
	submission := &submissionSink{}
	if err := accounting.flush(submission); err != nil || len(submission.batches) != 1 {
		t.Fatalf("待操作未交给提交方: %v", err)
	}
	for _, uid := range []uint64{8, 9} {
		if _, err := accounting.handleSetBalance(nil, uid, "member", silverBalance{Amount: 1000, Version: 1}.snapshot()); err != nil {
			t.Fatal(err)
		}
	}
	service := newMoleService()
	service.routes[7] = moleRoute{Attack: "stopped", LoginID: "login", Membership: "member-test"}
	service.routes[8] = moleRoute{Attack: "stopped", LoginID: "login", Membership: "member"}
	service.routes[9] = moleRoute{Attack: "other", LoginID: "login", Membership: "member"}
	service.groups["stopped"], service.groups["other"] = 2, 1
	sink := &forgetSink{}
	if err := service.handleDiscardGroup(sink, "stopped"); err != nil {
		t.Fatal(err)
	}
	if len(service.routes) != 1 || service.routes[9].Attack != "other" || len(service.groups) != 1 || service.groups["other"] != 1 {
		t.Fatal("清理已停止组时误删了其他成员")
	}
	if len(sink.notices) != 2 {
		t.Fatalf("未为整组发送回收通知: %v", sink.notices)
	}
	for _, notice := range sink.notices {
		if err := accounting.handleForgetBalance(nil, notice.userID, notice.membership); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := accounting.handleLoadBalance(nil, 8); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("闲置账户未回收: %v", err)
	}
	if accountAvailable(t, accounting, 7) != 900 || !accounting.hasPending(7) || !accounting.accounts[7].Departed {
		t.Fatal("整组回收丢失待结算扣款或未标记离开")
	}
	if accountAvailable(t, accounting, 9) != 1000 {
		t.Fatal("整组回收影响了其他组的余额")
	}
	batchID := submission.batches[0].ID
	if err := accounting.handleSettlementReceipt(sink, batchReceipt{
		BatchID:  batchID,
		Results:  []operationResult{{HitID: event.HitID, Status: "committed", OriginBatch: batchID}},
		Balances: map[uint64]silverBalance{7: {Amount: 900, Version: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := accounting.handleLoadBalance(nil, 7); !errors.Is(err, errAccountingUnavailable) {
		t.Fatalf("待扣结清后未回收已离开的缓存: %v", err)
	}
}

// TestMoleCacheCleanupPerCallbackIsBounded 保护回收积压分多次处理，单次目录回调最多投递一批且最终不丢失、不重复。
func TestMoleCacheCleanupPerCallbackIsBounded(t *testing.T) {
	service := newMoleService()
	sink := &forgetSink{reject: true}
	total := 2*maxForgetPerFlush + 1
	for index := range total {
		userID, attack := uint64(index+1), fmt.Sprintf("attack-%d", index)
		service.routes[userID] = moleRoute{Attack: attack, LoginID: "login", Membership: "member"}
		service.groups[attack] = 1
		if err := service.handleDeparted(sink, userID, "login", attack, "member"); err != nil {
			t.Fatal(err)
		}
	}
	sink.reject = false
	if err := service.handleHealth(sink); err != nil || len(sink.notices) != maxForgetPerFlush {
		t.Fatalf("单次补投未遵守批量上限: notices=%d err=%v", len(sink.notices), err)
	}
	for range 3 {
		if err := service.handleHealth(sink); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[uint64]bool)
	for _, notice := range sink.notices {
		if seen[notice.userID] || notice.membership != "member" {
			t.Fatalf("重复或错误的回收通知: %+v", notice)
		}
		seen[notice.userID] = true
	}
	if len(seen) != total {
		t.Fatalf("积压未全部补投: got=%d want=%d", len(seen), total)
	}
}

// TestMoleCleanupKeepsNewestMembership 保护多次离开积压时只补投最新成员实例，旧清理不能覆盖当前待投通知。
func TestMoleCleanupKeepsNewestMembership(t *testing.T) {
	service := newMoleService()
	sink := &forgetSink{reject: true}
	for _, membership := range []string{"old", "new"} {
		service.routes[7] = moleRoute{Attack: "attack", LoginID: "login", Membership: membership}
		service.groups["attack"] = 1
		if err := service.handleDeparted(sink, 7, "login", "attack", membership); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.handleDeparted(sink, 7, "login", "attack", "old"); err != nil {
		t.Fatal(err)
	}
	sink.reject = false
	if err := service.handleHealth(sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.notices) != 1 || sink.notices[0] != (cacheForget{7, "new"}) {
		t.Fatalf("补投未保留最新成员实例: %v", sink.notices)
	}
}

// TestMoleDirectoryIgnoresPreviousMembershipCleanup 保护相同 LoginID 退出再进入时旧清理通知不会删除新成员。
func TestMoleDirectoryIgnoresPreviousMembershipCleanup(t *testing.T) {
	service := newMoleService()
	service.routes[7] = moleRoute{Attack: "attack", LoginID: "login", Membership: "new"}
	service.groups["attack"] = 1
	if service.removeMember(7, "login", "attack", "old") || len(service.routes) != 1 {
		t.Fatal("old cleanup removed new membership")
	}
	if !service.removeMember(7, "login", "attack", "new") || len(service.routes) != 0 || len(service.groups) != 0 {
		t.Fatal("current cleanup failed")
	}
}
