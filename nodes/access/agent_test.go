package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

type testClientSession struct {
	closeCount int
	closeError error
}

var _ gate.ClientSession = (*testClientSession)(nil)

func (client *testClientSession) Send(_ []byte) error {
	return nil
}

func (client *testClientSession) Close() error {
	client.closeCount++
	return client.closeError
}

// 测试只替代 Exit 能力，不保存真实 Handler 上下文。
type exitContext struct {
	gsr.CommandContext
	exited bool
}

var _ gsr.CommandContext = (*exitContext)(nil)

func (ctx *exitContext) Exit() {
	ctx.exited = true
}

// TestAgentDisconnectExitsAndStopClosesClient 保护未知命令不退出、断线通知退出、停止关闭当前连接且保留关闭错误。
func TestAgentDisconnectExitsAndStopClosesClient(t *testing.T) {
	registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
	if err != nil {
		t.Fatal(err)
	}
	closeError := errors.New("close failed")
	client := &testClientSession{closeError: closeError}
	agent := newAgent(client, registry)
	self := &exitContext{}
	if _, err := agent.Handle(self, runtime.CommandID(0)); !errors.Is(err, runtime.ErrUnknownCommand) || self.exited {
		t.Fatalf("unknown command: exited=%v, err=%v", self.exited, err)
	}
	if _, err := agent.Handle(self, gate.ClientDisconnectedCommand); err != nil || !self.exited {
		t.Fatalf("disconnect: exited=%v, err=%v", self.exited, err)
	}
	if err := agent.OnStop(context.Background()); !errors.Is(err, closeError) || client.closeCount != 1 {
		t.Fatalf("stop: closed=%d, err=%v", client.closeCount, err)
	}
}
