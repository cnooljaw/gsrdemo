package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

type landlordTestMain struct {
	node     string
	services map[string]gsr.Service
	ready    chan string
}

var _ gsr.Service = (*landlordTestMain)(nil)

func (main *landlordTestMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	for name, service := range main.services {
		id, err := self.CreateService(ctx, service)
		if err != nil {
			return err
		}
		if err := self.Node().RegisterService(cluster.ServiceName(name), id); err != nil {
			return err
		}
	}
	if err := self.Node().OpenCluster(cluster.NodeID(main.node)); err != nil {
		return err
	}
	main.ready <- self.Node().ClusterListenAddress()
	self.Exit()
	return nil
}

func (main *landlordTestMain) OnStop(context.Context) error {
	return nil
}

func (main *landlordTestMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

type landlordTestBackend struct{ logins map[uint64]string }

var _ gsr.Service = (*landlordTestBackend)(nil)

func (backend *landlordTestBackend) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (backend *landlordTestBackend) OnStop(context.Context) error {
	return nil
}

func (backend *landlordTestBackend) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	uid := args[0].(uint64)
	switch cmd {
	case 1:
		backend.logins[uid] = args[1].(string)
		return nil, nil
	case 2:
		login := backend.logins[uid]
		if login == "" {
			login = "login"
		}
		return map[string]any{"login_id": login, "access_node": "access-01", "agent_name": "agent"}, nil
	case 4:
		return map[string]any{"user_id": uid, "amount": int64(10000), "version": uint64(1)}, nil
	}
	return nil, runtime.ErrUnknownCommand
}

func startLandlordTestNode(t *testing.T, routes map[cluster.NodeID]string, node string, services map[string]gsr.Service) {
	t.Helper()
	nodes := map[cluster.NodeID]string{}
	for k, v := range routes {
		nodes[k] = v
	}
	nodes[cluster.NodeID(node)] = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan string, 1)
	go func() {
		done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: nodes}}, &landlordTestMain{node, services, ready})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("node did not stop")
		}
	})
	select {
	case address := <-ready:
		routes[cluster.NodeID(node)] = address
	case err := <-done:
		done <- err
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("node did not start")
	}
}

type landlordTestCaller struct {
	ctx        context.Context
	cancel     context.CancelFunc
	node, name string
	cmd        runtime.CommandID
	args       []any
	value      any
	err        error
}

var _ gsr.Service = (*landlordTestCaller)(nil)

func (caller *landlordTestCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	caller.value, caller.err = self.Cluster().Call(caller.ctx, cluster.NodeID(caller.node), cluster.ServiceName(caller.name), caller.cmd, caller.args...)
	caller.cancel()
	self.Exit()
	return nil
}

func (caller *landlordTestCaller) OnStop(context.Context) error {
	return nil
}

func (caller *landlordTestCaller) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func landlordTestRPC(t *testing.T, routes map[cluster.NodeID]string, node, name string, cmd runtime.CommandID, args ...any) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	caller := &landlordTestCaller{ctx: ctx, cancel: cancel, node: node, name: name, cmd: cmd, args: args}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}, caller); err != nil {
		t.Fatal(err)
	}
	return caller.value, caller.err
}

func landlordTestValue(t *testing.T, routes map[cluster.NodeID]string, node, name string, cmd runtime.CommandID, args ...any) any {
	t.Helper()
	value, err := landlordTestRPC(t, routes, node, name, cmd, args...)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func joinLandlordTest(t *testing.T, routes map[cluster.NodeID]string, uid uint64, login string) map[string]any {
	t.Helper()
	landlordTestValue(t, routes, "online", "online", 1, uid, login)
	route := landlordTestValue(t, routes, "landlord", landlordName, resolveCommand, uid, login).(map[string]any)
	return landlordTestValue(t, routes, "landlord", route["room_name"].(string), roomJoinCommand, uid, login, "access-01", "agent", route["membership"]).(map[string]any)
}

// TestAttackRejoinAndPulse 保护随机凑桌、叫抢中的原手牌重连、旧身份清理隔离及真实 Pulse 超时推进，其他桌仍可准备。
func TestAttackRejoinAndPulse(t *testing.T) {
	routes := map[cluster.NodeID]string{}
	startLandlordTestNode(t, routes, "online", map[string]gsr.Service{"online": &landlordTestBackend{logins: map[uint64]string{}}})
	reads := map[string]gsr.Service{}
	for _, name := range []string{"settlement-read"} {
		reads[name] = &landlordTestBackend{}
	}
	startLandlordTestNode(t, routes, "settlement-02", reads)
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{landlordName: newLandlordService(landlordJournal{}, "settlement-02")})
	var first map[string]any
	name := ""
	for uid := uint64(1); uid <= 3; uid++ {
		view := joinLandlordTest(t, routes, uid, "login")
		if name == "" {
			name = view["room_name"].(string)
		}
		if name != view["room_name"] {
			t.Fatal("未凑到同桌")
		}
		first = landlordTestValue(t, routes, "landlord", name, roomReadyCommand, uid, "login").(map[string]any)
	}
	if first["phase"] != "calling" || len(first["bottom"].([]any)) != 0 {
		t.Fatal("未进入叫地主或提前泄露底牌")
	}
	actorUID := first["players"].([]any)[int(first["turn_seat"].(int64))].(map[string]any)["user_id"].(uint64)
	if _, err := landlordTestRPC(t, routes, "landlord", name, roomRobCommand, actorUID, "login", first["turn"], true); err == nil {
		t.Fatal("叫地主阶段接受了抢地主")
	}
	initialTurn := first["turn"].(uint64)
	initialSeat := first["turn_seat"].(int64)
	own := landlordTestValue(t, routes, "landlord", name, roomSnapshotCommand, uint64(1), "login").(map[string]any)
	rejoined := joinLandlordTest(t, routes, 1, "new-login")
	if rejoined["room_name"] != name || !reflect.DeepEqual(own["hand"], rejoined["hand"]) {
		t.Fatal("重连未保留原桌和手牌")
	}
	if _, err := landlordTestRPC(t, routes, "landlord", name, roomLeaveCommand, uint64(1), "login"); err == nil {
		t.Fatal("旧身份仍可离开新成员")
	}
	second := joinLandlordTest(t, routes, 4, "login")
	if second["room_name"] == name {
		t.Fatal("第四人进入满桌")
	}
	other := second["room_name"].(string)
	if view := landlordTestValue(t, routes, "landlord", other, roomReadyCommand, uint64(4), "login").(map[string]any); view["phase"] != "waiting" {
		t.Fatal("其他桌状态异常")
	}
	// 等实际 10 秒叫地主超时；不修改 owner 内部字段，也不注入生产测试命令。
	wait := time.Until(time.UnixMilli(first["deadline_ms"].(int64)).Add(500 * time.Millisecond))
	if wait > 0 {
		<-time.After(wait)
	}
	after := landlordTestValue(t, routes, "landlord", name, roomSnapshotCommand, uint64(1), "new-login").(map[string]any)
	if after["turn"].(uint64) <= initialTurn {
		t.Fatal("独立 Pulse 未推进真实叫地主超时")
	}
	for _, value := range after["players"].([]any) {
		p := value.(map[string]any)
		if p["seat"] == initialSeat && p["call_choice"] != int64(-1) {
			t.Fatal("超时未按不叫处理")
		}
	}
}
