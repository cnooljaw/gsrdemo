package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// molePulse 仅接收自己的 Timer，不发布名称、不执行业务或 I/O。
// 目标 Mailbox 满时丢弃本次唤醒，下一次仍会尝试，避免 owner 自续约链断裂。
type molePulse struct {
	target     runtime.ServiceID
	command    runtime.CommandID
	period     time.Duration
	failedLane int
	onStopped  func(error)
}

var _ gsr.Service = (*molePulse)(nil)

func startMolePulse(ctx context.Context, self gsr.ServiceContext, period time.Duration, command runtime.CommandID, onStopped func(error)) error {
	_, err := self.CreateService(ctx, &molePulse{target: self.Self(), command: command, period: period, failedLane: -1, onStopped: onStopped})
	return err
}

func (service *molePulse) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(service.period, 1)
	return err
}

func (service *molePulse) Handle(self gsr.CommandContext, command runtime.CommandID, _ ...any) (any, error) {
	if command != 1 || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	if _, err := self.After(service.period, 1); err != nil {
		self.Exit()
		return nil, err
	}
	err := self.Send(service.target, service.command)
	if errors.Is(err, runtime.ErrServiceNotFound) || errors.Is(err, runtime.ErrServiceClosed) || errors.Is(err, runtime.ErrServiceStopping) {
		if service.failedLane >= 0 {
			if notifyErr := self.SendName(moleAccountingName, accountingStopLaneCommand, uint64(service.failedLane)); notifyErr != nil {
				return nil, notifyErr
			}
		}
		if service.onStopped != nil {
			service.onStopped(fmt.Errorf("essential mole service %d stopped: %w", service.target, err))
		}
		self.Exit()
		return nil, nil
	}
	// MailboxFull 是本次唤醒的背压，不是取消后续推进。
	return nil, err
}

func (service *molePulse) OnStop(context.Context) error { return nil }
