package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/cnooljaw/gsr/runtime"
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
func (service *accessBackendFixture) OnStop(context.Context) error                      { return nil }
func (service *accessBackendFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	switch service.role {
	case "login":
		if args[0] != "alice" || args[1] != "password" {
			return nil, errors.New("invalid credentials")
		}
		return uint64(7), nil
	case "online":
		switch command {
		case 1:
			if service.failBind {
				return nil, errors.New("online unavailable")
			}
			previous := service.current
			service.current = map[string]any{"login_id": args[1], "access_node": args[2], "agent_name": args[3]}
			if previous == nil {
				return nil, nil
			}
			return previous, nil
		case 3:
			if service.current != nil && service.current["login_id"] == args[1] {
				service.current = nil
				return true, nil
			}
			return false, nil
		case 98:
			return nil, self.Cluster().Send(cluster.NodeID(service.current["access_node"].(string)), cluster.ServiceName(service.current["agent_name"].(string)), 100, args...)
		case 97:
			service.failBind = args[0].(bool)
			return nil, nil
		case 99:
			if service.current == nil {
				return nil, nil
			}
			return service.current, nil
		}
	case "mole":
		return map[string]any{"entry_name": "entry"}, nil
	case "entry":
		if command == 3 {
			return map[string]any{"operation_id": args[1], "status": "committed", "reason": ""}, nil
		}
		snapshot := fixtureSnapshot()
		snapshot["silver"] = map[string]any{"balance": int64(1000), "available": int64(1000), "revision": uint64(1)}
		return snapshot, nil
	case "attack":
		switch command {
		case 2:
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
		case 3:
			snapshot := fixtureSnapshot()
			snapshot["targets"].([]any)[0].(map[string]any)["spawn_seq"] = uint64(10)
			return snapshot, nil
		case 4:
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
	for _, role := range []string{"login", "online", "mole", "entry", "attack"} {
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
	for {
		envelope := readAccessEnvelope(t, client)
		if envelope.RequestId != 0 {
			return envelope
		}
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
		value, err = self.Cluster().Call(ctx, "backend", "online", command, args...)
		return err
	}}
	err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"backend": address}}}, main)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// TestAccessBusinessLoginHitAndPush 保护错误登录不绑定、未登录不进游戏、服务端填写身份及旧 LoginID 推送被忽略。
func TestAccessBusinessLoginHitAndPush(t *testing.T) {
	gateAddress, backend, hits := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	for _, test := range []struct {
		id      demopb.MsgId
		request proto.Message
	}{
		{demopb.MsgId_C2S_HIT, &demopb.HitRequest{}},
		{demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "wrong"}},
	} {
		if response := accessRequest(t, client, test.id, test.request); response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
			t.Fatal(response)
		}
	}
	if current := accessFixtureRPC(t, backend, 99); current != nil {
		t.Fatalf("错误登录绑定了位置: %v", current)
	}
	if response := accessRequest(t, client, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "password"}); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(response)
	}
	location := accessFixtureRPC(t, backend, 99).(map[string]any)
	if response := accessRequest(t, client, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "password"}); response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatal("同连接重复登录应拒绝", response)
	}
	if current := accessFixtureRPC(t, backend, 99).(map[string]any); current["login_id"] != location["login_id"] {
		t.Fatal("重复登录改写了当前身份")
	}
	if response := accessRequest(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatal(response)
	}
	for range 2 {
		response := accessRequest(t, client, demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: "same-action", SpawnSeq: 9, Hole: 2})
		if response.MsgId != uint32(demopb.MsgId_S2C_HIT) {
			t.Fatal(response)
		}
		args := <-hits
		if args[0] != uint64(7) || args[1] != location["login_id"] || args[2] != "same-action" || args[3] != uint64(9) || args[4] != int64(2) {
			t.Fatalf("hit args=%v", args)
		}
	}
	rejected := accessRequest(t, client, demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: "rejected-action", SpawnSeq: 9, Hole: 2})
	var rejectedHit demopb.HitResponse
	if err := proto.Unmarshal(rejected.Payload, &rejectedHit); err != nil || rejected.MsgId != uint32(demopb.MsgId_S2C_HIT) || rejectedHit.GetHit().GetStatus() != "rejected" || rejectedHit.Silver != nil {
		t.Fatalf("拒绝重试不能伪造余额: %v %v", &rejectedHit, err)
	}
	accessFixtureRPC(t, backend, 98, "old-login", "balance_correction", map[string]any{"balance": int64(1), "revision": uint64(99)})
	accessFixtureRPC(t, backend, 98, location["login_id"], "balance_correction", map[string]any{"balance": int64(900), "revision": uint64(3)})
	response := readAccessEnvelope(t, client)
	var push demopb.BalancePush
	if response.RequestId != 0 || response.MsgId != uint32(demopb.MsgId_S2C_BALANCE_PUSH) {
		t.Fatal(response)
	}
	if err := proto.Unmarshal(response.Payload, &push); err != nil || push.GetSilver().GetBalance() != 900 {
		t.Fatalf("push=%v err=%v", &push, err)
	}
}

// TestAccessReplacedConnectionCannotUnbindNewLogin 保护重复登录后旧 Agent 退出及迟到断线不会清理新 LoginID。
func TestAccessReplacedConnectionCannotUnbindNewLogin(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	old := dialAccess(t, gateAddress)
	first := accessRequest(t, old, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "password"})
	if first.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(first)
	}
	previous := accessFixtureRPC(t, backend, 99).(map[string]any)
	current := dialAccess(t, gateAddress)
	second := accessRequest(t, current, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "password"})
	if second.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(second)
	}
	_ = old.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := old.ReadMessage(); err == nil {
		t.Fatal("旧连接未关闭")
	}
	now := accessFixtureRPC(t, backend, 99).(map[string]any)
	if now["login_id"] == previous["login_id"] || now["access_node"] != "access-01" {
		t.Fatal(fmt.Sprint(now))
	}
	_ = old.Close()
	if response := accessRequest(t, current, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatal(response)
	}
}

// TestAccessOnlineFailureDoesNotOpenGame 保护认证成功但 Online 绑定失败时关闭连接，不留下已认证可游戏的 Agent。
func TestAccessOnlineFailureDoesNotOpenGame(t *testing.T) {
	gateAddress, backend, _ := startAccessBusiness(t)
	accessFixtureRPC(t, backend, 97, true)
	client := dialAccess(t, gateAddress)
	payload, err := proto.Marshal(&demopb.LoginRequest{Username: "alice", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: uint32(demopb.MsgId_C2S_LOGIN), RequestId: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetWriteDeadline(time.Now().Add(time.Second))
	if err := client.WriteMessage(websocket.BinaryMessage, client.authenticatedFrame(frame)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, contents, readErr := client.ReadMessage()
	if readErr == nil {
		var envelope commonv1.Envelope
		if err := proto.Unmarshal(contents, &envelope); err != nil || envelope.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
			t.Fatalf("Online 失败不应返回登录成功: %v %v", &envelope, err)
		}
		if _, _, err := client.ReadMessage(); err == nil {
			t.Fatal("失败绑定后连接仍开放")
		}
	}
	if current := accessFixtureRPC(t, backend, 99); current != nil {
		t.Fatal(current)
	}
	accessFixtureRPC(t, backend, 97, false)
	healthy := dialAccess(t, gateAddress)
	if response := accessRequest(t, healthy, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "password"}); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal("恢复后新连接不能登录", response)
	}
}

// TestAccessOnlyKnownTargetRefusalCanContinue 保护只有明确的目标失效才返回可继续响应，超时、繁忙和错误类别不被当成未扣款。
func TestAccessOnlyKnownTargetRefusalCanContinue(t *testing.T) {
	gateAddress, _, hits := startAccessBusiness(t)
	client := dialAccess(t, gateAddress)
	if response := accessRequest(t, client, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{Username: "alice", Password: "password"}); response.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal(response)
	}
	if response := accessRequest(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(demopb.MsgId_S2C_ENTER) {
		t.Fatal(response)
	}
	for _, action := range []string{"target-gone", "timeout", "busy", "wrong-class"} {
		response := accessRequest(t, client, demopb.MsgId_C2S_HIT, &demopb.HitRequest{ActionId: action, SpawnSeq: 9, Hole: 2})
		<-hits
		if action != "target-gone" {
			if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
				t.Fatalf("%s 不能变为明确拒绝: %v", action, response)
			}
			continue
		}
		var hit demopb.HitResponse
		if err := proto.Unmarshal(response.Payload, &hit); err != nil || response.MsgId != uint32(demopb.MsgId_S2C_HIT) || hit.Rejection != "target_unavailable" || hit.Hit != nil || hit.Silver != nil {
			t.Fatalf("目标失效不应有奖励或余额: %v %v", &hit, err)
		}
	}
}

// TestAccessCLITargetRefusalAndUnknownResult 保护实际 CLI 在目标失效后换目标继续，而超时/繁忙时只发一次命中并停止。
func TestAccessCLITargetRefusalAndUnknownResult(t *testing.T) {
	if os.Getenv("GSRDEMO_ACCESS_PROCESSES") != "1" {
		t.Skip("build typeclient and set GSRDEMO_ACCESS_PROCESSES=1")
	}
	for _, scenario := range []string{"target-first", "timeout-first", "busy-first"} {
		t.Run(scenario, func(t *testing.T) {
			gateAddress, _, hits := startAccessBusiness(t, scenario)
			clientPath, err := filepath.Abs("../../typeclient/dist/index.js")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+gateAddress+"/ws", "--username", "alice", "--hits", "1")
			command.Stdin = strings.NewReader("password\n")
			output, runErr := command.CombinedOutput()
			if scenario != "target-first" {
				if runErr == nil || strings.Contains(string(output), `"event":"miss"`) || len(hits) != 1 {
					t.Fatalf("未知结果不能继续: %v %s hits=%d", runErr, output, len(hits))
				}
				return
			}
			if runErr != nil || !strings.Contains(string(output), `"event":"miss"`) || !strings.Contains(string(output), `"event":"settlement"`) || len(hits) != 2 {
				t.Fatalf("目标拒绝后未继续: %v %s hits=%d", runErr, output, len(hits))
			}
			first, second := <-hits, <-hits
			if first[2] == second[2] || first[3] != uint64(9) || second[3] != uint64(10) {
				t.Fatalf("应刷新目标、使用新动作: %v %v", first, second)
			}
		})
	}
}
