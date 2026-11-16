package main

import (
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type moleRefreshContext struct {
	gsr.CommandContext
	commands []runtime.CommandID
	sendErr  error
	onSubmit func()
}

var _ gsr.CommandContext = (*moleRefreshContext)(nil)

func (*moleRefreshContext) Self() runtime.ServiceID        { return 7 }
func (*moleRefreshContext) Source() runtime.ServiceID      { return 7 }
func (*moleRefreshContext) Cluster() *gsr.Cluster          { return &gsr.Cluster{} }
func (*moleRefreshContext) Logger() *logging.ServiceLogger { return nil }
func (self *moleRefreshContext) Send(id runtime.ServiceID, command runtime.CommandID, _ ...any) error {
	if self.sendErr != nil {
		return self.sendErr
	}
	if id != self.Self() {
		return runtime.ErrServiceNotFound
	}
	self.commands = append(self.commands, command)
	return nil
}
func (self *moleRefreshContext) SendName(_ cluster.ServiceName, _ runtime.CommandID, _ ...any) error {
	if self.onSubmit != nil {
		self.onSubmit()
	}
	return nil
}

func dueMoleGroups(t *testing.T) *moleService {
	t.Helper()
	service := readyMole()
	for uid := uint64(1); uid <= 600; uid++ {
		enterMemory(t, service, uid, "load")
	}
	due := time.Now().Add(-time.Second)
	for _, group := range service.groups {
		group.nextSpawnAt = due
	}
	return service
}

func refreshedMoleGroups(service *moleService) int {
	count := 0
	for _, group := range service.groups {
		if group.game.sequence > 1 {
			count++
		}
	}
	return count
}

// TestMoleRefreshYieldsAndResumes 保护大批到期分段执行，段间可取快照和进出组，后续既不漏刷也不重复刷新。
func TestMoleRefreshYieldsAndResumes(t *testing.T) {
	service, self := dueMoleGroups(t), &moleRefreshContext{}
	if err := service.handleTick(self); err != nil {
		t.Fatal(err)
	}
	if count := refreshedMoleGroups(service); count == 0 || count > 128 || len(self.commands) != 1 {
		t.Fatalf("首段没有让出邮箱或未安排唯一续批: refreshed=%d queued=%d", count, len(self.commands))
	}
	var removed *moleGroup
	for _, group := range service.groups {
		if group.game.sequence == 1 {
			removed = group
			break
		}
	}
	for uid := range removed.members {
		if _, err := service.handleSnapshot(self, uid, "load", removed.game.name); err != nil {
			t.Fatal("分批期间快照不能执行", err)
		}
		if _, err := service.leave(uid, "load"); err != nil {
			t.Fatal(err)
		}
	}
	entered := enterMemory(t, service, 601, "load")
	newGroup := entered["attack_name"].(string)
	// 多个积压 Pulse 可以归批，但不得为同一刷新工作叠加续批消息。
	for range 4 {
		if err := service.handleTick(self); err != nil {
			t.Fatal(err)
		}
	}
	if len(self.commands) != 1 {
		t.Fatal("Pulse 重复安排续批")
	}
	for steps := 0; len(self.commands) > 0; steps++ {
		if steps > 200 {
			t.Fatal("续批没有收敛")
		}
		command := self.commands[0]
		self.commands = self.commands[1:]
		if _, err := service.Handle(self, command); err != nil {
			t.Fatal(err)
		}
	}
	if removed.game.sequence != 1 {
		t.Fatal("已删除组仍被刷新")
	}
	for name, group := range service.groups {
		want := uint64(2)
		if name == newGroup {
			want = 1
		}
		if group.game.sequence != want {
			t.Fatalf("组被漏刷、重复刷新或提前刷新: %s sequence=%d", name, group.game.sequence)
		}
	}
}

// TestMoleRefreshFullMailboxResumesOnTick 保护续批发送被满邮箱拒绝后，后续 Pulse 仍可完成剩余到期刷新。
func TestMoleRefreshFullMailboxResumesOnTick(t *testing.T) {
	service := dueMoleGroups(t)
	self := &moleRefreshContext{sendErr: runtime.ErrMailboxFull}
	if err := service.handleTick(self); err != nil {
		t.Fatal(err)
	}
	if count := refreshedMoleGroups(service); count == 0 || count > 128 || len(self.commands) != 0 {
		t.Fatalf("满邮箱时未限制工作或保留了不存在的续批: %d", count)
	}
	for steps := 0; refreshedMoleGroups(service) < 200; steps++ {
		if steps > 200 {
			t.Fatal("后续 Pulse 没有补做刷新")
		}
		if err := service.handleTick(self); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMoleTickSubmitsBeforeRefreshing 保护百毫秒归批在当次刷新前执行，不因大量到期组排到刷新之后。
func TestMoleTickSubmitsBeforeRefreshing(t *testing.T) {
	service := dueMoleGroups(t)
	if _, err := service.accounting.accept(exampleHit("refresh-test", 1), false); err != nil {
		t.Fatal(err)
	}
	submitted := false
	self := &moleRefreshContext{onSubmit: func() {
		submitted = true
		if count := refreshedMoleGroups(service); count != 0 {
			t.Errorf("归批发生在刷新之后: %d", count)
		}
	}}
	if err := service.handleTick(self); err != nil {
		t.Fatal(err)
	}
	if !submitted || service.accounting.inFlightID == "" {
		t.Fatal("未先提交待操作")
	}
}

type foreignMoleRefreshContext struct {
	*moleRefreshContext
	source runtime.ServiceID
}

var _ gsr.CommandContext = (*foreignMoleRefreshContext)(nil)

func (self *foreignMoleRefreshContext) Source() runtime.ServiceID { return self.source }

// TestMoleRefreshRequiresSelfSource 保护远端和其他本地 Service 不能推进自身续批或清掉在途标记。
func TestMoleRefreshRequiresSelfSource(t *testing.T) {
	service := dueMoleGroups(t)
	service.refreshQueued = true
	for _, source := range []runtime.ServiceID{0, 8} {
		self := &foreignMoleRefreshContext{moleRefreshContext: &moleRefreshContext{}, source: source}
		if _, err := service.Handle(self, moleRefreshCommand); err != errInvalidMoleInput || !service.refreshQueued || refreshedMoleGroups(service) != 0 {
			t.Fatalf("外来来源推进了续批: source=%d err=%v", source, err)
		}
	}
}
