package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/protocol"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/nodes/access/gameagent"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// accessPushClient 只注入 Gate 的写回结果，不复制 Gate 的队列或 socket 实现。
type accessPushClient struct {
	sendError error
	frames    chan []byte
	attempts  int
}

var _ gate.ClientSession = (*accessPushClient)(nil)

func (client *accessPushClient) Send(frame []byte) error {
	client.attempts++
	if client.sendError != nil {
		return client.sendError
	}
	select {
	case client.frames <- append([]byte(nil), frame...):
		return nil
	default:
		return errors.New("unexpected extra push")
	}
}

func (client *accessPushClient) Close() error {
	return nil
}

// TestAgentPushFailureIsLocal 保护一个真实 Agent 的出站失败不影响另一个 Agent，且旧 LoginID 不触发写回。
func TestAgentPushFailureIsLocal(t *testing.T) {
	for name, sendError := range map[string]error{
		"queue full": gate.ErrClientQueueFull,
		"closed":     gate.ErrClientSessionClosed,
	} {
		t.Run(name, func(t *testing.T) {
			registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
			if err != nil {
				t.Fatal(err)
			}
			failed := &accessPushClient{frames: make(chan []byte, 1)}
			healthy := &accessPushClient{frames: make(chan []byte, 2)}
			failedAgent := enteredMoleAgent(t, failed, registry, 1)
			healthyAgent := enteredMoleAgent(t, healthy, registry, 2)
			failed.sendError = sendError
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			main := &accessFixtureCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
				failedID, err := self.CreateService(ctx, failedAgent)
				if err != nil {
					return err
				}
				healthyID, err := self.CreateService(ctx, healthyAgent)
				if err != nil {
					return err
				}
				for revision := uint64(1); revision <= 2; revision++ {
					_, err := self.Call(ctx, failedID, 102, "current-login",
						map[string]any{"balance": int64(1), "revision": revision})
					// GSR 将未分类的 Gate 错误归一化，不将底层错误原文暴露给 Call 方。
					if !errors.Is(err, runtime.ErrHandlerFailed) {
						return fmt.Errorf("push failure: got %v want %v", err, runtime.ErrHandlerFailed)
					}
					if _, err := self.Call(ctx, healthyID, 102, "current-login",
						map[string]any{"balance": int64(1000) - int64(revision)*100, "revision": revision}); err != nil {
						return fmt.Errorf("healthy push: %w", err)
					}
				}
				_, err = self.Call(ctx, failedID, 102, "old-login",
					map[string]any{"balance": int64(0), "revision": uint64(99)})
				return err
			}}
			if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}}, main); err != nil {
				t.Fatal(err)
			}
			if failed.attempts != 2 || healthy.attempts != 2 || len(failed.frames) != 0 || len(healthy.frames) != 2 {
				t.Fatalf("failed attempts=%d frames=%d; healthy attempts=%d frames=%d", failed.attempts, len(failed.frames), healthy.attempts, len(healthy.frames))
			}
			for revision := uint64(1); revision <= 2; revision++ {
				var envelope commonv1.Envelope
				if err := proto.Unmarshal(<-healthy.frames, &envelope); err != nil {
					t.Fatal(err)
				}
				var push demopb.BalancePush
				if err := proto.Unmarshal(envelope.Payload, &push); err != nil || envelope.MsgId != uint32(demopb.MsgId_S2C_BALANCE_PUSH) || envelope.RequestId != 0 {
					t.Fatalf("invalid healthy push: %v %v", &envelope, err)
				}
				if push.GetSilver().GetRevision() != revision || push.GetSilver().GetBalance() != 1000-int64(revision)*100 {
					t.Fatalf("healthy balance: %v", &push)
				}
			}
		})
	}
}

// enteredMoleAgent 仅替代进入时的业务响应，通过实际协议建立路由，不越过组件私有字段。
func enteredMoleAgent(t *testing.T, client *accessPushClient, registry *protocol.Registry, userID uint64) *agent {
	t.Helper()
	agent := &agent{client: client, session: agentSession{userID: userID, loginID: "current-login"}}
	mole := gameagent.NewMole(gameagent.Dependencies{
		Identity:   agent.session.snapshot,
		AllowEnter: agent.allowEnter,
		Push:       agent.push,
		Call: func(_ gsr.ServiceContext, _ cluster.NodeID, name cluster.ServiceName, _ runtime.CommandID, _ ...any) (any, error) {
			if name == "mole" {
				return map[string]any{"entry_name": "entry"}, nil
			}
			snapshot := fixtureSnapshot()
			snapshot["silver"] = map[string]any{"balance": int64(1000)}
			return snapshot, nil
		},
	})
	agent.games = []gameagent.Component{mole}
	agent.mustBindHandlers(registry)
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(demopb.MsgId_C2S_ENTER), RequestId: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.HandleClientMessage(&exitContext{}, frame); err != nil {
		t.Fatal(err)
	}
	if !mole.Entered() || len(client.frames) != 1 {
		t.Fatal("未通过进入协议建立路由")
	}
	<-client.frames
	client.attempts = 0
	return agent
}
