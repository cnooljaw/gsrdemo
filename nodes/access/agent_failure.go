package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/runtime"
)

// 失败关闭辅助 Service 仅由本地计时唤醒，不发布远程名称或共享契约。
//
//rpc:service failurecloser private
const failureCloseCommand runtime.CommandID = 1

// failureCloser 独立等待错误回包的短发送窗口；Agent 已封闭后续业务，连接队列仍由 Gate 独占。
type failureCloser struct{ client gate.ClientSession }

var _ gsr.Service = (*failureCloser)(nil)

func (closer *failureCloser) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(200*time.Millisecond, failureCloseCommand)
	return err
}

func (closer *failureCloser) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != failureCloseCommand || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	self.Exit()
	return nil, nil
}

func (closer *failureCloser) OnStop(_ context.Context) error {
	return closer.client.Close()
}

// closeAfterFailure 复用同一个发送窗口处理认证/登录失败和客户端请求中的非法后端回包。
func (agent *agent) closeAfterFailure(self gsr.ServiceContext) {
	agent.closing = true
	ctx, cancel := context.WithTimeout(context.Background(), agentCallTimeout)
	defer cancel()
	if _, err := self.CreateService(ctx, &failureCloser{client: agent.client}); err != nil {
		// 没有辅助 Service 时不会再等 Gate 的断线通知；先尽力清理，再直接退出。
		agent.disconnect(self)
		self.Exit()
		return
	}
}
