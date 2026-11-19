package main

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

//rpc:service dhmjpulse private
const pulseTickCommand runtime.CommandID = 1

const pulsePeriod = 250 * time.Millisecond

// pulseService 独立唤醒牌桌或目录。目标邮箱满时保留下一次唤醒。
type pulseService struct {
	target  runtime.ServiceID
	command runtime.CommandID
	period  time.Duration
}

var _ gsr.Service = (*pulseService)(nil)

func (pulse *pulseService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(pulse.period, pulseTickCommand)
	return err
}

func (*pulseService) OnStop(context.Context) error { return nil }

func (pulse *pulseService) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != pulseTickCommand || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	if _, err := self.After(pulse.period, pulseTickCommand); err != nil {
		return nil, err
	}
	err := self.Send(pulse.target, pulse.command)
	if err != nil && !errors.Is(err, runtime.ErrMailboxFull) {
		self.Exit()
		return nil, err
	}
	return nil, nil
}
