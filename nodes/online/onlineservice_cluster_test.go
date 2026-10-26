package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

// onlineObservedMain 保留真实 MainService 接线，只报告启动地址。
type onlineObservedMain struct {
	service gsr.Service
	ready   chan<- string
}

var _ gsr.Service = (*onlineObservedMain)(nil)

func (service *onlineObservedMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
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

func (service *onlineObservedMain) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.service.Handle(self, command, args...)
}

func (service *onlineObservedMain) OnStop(ctx context.Context) error {
	return service.service.OnStop(ctx)
}

func startOnline(t *testing.T) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	main := &onlineObservedMain{service: NewMain("online"), ready: ready}
	directory := t.TempDir()
	go func() {
		done <- gsr.Run(ctx, gsr.Config{
			Logging: logging.Config{Directory: directory},
			Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"online": "127.0.0.1:0"}},
		}, main)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("online shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("online shutdown timed out")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case address := <-ready:
		return address, stop
	case err := <-done:
		done <- err
		t.Fatalf("online startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("online startup timed out")
	}
	return "", stop
}

// onlineAccessCaller 是测试用接入节点，所有调用都由自己的 Service 回调发起。
type onlineAccessCaller struct {
	node   cluster.NodeID
	work   func(gsr.ServiceContext) error
	cancel context.CancelFunc
}

var _ gsr.Service = (*onlineAccessCaller)(nil)

func (service *onlineAccessCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	if err := self.Node().OpenCluster(service.node); err != nil {
		return err
	}
	if err := service.work(self); err != nil {
		return err
	}
	service.cancel()
	self.Exit()
	return nil
}

func (service *onlineAccessCaller) Handle(_ gsr.CommandContext, _ runtime.CommandID, _ ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *onlineAccessCaller) OnStop(_ context.Context) error {
	return nil
}

func runOnlineAccess(ctx context.Context, address, node, directory string, work func(gsr.ServiceContext) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: directory},
		Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{
			"online": address, cluster.NodeID(node): "127.0.0.1:0",
		}},
	}, &onlineAccessCaller{node: cluster.NodeID(node), work: work, cancel: cancel})
}

func callOnline(self gsr.ServiceContext, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return self.Cluster().Call(ctx, "online", onlineServiceName, command, args...)
}

func expectOnlineCall(self gsr.ServiceContext, command runtime.CommandID, want any, args ...any) error {
	value, err := callOnline(self, command, args...)
	if err != nil || !reflect.DeepEqual(value, want) {
		return fmt.Errorf("command %d: value=%v want=%v err=%v", command, value, want, err)
	}
	return nil
}

func waitOnlineStep(ctx context.Context, ready <-chan struct{}) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestOnlineTwoAccessNodesReplaceAndDisconnect 保护两个同时运行的接入节点使用同名 Agent 时，跨节点重连和旧连接断线不误清理。
func TestOnlineTwoAccessNodesReplaceAndDisconnect(t *testing.T) {
	address, _ := startOnline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldBound, newBound, oldDisconnected := make(chan struct{}), make(chan struct{}), make(chan struct{})
	works := []func(gsr.ServiceContext) error{
		func(self gsr.ServiceContext) error {
			if err := expectOnlineCall(self, bindCommand, nil, uint64(7), "old", "access-01", "agent-1"); err != nil {
				return err
			}
			close(oldBound)
			if err := waitOnlineStep(ctx, newBound); err != nil {
				return err
			}
			if err := expectOnlineCall(self, unbindCommand, false, uint64(7), "old"); err != nil {
				return err
			}
			if err := expectOnlineCall(self, lookupCommand, wantLocation("new", "access-02", "agent-1"), uint64(7)); err != nil {
				return err
			}
			close(oldDisconnected)
			return nil
		},
		func(self gsr.ServiceContext) error {
			if err := waitOnlineStep(ctx, oldBound); err != nil {
				return err
			}
			if err := expectOnlineCall(self, bindCommand, wantLocation("old", "access-01", "agent-1"), uint64(7), "new", "access-02", "agent-1"); err != nil {
				return err
			}
			if err := expectOnlineCall(self, bindCommand, nil, uint64(8), "other", "access-02", "agent-2"); err != nil {
				return err
			}
			close(newBound)
			if err := waitOnlineStep(ctx, oldDisconnected); err != nil {
				return err
			}
			if err := expectOnlineCall(self, unbindCommand, true, uint64(7), "new"); err != nil {
				return err
			}
			if err := expectOnlineCall(self, lookupCommand, nil, uint64(7)); err != nil {
				return err
			}
			return expectOnlineCall(self, lookupCommand, wantLocation("other", "access-02", "agent-2"), uint64(8))
		},
	}
	done := make(chan error, len(works))
	for index, work := range works {
		node, directory := fmt.Sprintf("access-%02d", index+1), t.TempDir()
		go func() { done <- runOnlineAccess(ctx, address, node, directory, work) }()
	}
	for range works {
		if err := <-done; err != nil {
			cancel()
			t.Errorf("access node: %v", err)
		}
	}
}

// TestOnlineConcurrentBindsHaveOneWinner 保护两个接入节点同时绑定同一用户时，只有一个当前记录，落败连接不能清除胜出连接。
func TestOnlineConcurrentBindsHaveOneWinner(t *testing.T) {
	address, _ := startOnline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready, start := make(chan struct{}, 2), make(chan struct{})
	type outcome struct {
		node     string
		previous any
		err      error
	}
	results := make(chan outcome, 2)
	for _, node := range []string{"access-01", "access-02"} {
		directory := t.TempDir()
		go func() {
			var previous any
			err := runOnlineAccess(ctx, address, node, directory, func(self gsr.ServiceContext) error {
				ready <- struct{}{}
				if err := waitOnlineStep(ctx, start); err != nil {
					return err
				}
				var err error
				previous, err = callOnline(self, bindCommand, uint64(7), "login-"+node, node, "agent-1")
				return err
			})
			results <- outcome{node: node, previous: previous, err: err}
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("access nodes did not reach bind barrier")
		}
	}
	close(start)
	var winner, loser string
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.previous == nil {
			loser = result.node
			continue
		}
		winner = result.node
		other := "access-01"
		if result.node == other {
			other = "access-02"
		}
		if !reflect.DeepEqual(result.previous, wantLocation("login-"+other, other, "agent-1")) {
			t.Fatalf("unexpected displaced location: %v", result.previous)
		}
	}
	if winner == "" || loser == "" || winner == loser {
		t.Fatalf("expected one winner and one loser: %q %q", winner, loser)
	}
	err := runOnlineAccess(ctx, address, "access-check", t.TempDir(), func(self gsr.ServiceContext) error {
		if err := expectOnlineCall(self, unbindCommand, false, uint64(7), "login-"+loser); err != nil {
			return err
		}
		return expectOnlineCall(self, lookupCommand, wantLocation("login-"+winner, winner, "agent-1"), uint64(7))
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestOnlineClusterContractAndRestart 保护规范参数、稳定错误、当前绑定重复提交及重启后空目录，不依赖进程内 Go 类型穿透网络。
func TestOnlineClusterContractAndRestart(t *testing.T) {
	address, stop := startOnline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runOnlineAccess(ctx, address, "access-01", t.TempDir(), func(self gsr.ServiceContext) error {
		if err := expectOnlineCall(self, lookupCommand, nil, uint64(7)); err != nil {
			return err
		}
		if err := expectOnlineCall(self, bindCommand, nil, uint64(7), "login-1", "access-01", "agent-1"); err != nil {
			return err
		}
		if err := expectOnlineCall(self, bindCommand, nil, uint64(7), "login-1", "access-01", "agent-1"); err != nil {
			return err
		}
		for _, entry := range []struct {
			command runtime.CommandID
			args    []any
			want    error
		}{
			{bindCommand, []any{uint64(7), "login-1", "access-02", "agent-1"}, errLoginIDConflict},
			{bindCommand, []any{uint64(7), "", "access-02", "agent-1"}, errInvalidOnlineInput},
			{lookupCommand, []any{uint64(0)}, errInvalidOnlineInput},
			{unbindCommand, []any{uint64(7), ""}, errInvalidOnlineInput},
			{lookupCommand, []any{int64(7)}, runtime.ErrInvalidCommandArguments},
			{bindCommand, []any{uint64(7)}, runtime.ErrInvalidCommandArguments},
			{999, nil, runtime.ErrUnknownCommand},
		} {
			if _, err := callOnline(self, entry.command, entry.args...); !errors.Is(err, entry.want) {
				return fmt.Errorf("command %d: err=%v want=%v", entry.command, err, entry.want)
			}
		}
		return expectOnlineCall(self, lookupCommand, wantLocation("login-1", "access-01", "agent-1"), uint64(7))
	})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	address, _ = startOnline(t)
	if err := runOnlineAccess(ctx, address, "access-02", t.TempDir(), func(self gsr.ServiceContext) error {
		return expectOnlineCall(self, lookupCommand, nil, uint64(7))
	}); err != nil {
		t.Fatal(err)
	}
}
