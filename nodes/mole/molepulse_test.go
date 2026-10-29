package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type busyMoleFixture struct {
	owner   gsr.Service
	command runtime.CommandID
	blocked chan struct{}
	release chan struct{}
	ticks   chan struct{}
}

var _ gsr.Service = (*busyMoleFixture)(nil)

func (service *busyMoleFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return service.owner.OnStart(ctx, self)
}

func (service *busyMoleFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	switch command {
	case 998:
		close(service.blocked)
		<-service.release
		return nil, nil
	case 999:
		return nil, nil
	case service.command:
		select {
		case service.ticks <- struct{}{}:
		default:
		}
	}
	return service.owner.Handle(self, command, args...)
}

func (service *busyMoleFixture) OnStop(ctx context.Context) error {
	return service.owner.OnStop(ctx)
}

type saturatedMoleMain struct {
	owner *busyMoleFixture
	ready chan struct{}
}

var _ gsr.Service = (*saturatedMoleMain)(nil)

func (service *saturatedMoleMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, service.owner)
	if err != nil {
		return err
	}
	if err := self.Send(id, 998); err != nil {
		return err
	}
	select {
	case <-service.owner.blocked:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := self.Send(id, 999); err != nil {
		return err
	}
	close(service.ready)
	self.Exit()
	return nil
}

func (service *saturatedMoleMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *saturatedMoleMain) OnStop(context.Context) error { return nil }

// TestMoleTicksSurviveFullMailbox 保护刷批、提交重试和刷地鼠在满队列拒绝唤醒后均能继续，不需要新请求唤醒。
func TestMoleTicksSurviveFullMailbox(t *testing.T) {
	for _, name := range []string{"accounting", "submission", "attack"} {
		t.Run(name, func(t *testing.T) {
			var inner gsr.Service
			var command runtime.CommandID
			switch name {
			case "accounting":
				inner, command = newMoleAccountingService(1), accountingTickCommand
			case "submission":
				inner, command = newMoleSubmissionService(testMoleCheckpoint(t), 0), submissionTickCommand
			case "attack":
				inner, command = newAttackService("attack-timer"), attackTickCommand
			}
			owner := &busyMoleFixture{owner: inner, command: command, blocked: make(chan struct{}), release: make(chan struct{}), ticks: make(chan struct{}, 8)}
			main := &saturatedMoleMain{owner: owner, ready: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			var release sync.Once
			unblock := func() { release.Do(func() { close(owner.release) }) }
			t.Cleanup(func() {
				unblock()
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("shutdown timeout")
				}
			})
			directory := t.TempDir()
			go func() {
				done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: directory}, Runtime: gsr.RuntimeConfig{MailboxCapacity: 1}}, main)
			}()
			select {
			case <-main.ready:
			case <-time.After(time.Second):
				t.Fatal("not ready")
			}
			period := accountingPeriod
			if name == "attack" {
				period = spawnPeriod
			}
			time.Sleep(2*period + 50*time.Millisecond)
			unblock()
			select {
			case <-owner.ticks:
			case <-time.After(2 * period):
				t.Fatal("tick stopped after mailbox rejection")
			}
		})
	}
}

// panicMoleFixture 只注入 owner panic，不改变正常业务命令。
type panicMoleFixture struct{ owner gsr.Service }

var _ gsr.Service = (*panicMoleFixture)(nil)

func (service *panicMoleFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return service.owner.OnStart(ctx, self)
}
func (service *panicMoleFixture) OnStop(ctx context.Context) error { return service.owner.OnStop(ctx) }
func (service *panicMoleFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == 997 {
		panic("injected essential owner panic")
	}
	return service.owner.Handle(self, command, args...)
}

type panicMoleMain struct{ owner gsr.Service }

var _ gsr.Service = (*panicMoleMain)(nil)

func (service *panicMoleMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, &panicMoleFixture{owner: service.owner})
	if err != nil {
		return err
	}
	if err := self.Send(id, 997); err != nil {
		return err
	}
	self.Exit()
	return nil
}
func (service *panicMoleMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *panicMoleMain) OnStop(context.Context) error { return nil }

// TestMoleEssentialOwnerPanicStopsHost 保护全局目录或内存账退出后宿主明确失败，不维持只有端口可用的假健康进程。
func TestMoleEssentialOwnerPanicStopsHost(t *testing.T) {
	for _, name := range []string{"accounting", "directory"} {
		t.Run(name, func(t *testing.T) {
			ctx, fail := context.WithCancelCause(context.Background())
			defer fail(nil)
			var owner gsr.Service
			if name == "accounting" {
				service := newMoleAccountingService(1)
				service.onStopped = fail
				owner = service
			} else {
				service := newMoleService()
				service.onStopped = fail
				owner = service
			}
			done := make(chan error, 1)
			directory := t.TempDir()
			go func() {
				done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: directory}}, &panicMoleMain{owner: owner})
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
				if context.Cause(ctx) == nil || context.Cause(ctx) == context.Canceled {
					t.Fatal("essential failure was hidden")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("host stayed alive after essential owner panic")
			}
		})
	}
}
