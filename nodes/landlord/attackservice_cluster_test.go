package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	landlordrpc "github.com/cnooljaw/gsrdemo/common/rpc/landlord"
	"github.com/cnooljaw/gsrdemo/common/rpc/landlordattack"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
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
	case onlinerpc.BindCommand:
		backend.logins[uid] = args[1].(string)
		return nil, nil
	case onlinerpc.LookupCommand:
		login := backend.logins[uid]
		if login == "" {
			login = "login"
		}
		return map[string]any{"login_id": login, "access_node": "access-01", "agent_name": "agent"}, nil
	case settlementread.GetBalanceCommand:
		return map[string]any{"user_id": uid, "amount": int64(10000), "version": uint64(1)}, nil
	case landlordrpc.ReleaseCommand:
		return true, nil
	}
	return nil, runtime.ErrUnknownCommand
}

func startLandlordTestNode(t *testing.T, routes map[cluster.NodeID]string, node string, services map[string]gsr.Service) {
	t.Helper()
	nodes := map[cluster.NodeID]string{}
	for k, v := range routes {
		nodes[k] = v
	}
	// Call 沿原连接回复，不再预选回调端口；启动时由系统直接分配。
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
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		routes[cluster.NodeID(node)] = net.JoinHostPort("127.0.0.1", port)
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	landlordTestValue(t, routes, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uid, login)
	route := landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, landlordrpc.ResolveCommand, uid, login).(map[string]any)
	return landlordTestValue(t, routes, "landlord", route["room_name"].(string), landlordattack.RoomJoinCommand, uid, login, "access-01", "agent", route["membership"]).(map[string]any)
}

// landlordReadyProbe 不创建 Pulse，固定快照观察时序；仅让第一次封桌 Send 未被接纳。
type landlordReadyProbe struct {
	*attackService
	sealAttempts int
}

var _ gsr.Service = (*landlordReadyProbe)(nil)

func (*landlordReadyProbe) OnStart(context.Context, gsr.ServiceContext) error { return nil }

func (probe *landlordReadyProbe) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == 99 {
		return int64(probe.sealAttempts), nil
	}
	return probe.attackService.Handle(&landlordReadyContext{CommandContext: self, probe: probe}, cmd, args...)
}

type landlordReadyContext struct {
	gsr.CommandContext
	probe *landlordReadyProbe
}

var _ gsr.CommandContext = (*landlordReadyContext)(nil)

func (self *landlordReadyContext) SendName(name cluster.ServiceName, cmd runtime.CommandID, args ...any) error {
	if name == landlordName && cmd == sealCommand {
		self.probe.sealAttempts++
		if self.probe.sealAttempts == 1 {
			return runtime.ErrMailboxFull
		}
	}
	return self.CommandContext.SendName(name, cmd, args...)
}

// TestReadyFailureKeepsTableAndRetryDealsOnce 保护第三人准备失败不改变原桌，重试成功后重复 Ready 不再次封桌或发牌。
func TestReadyFailureKeepsTableAndRetryDealsOnce(t *testing.T) {
	table, directory := occupiedLandlordTable()
	table.game.Players[0].Ready, table.game.Players[1].Ready = true, true
	room := table.game.Name
	probe := &landlordReadyProbe{attackService: table}
	routes := map[cluster.NodeID]string{}
	startLandlordTestNode(t, routes, "settlement", map[string]gsr.Service{settlementread.ServiceName: &landlordTestBackend{}})
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{landlordrpc.ServiceName: directory, room: probe})
	before := landlordTestValue(t, routes, "landlord", room, landlordattack.RoomSnapshotCommand, uint64(3), "login").(map[string]any)
	if _, err := landlordTestRPC(t, routes, "landlord", room, landlordattack.RoomReadyCommand, uint64(3), "login"); !errors.Is(err, runtime.ErrMailboxFull) {
		t.Fatalf("未注入封桌消息满邮箱错误: %v", err)
	}
	after := landlordTestValue(t, routes, "landlord", room, landlordattack.RoomSnapshotCommand, uint64(3), "login")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("失败的 Ready 改变了准备、轮次或手牌")
	}
	dealt := landlordTestValue(t, routes, "landlord", room, landlordattack.RoomReadyCommand, uint64(3), "login").(map[string]any)
	if dealt["phase"] != "calling" || dealt["turn"] != before["turn"].(uint64)+1 || len(dealt["hand"].([]any)) != 17 || len(dealt["bottom"].([]any)) != 0 {
		t.Fatal("重试未正常发牌或提前公开底牌")
	}
	for _, value := range dealt["players"].([]any) {
		if value.(map[string]any)["ready"] != true {
			t.Fatal("重试发牌后遗漏了准备状态")
		}
	}
	if _, err := landlordTestRPC(t, routes, "landlord", room, landlordattack.RoomReadyCommand, uint64(3), "login"); !errors.Is(err, errLandlordBusy) {
		t.Fatalf("发牌后仍接受重复 Ready: %v", err)
	}
	current := landlordTestValue(t, routes, "landlord", room, landlordattack.RoomSnapshotCommand, uint64(3), "login")
	if !reflect.DeepEqual(dealt, current) || landlordTestValue(t, routes, "landlord", room, 99) != int64(2) {
		t.Fatal("重复 Ready 改变了已发的牌、轮次或重复封桌")
	}
}

// landlordMemberPushRecorder 只观察第三个座位收到的快照，计数和最后快照均由本 Service 管理。
type landlordMemberPushRecorder struct {
	count  int64
	latest map[string]any
}

var _ gsr.Service = (*landlordMemberPushRecorder)(nil)

func (*landlordMemberPushRecorder) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (*landlordMemberPushRecorder) OnStop(context.Context) error                      { return nil }
func (recorder *landlordMemberPushRecorder) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == 99 {
		return map[string]any{"count": recorder.count, "snapshot": recorder.latest}, nil
	}
	if cmd != accessagent.LandlordSnapshotPushCommand {
		return nil, runtime.ErrUnknownCommand
	}
	view := args[1].(map[string]any)
	if view["seat"] == int64(2) {
		recorder.count++
		recorder.latest = view
	}
	return nil, nil
}

// landlordReclaimProbe 不启用周期 Timer，测试显式推进 Tick；可阻止指定成员释放，以观察部分成功的通知。
type landlordReclaimProbe struct {
	*attackService
	blockedUID uint64
}

var _ gsr.Service = (*landlordReclaimProbe)(nil)

func (*landlordReclaimProbe) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (probe *landlordReclaimProbe) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd != 98 {
		return probe.attackService.Handle(self, cmd, args...)
	}
	err := probe.handleTick(&landlordReclaimContext{CommandContext: self, blockedUID: probe.blockedUID})
	// 同一推送目标随后处理 Call，确保前面的 Send 已被观察端处理，避免用睡眠猜测送达时间。
	if _, barrierErr := remote(self, "access", "agent", 99); barrierErr != nil {
		return nil, barrierErr
	}
	return nil, err
}

type landlordReclaimContext struct {
	gsr.CommandContext
	blockedUID uint64
}

var _ gsr.CommandContext = (*landlordReclaimContext)(nil)

func (*landlordReclaimContext) Source() runtime.ServiceID { return 1 }
func (self *landlordReclaimContext) CallName(ctx context.Context, name cluster.ServiceName, cmd runtime.CommandID, args ...any) (any, error) {
	if name == landlordName && cmd == releaseCommand && args[0] == self.blockedUID {
		return nil, runtime.ErrMailboxFull
	}
	return self.CommandContext.CallName(ctx, name, cmd, args...)
}

// TestReclaimedMembersPushSnapshot 保护实际回收和部分成功均通知剩余玩家，新预留或空转 Tick 不额外推送。
func TestReclaimedMembersPushSnapshot(t *testing.T) {
	for _, scenario := range []struct {
		name, phase    string
		blockedUID     uint64
		newReservation bool
		players        int
		pushes         int64
	}{
		{name: "waiting", phase: "waiting", players: 1, pushes: 1},
		{name: "partial_failure", phase: "waiting", blockedUID: 2, players: 2, pushes: 1},
		{name: "new_reservations", phase: "waiting", newReservation: true, players: 3},
		{name: "settled", phase: "finished", players: 1, pushes: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			table, directory := occupiedLandlordTable()
			room := table.game.Name
			table.game.Phase = scenario.phase
			if scenario.phase == "finished" {
				table.game.Landlord, table.game.Winner = 0, 2
				table.settlement.prepareOperation(table.game)
				table.settlement.operation.Status = "committed"
			}
			for seat := range 2 {
				table.game.detach(seat)
				if scenario.newReservation {
					uid := table.game.Players[seat].UserID
					directory.routes[uid] = landlordRoute{room, "new-login", "new-membership"}
				}
			}
			routes := map[cluster.NodeID]string{}
			startLandlordTestNode(t, routes, "access", map[string]gsr.Service{"agent": &landlordMemberPushRecorder{}})
			startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{
				landlordrpc.ServiceName: directory, room: &landlordReclaimProbe{attackService: table, blockedUID: scenario.blockedUID},
			})
			_, err := landlordTestRPC(t, routes, "landlord", room, 98)
			if (scenario.blockedUID != 0 && !errors.Is(err, runtime.ErrMailboxFull)) || (scenario.blockedUID == 0 && err != nil) {
				t.Fatalf("Tick 结果与注入不符: %v", err)
			}
			view := landlordTestValue(t, routes, "landlord", room, landlordattack.RoomSnapshotCommand, uint64(3), "login").(map[string]any)
			if len(view["players"].([]any)) != scenario.players {
				t.Fatal("回收后的座位数量不正确")
			}
			observed := landlordTestValue(t, routes, "access", "agent", 99).(map[string]any)
			if observed["count"] != scenario.pushes {
				t.Fatalf("成员变化推送次数不符: %v", observed)
			}
			if scenario.pushes != 0 && !reflect.DeepEqual(view, observed["snapshot"]) {
				t.Fatal("推送不是回收后的最新快照")
			}
			landlordTestValue(t, routes, "landlord", room, 98)
			idle := landlordTestValue(t, routes, "access", "agent", 99).(map[string]any)
			if !reflect.DeepEqual(observed, idle) {
				t.Fatal("空转或退避 Tick 重复推送")
			}
		})
	}
}

// landlordLeaveProbe 不创建 Pulse，固定下一次 Join 先于后台回收；仅对第一次 Release 注入失败。
type landlordLeaveProbe struct {
	*attackService
	afterRelease bool
	releaseCalls int
}

var _ gsr.Service = (*landlordLeaveProbe)(nil)

func (*landlordLeaveProbe) OnStart(context.Context, gsr.ServiceContext) error { return nil }

func (probe *landlordLeaveProbe) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == 99 {
		return int64(probe.releaseCalls), nil
	}
	return probe.attackService.Handle(&landlordLeaveContext{CommandContext: self, probe: probe}, cmd, args...)
}

type landlordLeaveContext struct {
	gsr.CommandContext
	probe *landlordLeaveProbe
}

var _ gsr.CommandContext = (*landlordLeaveContext)(nil)

func (self *landlordLeaveContext) CallName(ctx context.Context, name cluster.ServiceName, cmd runtime.CommandID, args ...any) (any, error) {
	if name != landlordrpc.ServiceName || cmd != landlordrpc.ReleaseCommand {
		return self.CommandContext.CallName(ctx, name, cmd, args...)
	}
	self.probe.releaseCalls++
	if self.probe.releaseCalls == 1 && !self.probe.afterRelease {
		return nil, context.DeadlineExceeded
	}
	value, err := self.CommandContext.CallName(ctx, name, cmd, args...)
	if self.probe.releaseCalls == 1 && err == nil {
		return nil, context.DeadlineExceeded
	}
	return value, err
}

// TestJoinReconcilesUnconfirmedLeave 保护新人早于 Pulse 到达时能收掉旧座位，同用户新预留则保留座位并隔离迟到清理。
func TestJoinReconcilesUnconfirmedLeave(t *testing.T) {
	for _, afterRelease := range []bool{true, false} {
		name, uid, login := "new_player_before_pulse", uint64(4), "login"
		if !afterRelease {
			name, uid, login = "rejoin_before_release", 1, "new-login"
		}
		t.Run(name, func(t *testing.T) {
			table, directory := occupiedLandlordTable()
			old := table.game.Players[0]
			probe := &landlordLeaveProbe{attackService: table, afterRelease: afterRelease}
			routes := map[cluster.NodeID]string{}
			startLandlordTestNode(t, routes, "online", map[string]gsr.Service{onlinerpc.ServiceName: &landlordTestBackend{logins: map[uint64]string{}}})
			startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{landlordrpc.ServiceName: directory, table.game.Name: probe})
			if left := landlordTestValue(t, routes, "landlord", table.game.Name, landlordattack.RoomLeaveCommand, uint64(1), "login"); left != true {
				t.Fatal("释放错误阻止了接纳离开")
			}
			joined := joinLandlordTest(t, routes, uid, login)
			if joined["room_name"] != table.game.Name || len(joined["players"].([]any)) != 3 {
				t.Fatal("Join 未复用原桌的空位")
			}
			if calls := landlordTestValue(t, routes, "landlord", table.game.Name, 99); calls != int64(2) {
				t.Fatalf("Join 没有主动核对待回收座位: %v", calls)
			}
			if _, err := landlordTestRPC(t, routes, "landlord", table.game.Name, landlordattack.RoomSnapshotCommand, uint64(1), "login"); err == nil {
				t.Fatal("旧成员仍能访问牌桌")
			}
			if !afterRelease {
				if released := landlordTestValue(t, routes, "landlord", landlordrpc.ServiceName, landlordrpc.ReleaseCommand,
					old.UserID, old.LoginID, table.game.Name, old.Membership, true); released != false {
					t.Fatal("旧清理释放了新预留")
				}
				landlordTestValue(t, routes, "landlord", table.game.Name, landlordattack.RoomSnapshotCommand, uid, login)
			}
		})
	}
}

type landlordSlowRelease struct{ *landlordService }

var _ gsr.Service = (*landlordSlowRelease)(nil)

func (directory *landlordSlowRelease) Handle(self gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == landlordrpc.ReleaseCommand {
		time.Sleep(1100 * time.Millisecond)
	}
	return directory.landlordService.Handle(self, cmd, args...)
}

// TestSlowMemberReleaseKeepsSnapshotResponsive 保护目录释放超过 Call 预算时，Pulse 退避使积压 Tick 不会饿死本桌快照。
func TestSlowMemberReleaseKeepsSnapshotResponsive(t *testing.T) {
	table, directory := occupiedLandlordTable()
	table.game.detach(0)
	routes := map[cluster.NodeID]string{}
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{
		landlordrpc.ServiceName: &landlordSlowRelease{directory},
		table.game.Name:         &landlordSettlementProbe{table},
	})
	time.Sleep(2200 * time.Millisecond)
	if _, err := landlordTestRPC(t, routes, "landlord", table.game.Name, landlordattack.RoomSnapshotCommand, uint64(2), "login"); err != nil {
		t.Fatalf("慢目录引起积压 Tick，导致快照无法响应: %v", err)
	}
}

// TestAttackRejoinAndPulse 保护随机凑桌、叫抢中的原手牌重连、旧身份清理隔离及真实 Pulse 超时推进，其他桌仍可准备。
func TestAttackRejoinAndPulse(t *testing.T) {
	routes := map[cluster.NodeID]string{}
	startLandlordTestNode(t, routes, "online", map[string]gsr.Service{onlinerpc.ServiceName: &landlordTestBackend{logins: map[uint64]string{}}})
	reads := map[string]gsr.Service{}
	for _, name := range []string{settlementread.ServiceName} {
		reads[name] = &landlordTestBackend{}
	}
	startLandlordTestNode(t, routes, "settlement-02", reads)
	startLandlordTestNode(t, routes, "landlord", map[string]gsr.Service{landlordrpc.ServiceName: newLandlordService(landlordJournal{}, "settlement-02")})
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
		first = landlordTestValue(t, routes, "landlord", name, landlordattack.RoomReadyCommand, uid, "login").(map[string]any)
	}
	if first["phase"] != "calling" || len(first["bottom"].([]any)) != 0 {
		t.Fatal("未进入叫地主或提前泄露底牌")
	}
	actorUID := first["players"].([]any)[int(first["turn_seat"].(int64))].(map[string]any)["user_id"].(uint64)
	if _, err := landlordTestRPC(t, routes, "landlord", name, landlordattack.RoomRobCommand, actorUID, "login", first["turn"], true); err == nil {
		t.Fatal("叫地主阶段接受了抢地主")
	}
	initialTurn := first["turn"].(uint64)
	initialSeat := first["turn_seat"].(int64)
	own := landlordTestValue(t, routes, "landlord", name, landlordattack.RoomSnapshotCommand, uint64(1), "login").(map[string]any)
	rejoined := joinLandlordTest(t, routes, 1, "new-login")
	if rejoined["room_name"] != name || !reflect.DeepEqual(own["hand"], rejoined["hand"]) {
		t.Fatal("重连未保留原桌和手牌")
	}
	if _, err := landlordTestRPC(t, routes, "landlord", name, landlordattack.RoomLeaveCommand, uint64(1), "login"); err == nil {
		t.Fatal("旧身份仍可离开新成员")
	}
	second := joinLandlordTest(t, routes, 4, "login")
	if second["room_name"] == name {
		t.Fatal("第四人进入满桌")
	}
	other := second["room_name"].(string)
	if view := landlordTestValue(t, routes, "landlord", other, landlordattack.RoomReadyCommand, uint64(4), "login").(map[string]any); view["phase"] != "waiting" {
		t.Fatal("其他桌状态异常")
	}
	// 等实际 10 秒叫地主超时；不修改 owner 内部字段，也不注入生产测试命令。
	wait := time.Until(time.UnixMilli(first["deadline_ms"].(int64)).Add(500 * time.Millisecond))
	if wait > 0 {
		<-time.After(wait)
	}
	after := landlordTestValue(t, routes, "landlord", name, landlordattack.RoomSnapshotCommand, uint64(1), "new-login").(map[string]any)
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
