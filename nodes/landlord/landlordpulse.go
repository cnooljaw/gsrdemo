package main

import (
	"context"
	"errors"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

// 计时命令仅接收本地 Timer，不生成共享契约。
//
//rpc:service landlordpulse private
const landlordPulseTickCommand runtime.CommandID = 1

// landlordPulse 只负责唤醒本桌；先续订自身 Timer，再非阻塞 Send，满队列不切断后续超时推进。
type landlordPulse struct{ target runtime.ServiceID }

var _ gsr.Service = (*landlordPulse)(nil)

func (pulse *landlordPulse) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(250*time.Millisecond, landlordPulseTickCommand)
	return err
}

func (pulse *landlordPulse) OnStop(context.Context) error {
	return nil
}

func (pulse *landlordPulse) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != landlordPulseTickCommand || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	if _, err := self.After(250*time.Millisecond, landlordPulseTickCommand); err != nil {
		return nil, err
	}
	err := self.Send(pulse.target, roomTickCommand)
	if err != nil && !errors.Is(err, runtime.ErrMailboxFull) {
		self.Exit()
		return nil, err
	}
	return nil, nil
}
