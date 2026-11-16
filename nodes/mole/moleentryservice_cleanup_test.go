package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
)

// preparationProbe 只注入本地接纳故障或延迟准备回包，不替代目录或 Accounting 的状态变化。
type preparationProbe struct {
	rejectReserve, rejectCleanup atomic.Bool
	delayPreparation             atomic.Uint32
	preparations                 atomic.Uint64
}
type preparationContext struct {
	gsr.CommandContext
	probe *preparationProbe
}

func (self *preparationContext) CallName(ctx context.Context, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	if name == moleAccountingName && (command == accountingResumeCommand || command == accountingSetBalanceCommand) {
		self.probe.preparations.Add(1)
	}
	if name == moleServiceName && command == moleReserveCommand && self.probe.rejectReserve.Swap(false) {
		return nil, runtime.ErrMailboxFull
	}
	if name == moleServiceName && command == moleReconcilePreparationCommand && self.probe.rejectCleanup.Load() {
		return nil, runtime.ErrMailboxFull
	}
	return self.CommandContext.CallName(ctx, name, command, args...)
}

type preparationAccounting struct {
	gsr.Service
	probe *preparationProbe
}

func (accounting *preparationAccounting) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	value, err := accounting.Service.Handle(self, command, args...)
	if err == nil && uint32(command) != 0 && accounting.probe.delayPreparation.CompareAndSwap(uint32(command), 0) {
		// 状态已由真实 Handler 修改，只延迟回包，让 Entry 的真实本地 Call 超时。
		time.Sleep(moleCallTimeout + 100*time.Millisecond)
	}
	return value, err
}

type preparationEntry struct {
	gsr.Service
	probe *preparationProbe
}

func (entry *preparationEntry) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return entry.Service.Handle(&preparationContext{self, entry.probe}, command, args...)
}

type preparationMainContext struct {
	gsr.ServiceContext
	probe *preparationProbe
}

func (self *preparationMainContext) CreateService(ctx context.Context, child gsr.Service) (runtime.ServiceID, error) {
	switch child.(type) {
	case *moleEntryService:
		child = &preparationEntry{child, self.probe}
	case *moleAccountingService:
		child = &preparationAccounting{child, self.probe}
	}
	return self.ServiceContext.CreateService(ctx, child)
}

type preparationMain struct {
	gsr.Service
	probe *preparationProbe
}

func (main *preparationMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return main.Service.OnStart(ctx, &preparationMainContext{self, main.probe})
}

func startPreparationMole(t *testing.T, probe *preparationProbe) map[cluster.NodeID]string {
	t.Helper()
	fixture := newSettlementFixture()
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	address, _ := startMoleTestNode(t, "mole", routes, &preparationMain{newMoleTestMain(t, fixture), probe})
	routes["mole"] = address
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Accounting 未初始化")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := moleRPC(t, routes, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uint64(1), "login", "access-01", "agent"); err != nil {
		t.Fatal(err)
	}
	return routes
}

// TestMoleReserveAdmissionCleanupRetries 保护背压时旧补偿不被新准备覆盖，背压恢复后自动回收无路由闲置缓存。
func TestMoleReserveAdmissionCleanupRetries(t *testing.T) {
	probe := &preparationProbe{}
	probe.rejectReserve.Store(true)
	probe.rejectCleanup.Store(true)
	routes := startPreparationMole(t, probe)
	if _, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent"); !errors.Is(err, runtime.ErrMailboxFull) {
		t.Fatalf("预留未返回原错误: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1)); err != nil {
		t.Fatal("背压前准备缓存丢失", err)
	}
	prepared := probe.preparations.Load()
	if _, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent"); !errors.Is(err, errMoleBusy) {
		t.Fatalf("旧补偿未交付仍开始新进入: %v", err)
	}
	if probe.preparations.Load() != prepared {
		t.Fatal("旧补偿未交付仍改变了余额准备身份")
	}
	probe.rejectCleanup.Store(false)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
		if errors.Is(err, errAccountingUnavailable) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("闲置准备缓存未回收: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 下一次显式进入能重新加载缓存、入组并实际命中。
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "after-cleanup")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}

// TestMolePreparationTimeoutCleanup 保护余额准备已执行但回包超时后，首次缓存和旧成员离开后的缓存都能回收。
func TestMolePreparationTimeoutCleanup(t *testing.T) {
	for _, command := range []runtime.CommandID{accountingSetBalanceCommand, accountingResumeCommand} {
		name := "SetBalance"
		if command == accountingResumeCommand {
			name = "ResumeThenLeave"
		}
		t.Run(name, func(t *testing.T) {
			probe := &preparationProbe{}
			routes := startPreparationMole(t, probe)
			var attack string
			if command == accountingResumeCommand {
				attack = enterMoleTestUser(t, routes, 1, "login", "access-01")["attack_name"].(string)
			}
			probe.delayPreparation.Store(uint32(command))
			// Entry 原样返回本地 context 超时，跨节点边界当前将其封装为 HandlerFailed。
			if _, err := callMoleEnter(t, routes, uint64(1), "login", "access-01", "agent"); !errors.Is(err, runtime.ErrHandlerFailed) {
				t.Fatalf("准备未按预期超时: %v", err)
			}
			if probe.delayPreparation.Load() != 0 {
				t.Fatal("未经过真实余额准备 Handler")
			}
			if attack != "" {
				if value, err := moleRPC(t, routes, "mole", attack, attackLeaveCommand, uint64(1), "login"); err != nil || value != true {
					t.Fatalf("旧成员离开失败: %v %v", value, err)
				}
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				_, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
				if errors.Is(err, errAccountingUnavailable) {
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatalf("准备超时留下无路由闲置缓存: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			// 清理后显式重进仍能命中并按原链路结算。
			snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
			hit := testHitTarget(t, routes, snapshot, 1, "login", "after-preparation-timeout")
			waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
		})
	}
}

type preparationNotice struct {
	command runtime.CommandID
	args    []any
}

// preparationDelivery 只观察目录向 Accounting 的接纳顺序，不保存真实回调上下文。
type preparationDelivery struct {
	gsr.CommandContext
	reject  bool
	notices []preparationNotice
}

func (*preparationDelivery) Source() runtime.ServiceID { return 1 }
func (self *preparationDelivery) SendName(name cluster.ServiceName, command runtime.CommandID, args ...any) error {
	if name != moleAccountingName {
		return errInvalidMoleInput
	}
	if self.reject {
		return runtime.ErrMailboxFull
	}
	self.notices = append(self.notices, preparationNotice{command, append([]any(nil), args...)})
	return nil
}

// TestMolePreparationReconcilesOwnership 保护无路由回收、待扣保留、旧归属恢复与新尝试不被晚到补偿覆盖。
func TestMolePreparationReconcilesOwnership(t *testing.T) {
	for _, entry := range []struct {
		name                  string
		route, pending, newer bool
	}{
		{name: "idle"}, {name: "pending", pending: true}, {name: "old route", route: true},
		{name: "old route with pending", route: true, pending: true}, {name: "new membership", route: true, newer: true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			accounting := fundedAccounting(t, 1000)
			if entry.pending {
				if _, err := accounting.accept(exampleHit("pending", 7), false); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := accounting.handleResumeBalance(nil, 7, "failed-prepare"); err != nil {
				t.Fatal(err)
			}
			directory := newMoleService()
			if entry.route {
				directory.routes[7] = moleRoute{Attack: "attack", LoginID: "login", Membership: "old-member"}
			}
			sink := &preparationDelivery{}
			if err := directory.handleReconcilePreparation(sink, 7, "failed-prepare"); err != nil {
				t.Fatal(err)
			}
			if entry.newer {
				if _, err := accounting.handleResumeBalance(nil, 7, "newer-member"); err != nil {
					t.Fatal(err)
				}
			}
			if len(sink.notices) != 1 {
				t.Fatal("目录未交付修正")
			}
			notice := sink.notices[0]
			if _, err := accounting.Handle(sink, notice.command, notice.args...); err != nil {
				t.Fatal(err)
			}
			account := accounting.accounts[7]
			if !entry.route && !entry.pending {
				if account != nil {
					t.Fatal("无路由闲置缓存未释放")
				}
				return
			}
			expected := "failed-prepare"
			if entry.route {
				expected = "old-member"
			}
			if entry.newer {
				expected = "newer-member"
			}
			if account == nil || account.Membership != expected || account.Departed != !entry.route {
				t.Fatalf("归属错误: %+v", account)
			}
			expectedAmount := int64(1000)
			if entry.pending {
				expectedAmount = 900
			}
			if accountAvailable(t, accounting, 7) != expectedAmount || account.Balance.Amount != 1000 || account.Balance.Version != 1 || accounting.hasPending(7) != entry.pending {
				t.Fatal("补偿改变了账务")
			}
		})
	}
}

// TestMolePreparationAndLeaveOrder 保护目录在补偿前后处理旧 Leave 时，Accounting 最终都能回收闲置缓存。
func TestMolePreparationAndLeaveOrder(t *testing.T) {
	for _, leaveFirst := range []bool{false, true} {
		name := "reconcile first"
		if leaveFirst {
			name = "leave first"
		}
		t.Run(name, func(t *testing.T) {
			accounting := fundedAccounting(t, 1000)
			if _, err := accounting.handleResumeBalance(nil, 7, "failed-prepare"); err != nil {
				t.Fatal(err)
			}
			directory := newMoleService()
			directory.routes[7] = moleRoute{Attack: "attack", LoginID: "login", Membership: "old-member"}
			directory.groups["attack"] = 1
			sink := &preparationDelivery{}
			reconcile := func() {
				t.Helper()
				if err := directory.handleReconcilePreparation(sink, 7, "failed-prepare"); err != nil {
					t.Fatal(err)
				}
			}
			leave := func() {
				t.Helper()
				if err := directory.handleDeparted(sink, 7, "login", "attack", "old-member"); err != nil {
					t.Fatal(err)
				}
			}
			if leaveFirst {
				leave()
				reconcile()
			} else {
				reconcile()
				leave()
			}
			if len(sink.notices) != 2 {
				t.Fatal("目录通知丢失")
			}
			for _, notice := range sink.notices {
				if _, err := accounting.Handle(sink, notice.command, notice.args...); err != nil {
					t.Fatal(err)
				}
			}
			if accounting.accounts[7] != nil {
				t.Fatal("补偿与 Leave 交错留下无归属缓存")
			}
		})
	}
}

// TestMolePreparationForwardingBackpressure 保护目录转发失败返回背压，重试按最新路由判断归属而不使用旧快照。
func TestMolePreparationForwardingBackpressure(t *testing.T) {
	directory := newMoleService()
	directory.routes[7] = moleRoute{Membership: "old-member"}
	sink := &preparationDelivery{reject: true}
	if err := directory.handleReconcilePreparation(sink, 7, "failed-prepare"); !errors.Is(err, runtime.ErrMailboxFull) {
		t.Fatal(err)
	}
	delete(directory.routes, 7)
	sink.reject = false
	if err := directory.handleReconcilePreparation(sink, 7, "failed-prepare"); err != nil {
		t.Fatal(err)
	}
	if len(sink.notices) != 1 || sink.notices[0].args[2] != "" {
		t.Fatal("重试沿用了旧归属快照")
	}
}

// TestMoleReserveFailureClassification 保护超时、panic 和未知 Handler 结果不进入准备补偿。
func TestMoleReserveFailureClassification(t *testing.T) {
	for _, err := range []error{runtime.ErrMailboxFull, runtime.ErrServiceNotFound, runtime.ErrServiceClosed, runtime.ErrServiceStarting, runtime.ErrServiceStopping} {
		if !reserveNotAccepted(err) {
			t.Errorf("明确未接纳没有补偿: %v", err)
		}
	}
	for _, err := range []error{nil, runtime.ErrCallTimeout, context.DeadlineExceeded, runtime.ErrServicePanicked, runtime.ErrHandlerFailed} {
		if reserveNotAccepted(err) {
			t.Errorf("未知结果被撤销: %v", err)
		}
	}
}

type preparationCleanupSink struct {
	gsr.ServiceContext
	delivered map[uint64]string
}

func (self *preparationCleanupSink) CallName(_ context.Context, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	if name != moleServiceName || command != moleReconcilePreparationCommand {
		return nil, runtime.ErrUnknownCommand
	}
	self.delivered[args[0].(uint64)] = args[1].(string)
	return nil, nil
}

// TestMolePreparationCleanupIsBounded 保护补偿表满时不再准备新用户，每轮限量交付且已接纳任务不重复投递。
func TestMolePreparationCleanupIsBounded(t *testing.T) {
	service := newMoleEntryService("settlement")
	for userID := uint64(1); userID <= maxMoleUsers; userID++ {
		service.pendingCleanup[userID] = fmt.Sprintf("member-%d", userID)
	}
	if _, err := service.handleEnter(nil, maxMoleUsers+1, "login", "access-01", "agent"); !errors.Is(err, errMoleBusy) {
		t.Fatalf("补偿表满仍准备新用户: %v", err)
	}
	sink := &preparationCleanupSink{delivered: make(map[uint64]string)}
	service.retryPreparedCleanup(sink)
	if len(sink.delivered) != maxForgetPerFlush || len(service.pendingCleanup) != maxMoleUsers-maxForgetPerFlush {
		t.Fatal("补偿交付没有限量")
	}
	for userID := range sink.delivered {
		if _, exists := service.pendingCleanup[userID]; exists {
			t.Fatal("已接纳任务仍等待补投")
		}
	}
	service.retryPreparedCleanup(sink)
	if len(sink.delivered) != 2*maxForgetPerFlush {
		t.Fatal("下一轮重复交付已接纳任务")
	}
}
