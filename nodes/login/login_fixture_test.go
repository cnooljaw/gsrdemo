// login 节点的 Runtime/TCP 调用夹具，供认证与 Token 协作用例复用。
package main

import (
	"context"
	"database/sql"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	authrpc "github.com/cnooljaw/gsrdemo/common/rpc/auth"
)

// observedMain 只观察真实 MainService 的启动结果，保留实际创建和命名流程。
type observedMain struct {
	service gsr.Service
	ready   chan<- string
}

var _ gsr.Service = (*observedMain)(nil)

func (service *observedMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if err := service.service.OnStart(ctx, self); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(self.Node().ClusterListenAddress())
	if err != nil {
		return err
	}
	service.ready <- net.JoinHostPort("127.0.0.1", port)
	return nil
}

func (service *observedMain) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.service.Handle(self, command, args...)
}

func (service *observedMain) OnStop(ctx context.Context) error {
	return service.service.OnStop(ctx)
}

func startLogin(t *testing.T, db *sql.DB) string {
	t.Helper()
	return startLoginWithStore(t, db, tokenStore{client: startTokenRedis(t), prefix: "test:login"})
}

func startLoginWithStore(t *testing.T, db *sql.DB, store tokenStore) string {
	t.Helper()
	address, _ := startLoginNode(t, db, store)
	return address
}

func startLoginNode(t *testing.T, db *sql.DB, store tokenStore) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	main := &observedMain{service: NewMain("login", db, store, time.Hour), ready: ready}
	directory := t.TempDir()
	go func() {
		done <- gsr.Run(ctx, gsr.Config{
			Logging: logging.Config{Directory: directory},
			Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"login": "127.0.0.1:0"}},
		}, main)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("login shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("login shutdown timed out")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case address := <-ready:
		return address, stop
	case err := <-done:
		// 留回结果供 cleanup 统一回收。
		done <- err
		t.Fatalf("login startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("login startup timed out")
	}
	return "", stop
}

// loginCaller 用普通 Service 身份发起真实 Cluster 请求，不使用 Runtime 注入捷径。
type loginCaller struct {
	call   func(gsr.ServiceContext)
	cancel context.CancelFunc
}

var _ gsr.Service = (*loginCaller)(nil)

func (service *loginCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	service.call(self)
	service.cancel()
	self.Exit()
	return nil
}

func (service *loginCaller) Handle(_ gsr.CommandContext, _ runtime.CommandID, _ ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *loginCaller) OnStop(_ context.Context) error {
	return nil
}

func callLogin(t *testing.T, address string, args ...any) (any, error) {
	t.Helper()
	return callLoginCommand(t, address, authrpc.ServiceName, authrpc.AuthenticateCommand, args...)
}

func callLoginCommand(t *testing.T, address, name string, command runtime.CommandID, args ...any) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var value any
	var callErr error
	caller := &loginCaller{cancel: cancel, call: func(self gsr.ServiceContext) {
		wait, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		value, callErr = self.Cluster().Call(wait, "login", cluster.ServiceName(name), command, args...)
	}}
	if err := gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: t.TempDir()},
		Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"login": address}},
	}, caller); err != nil {
		t.Fatal(err)
	}
	return value, callErr
}

func validAuthResult(value any, uid uint64) bool {
	result, ok := value.(map[string]any)
	if !ok || result["user_id"] != uid {
		return false
	}
	token, ok := result["access_token"].(string)
	expires, valid := result["expires_at_ms"].(int64)
	return ok && validToken(token) && valid && expires > time.Now().UnixMilli()
}
