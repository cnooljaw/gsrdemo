package dhmj

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

// tablePulse 只发送唤醒消息。牌桌邮箱满时保留下一次唤醒。
type tablePulse struct{ target runtime.ServiceID }

var _ gsr.Service = (*tablePulse)(nil)

func (*tablePulse) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(pulsePeriod, pulseTickCommand)
	return err
}

func (*tablePulse) OnStop(context.Context) error { return nil }

func (pulse *tablePulse) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != pulseTickCommand || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	if _, err := self.After(pulsePeriod, pulseTickCommand); err != nil {
		return nil, err
	}
	err := self.Send(pulse.target, tableTickCommand)
	if err != nil && !errors.Is(err, runtime.ErrMailboxFull) {
		self.Exit()
		return nil, err
	}
	return nil, nil
}
