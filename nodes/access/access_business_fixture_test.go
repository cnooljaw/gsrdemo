// access 业务协作测试的 TCP 后端替身和客户端调用夹具；不替代被测 Agent。
package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
	authrpc "github.com/cnooljaw/gsrdemo/common/rpc/auth"
	molerpc "github.com/cnooljaw/gsrdemo/common/rpc/mole"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleattack"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	tokenrpc "github.com/cnooljaw/gsrdemo/common/rpc/token"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// accessBackendFixture 用真实 TCP 接口替代业务节点，所有目录记录由 online 实例独占。
type accessBackendFixture struct {
	role     string
	current  map[string]any
	hits     chan<- []any
	failBind bool
	scenario string
	attempts int
}

var _ gsr.Service = (*accessBackendFixture)(nil)

func (service *accessBackendFixture) OnStart(context.Context, gsr.ServiceContext) error { return nil }

func (service *accessBackendFixture) OnStop(context.Context) error { return nil }

func (service *accessBackendFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	switch service.role {
	case authrpc.ServiceName:
		if args[0] != "alice" || args[1] != "password" {
			return nil, errors.New("invalid credentials")
		}
		return map[string]any{"user_id": uint64(7), "access_token": strings.Repeat("A", 43), "expires_at_ms": time.Now().Add(time.Hour).UnixMilli()}, nil
	case tokenrpc.ServiceName:
		if command != tokenrpc.VerifyCommand || args[0] != strings.Repeat("A", 43) {
			return nil, errors.New("invalid token")
		}
		return uint64(7), nil
	case onlinerpc.ServiceName:
		switch command {
		case onlinerpc.BindCommand:
			if service.failBind {
				return nil, errors.New("online unavailable")
			}
			previous := service.current
			service.current = map[string]any{"login_id": args[1], "access_node": args[2], "agent_name": args[3]}
			if previous == nil {
				return nil, nil
			}
			return previous, nil
		case onlinerpc.UnbindCommand:
			if service.current != nil && service.current["login_id"] == args[1] {
				service.current = nil
				return true, nil
			}
			return false, nil
		case 98:
			return nil, self.Cluster().Send(cluster.NodeID(service.current["access_node"].(string)), cluster.ServiceName(service.current["agent_name"].(string)), runtime.CommandID(args[0].(uint64)), args[1:]...)
		case 97:
			service.failBind = args[0].(bool)
			return nil, nil
		case 99:
			if service.current == nil {
				return nil, nil
			}
			return service.current, nil
		}
	case molerpc.ServiceName:
		return map[string]any{"entry_name": "entry"}, nil
	case "entry":
		if command == 3 {
			return nil, runtime.ErrUnknownCommand
		}
		snapshot := fixtureSnapshot()
		snapshot["silver"] = map[string]any{"balance": int64(1000), "available": int64(1000), "revision": uint64(1)}
		return snapshot, nil
	case "attack":
		switch command {
		case moleattack.AttackHitCommand:
			service.hits <- append([]any(nil), args...)
			service.attempts++
			if service.attempts == 1 {
				switch service.scenario {
				case "target-first":
					return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "mole.target_unavailable", "target gone")
				case "timeout-first":
					return nil, runtime.ErrCallTimeout
				case "busy-first":
					return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "mole.busy", "busy")
				}
			}
			switch args[2] {
			case "target-gone":
				return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "mole.target_unavailable", "target gone")
			case "timeout":
				return nil, runtime.ErrCallTimeout
			case "busy":
				return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "mole.busy", "busy")
			case "wrong-class":
				return nil, runtime.NewStatusError(runtime.ErrorClassRuntime, "mole.target_unavailable", "wrong class")
			}
			if args[2] == "rejected-action" {
				return map[string]any{"hit_id": "hit-rejected", "action_id": args[2], "status": "rejected"}, nil
			}
			return map[string]any{"hit_id": "hit-1", "action_id": args[2], "spawn_seq": args[3], "hole": args[4], "status": "accepted", "debit": int64(100), "credit": int64(0), "balance": int64(900), "available": int64(900), "revision": uint64(2)}, nil
		case moleattack.AttackSnapshotCommand:
			snapshot := fixtureSnapshot()
			snapshot["targets"].([]any)[0].(map[string]any)["spawn_seq"] = uint64(10)
			return snapshot, nil
		case moleattack.AttackLeaveCommand:
			return true, nil
		}
	}
	return nil, runtime.ErrUnknownCommand
}

func fixtureSnapshot() map[string]any {
	return map[string]any{"attack_name": "attack", "seat": int64(0), "server_ms": time.Now().UnixMilli(), "targets": []any{map[string]any{"spawn_seq": uint64(9), "hole": int64(0), "deadline_ms": time.Now().Add(10 * time.Second).UnixMilli(), "claimed": false}}, "members": []any{}, "hits": []any{}}
}

type accessFixtureMain struct {
	ready    chan<- string
	hits     chan<- []any
	scenario string
}

var _ gsr.Service = (*accessFixtureMain)(nil)

func (service *accessFixtureMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for _, role := range []string{authrpc.ServiceName, tokenrpc.ServiceName, onlinerpc.ServiceName, molerpc.ServiceName, "entry", "attack"} {
		id, err := self.CreateService(ctx, &accessBackendFixture{role: role, hits: service.hits, scenario: service.scenario})
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(cluster.ServiceName(role), id); err != nil {
			return err
		}
	}
	if err := self.Node().OpenCluster("backend"); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(self.Node().ClusterListenAddress())
	if err != nil {
		return err
	}
	service.ready <- net.JoinHostPort("127.0.0.1", port)
	self.Exit()
	return nil
}

func (*accessFixtureMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (*accessFixtureMain) OnStop(context.Context) error { return nil }

func startAccessBusiness(t *testing.T, scenarios ...string) (string, string, <-chan []any) {
	t.Helper()
	accessAddress, gateAddress := testAddresses(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready, hits := make(chan string, 1), make(chan []any, 8)
	scenario := ""
	if len(scenarios) != 0 {
		scenario = scenarios[0]
	}
	backendDone := make(chan error, 1)
	backendLogs := t.TempDir()
	go func() {
		backendDone <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: backendLogs}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"backend": "127.0.0.1:0", "access-01": accessAddress}}}, &accessFixtureMain{ready: ready, hits: hits, scenario: scenario})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-backendDone; err != nil {
			t.Error(err)
		}
	})
	var backend string
	select {
	case backend = <-ready:
	case err := <-backendDone:
		backendDone <- err
		t.Fatalf("backend: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("backend startup timed out")
	}
	routes := map[cluster.NodeID]string{"access-01": accessAddress, "login": backend, "online": backend, "mole": backend}
	accessReady, accessDone := make(chan error, 1), make(chan error, 1)
	accessCtx, stopAccess := context.WithCancel(context.Background())
	accessLogs := t.TempDir()
	go func() {
		accessDone <- gsr.Run(accessCtx, gsr.Config{Logging: logging.Config{Directory: accessLogs}, Cluster: gsr.ClusterConfig{Nodes: routes}}, &readyMain{Service: NewMain("access-01", gateOptions{Address: gateAddress, MaxConnections: 8, MaxFrameBytes: 4096, MaxOutboundQueueBytes: 65536}), ready: accessReady})
	}()
	t.Cleanup(func() {
		stopAccess()
		if err := <-accessDone; err != nil {
			t.Error(err)
		}
	})
	if err := <-accessReady; err != nil {
		t.Fatal(err)
	}
	return gateAddress, backend, hits
}

func accessRequest(t *testing.T, client *accessClient, id demopb.MsgId, request proto.Message) *commonv1.Envelope {
	t.Helper()
	response, _ := accessRequestAndPushes(t, client, id, request)
	return response
}

// accessRequestAndPushes 保留请求等待期间交错到达的推送，避免故障用例漏掉早于响应的余额校正。
func accessRequestAndPushes(t *testing.T, client *accessClient, id demopb.MsgId, request proto.Message) (*commonv1.Envelope, []*commonv1.Envelope) {
	t.Helper()
	payload, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(id), RequestId: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(frame)); err != nil {
		t.Fatal(err)
	}
	var pushes []*commonv1.Envelope
	for {
		envelope := readAccessEnvelope(t, client)
		if envelope.RequestId != 0 {
			return envelope, pushes
		}
		pushes = append(pushes, envelope)
	}
}

func readAccessEnvelope(t *testing.T, client *accessClient) *commonv1.Envelope {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, contents, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	envelope := &commonv1.Envelope{}
	if err := proto.Unmarshal(contents, envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

type accessFixtureCaller struct {
	work   func(gsr.ServiceContext) error
	cancel context.CancelFunc
}

var _ gsr.Service = (*accessFixtureCaller)(nil)

func (service *accessFixtureCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	err := service.work(self)
	service.cancel()
	self.Exit()
	return err
}

func (*accessFixtureCaller) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (*accessFixtureCaller) OnStop(context.Context) error { return nil }

func accessFixtureRPC(t *testing.T, address string, command runtime.CommandID, args ...any) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var value any
	main := &accessFixtureCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		var err error
		value, err = self.Cluster().Call(ctx, "backend", onlinerpc.ServiceName, command, args...)
		return err
	}}
	err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"backend": address}}}, main)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
