package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	dhmjrpc "github.com/cnooljaw/gsrdemo/common/rpc/dhmj"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
)

type fixtureMain struct {
	onStart func(context.Context, gsr.ServiceContext) error
}

func (m fixtureMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return m.onStart(ctx, self)
}

func (fixtureMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (fixtureMain) OnStop(context.Context) error { return nil }

type directoryBackend struct{ logins map[uint64]string }

func (directoryBackend) OnStart(context.Context, gsr.ServiceContext) error { return nil }
func (directoryBackend) OnStop(context.Context) error                      { return nil }

func (b directoryBackend) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	uid := args[0].(uint64)
	if b.logins == nil && cmd == settlementread.GetBalanceCommand {
		return map[string]any{"user_id": uid, "amount": int64(3000)}, nil
	}
	switch cmd {
	case onlinerpc.BindCommand:
		b.logins[uid] = args[1].(string)
		return nil, nil
	case onlinerpc.LookupCommand:
		return map[string]any{"login_id": b.logins[uid], "access_node": "access-01", "agent_name": "agent"}, nil
	default:
		return nil, runtime.ErrUnknownCommand
	}
}

func startDirectoryFixture(t *testing.T, routes map[cluster.NodeID]string, node cluster.NodeID, name cluster.ServiceName, service gsr.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	address := make(chan string, 1)
	nodes := map[cluster.NodeID]string{node: "127.0.0.1:0"}
	for id, addr := range routes {
		nodes[id] = addr
	}
	cfg := gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: nodes}}
	go func() {
		done <- gsr.Run(ctx, cfg, fixtureMain{onStart: func(ctx context.Context, self gsr.ServiceContext) error {
			id, err := self.CreateService(ctx, service)
			if err != nil {
				return err
			}
			if err := self.Node().RegisterService(name, id); err != nil {
				return err
			}
			if err := self.Node().OpenCluster(node); err != nil {
				return err
			}
			address <- self.Node().ClusterListenAddress()
			self.Exit()
			return nil
		}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("节点未退出")
		}
	})
	select {
	case addr := <-address:
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatal(err)
		}
		routes[node] = net.JoinHostPort("127.0.0.1", port)
	case err := <-done:
		done <- err
		t.Fatalf("节点未启动：%v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("节点启动超时")
	}
}

type agentPushSink struct {
	directoryBackend
	command         runtime.CommandID
	received        chan []any
	deals           chan []any
	includeRoundEnd bool
}

func (s agentPushSink) Handle(_ gsr.CommandContext, cmd runtime.CommandID, args ...any) (any, error) {
	if cmd == accessagent.DhmjDealPushCommand && s.deals != nil {
		s.deals <- args
		return nil, nil
	}
	if cmd != s.command && !(s.includeRoundEnd && cmd == accessagent.DhmjRoundEndPushCommand) {
		return nil, runtime.ErrUnknownCommand
	}
	s.received <- args
	return nil, nil
}

// TestDirectoryClusterJoinReadyAndReconnect 保护四人凑桌、按座位发牌、重连及同桌出牌广播。
func TestDirectoryClusterJoinReadyAndReconnect(t *testing.T) {
	routes := make(map[cluster.NodeID]string)
	pushes := make(chan []any, 8)
	deals := make(chan []any, seatCount)
	startDirectoryFixture(t, routes, "access-01", "agent", agentPushSink{command: accessagent.DhmjDiscardPushCommand, received: pushes, deals: deals})
	startDirectoryFixture(t, routes, "online", onlinerpc.ServiceName, directoryBackend{logins: map[uint64]string{}})
	startDirectoryFixture(t, routes, "settlement", settlementread.ServiceName, directoryBackend{})
	startDirectoryFixture(t, routes, "dhmj", dhmjrpc.ServiceName, newDirectory(defaultRoomRules, settlementJournal{}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var verified bool
	cfg := gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}
	err := gsr.Run(ctx, cfg, fixtureMain{onStart: func(ctx context.Context, self gsr.ServiceContext) error {
		call := func(cmd runtime.CommandID, args ...any) (any, error) {
			return self.Cluster().Call(ctx, "dhmj", dhmjrpc.ServiceName, cmd, args...)
		}
		bind := func(uid uint64, login string) error {
			_, err := self.Cluster().Call(ctx, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, uid, login)
			return err
		}
		var tableName string
		for uid := uint64(1); uid <= 4; uid++ {
			if err := bind(uid, "login"); err != nil {
				return err
			}
			result, err := call(dhmjrpc.JoinCommand, uid, "login", "access-01", "agent")
			if err != nil {
				return err
			}
			scene := result.(map[string]any)
			if uid == 1 {
				tableName = scene["table_name"].(string)
			}
			if scene["table_name"] != tableName || scene["seat"] != int64(uid-1) || scene["phase"] != "waiting" {
				return errors.New("四人未依序入同一张等待桌")
			}
			if _, err := call(dhmjrpc.DiscardCommand, uid, "login", uint64(1), int64(5)); err == nil {
				return errors.New("等待中的牌桌接受了出牌")
			}
			if _, err := call(dhmjrpc.ReadyCommand, uid, "login"); err != nil {
				return err
			}
		}
		for seat := range seatCount {
			var deal map[string]any
			select {
			case args := <-deals:
				deal = args[1].(map[string]any)
			case <-ctx.Done():
				return ctx.Err()
			}
			hand, _ := deal["hand"].([]any)
			wantUsers := []any{uint64(1), uint64(2), uint64(3), uint64(4)}
			if deal["table_name"] != tableName || deal["seat"] != int64(seat) || deal["revision"] != uint64(1) ||
				deal["dealer_seat"] != int64(0) || deal["remaining"] != int64(92) ||
				len(hand) != openingHandSize || !reflect.DeepEqual(deal["player_ids"], wantUsers) {
				return fmt.Errorf("错误的开局发牌：%v", deal)
			}
			if seat != 0 { // 开局至此只有庄家会补花或摸牌。
				value, err := call(dhmjrpc.SnapshotCommand, uint64(seat+1), "login")
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(hand, value.(map[string]any)["hand"]) {
					return fmt.Errorf("座位 %d 收到了其他人的起手牌", seat)
				}
			}
		}
		if _, err := call(dhmjrpc.LeaveCommand, uint64(1), "login"); err != nil {
			return err
		}
		if err := bind(1, "new-login"); err != nil {
			return err
		}
		if _, err := call(dhmjrpc.JoinCommand, uint64(1), "login", "access-01", "agent"); err == nil {
			return errors.New("在线目录已替换的旧登录仍可进入")
		}
		result, err := call(dhmjrpc.JoinCommand, uint64(1), "new-login", "access-01", "agent")
		if err != nil {
			return err
		}
		scene := result.(map[string]any)
		hand, ok := scene["hand"].([]any)
		if scene["table_name"] != tableName || scene["seat"] != int64(0) || scene["phase"] != "playing" || !ok || len(hand) != 14 {
			return errors.New("重连未恢复原牌桌和本人暗手")
		}
		if _, err := call(dhmjrpc.SnapshotCommand, uint64(1), "login"); err == nil {
			return errors.New("旧登录仍可读取重连后的暗手")
		}
		if _, err := call(dhmjrpc.SnapshotCommand, uint64(1), "new-login"); err != nil {
			return err
		}
		turn := scene["turn"].(uint64)
		tile := hand[0].(int64)
		for _, attempt := range []struct {
			uid   uint64
			login string
			turn  uint64
			tile  int64
		}{
			{1, "login", turn, tile},
			{2, "login", turn, tile},
			{1, "new-login", turn - 1, tile},
			{1, "new-login", turn, tile + 256},
		} {
			if _, err := call(dhmjrpc.DiscardCommand, attempt.uid, attempt.login, attempt.turn, attempt.tile); err == nil {
				return errors.New("非法远程出牌被接受")
			}
		}
		if value, err := call(dhmjrpc.DiscardCommand, uint64(1), "new-login", turn, tile); err != nil || value != true {
			return fmt.Errorf("当前座位出牌未执行：%v %w", value, err)
		}
		discardRevision := scene["revision"].(uint64) + 1
		event := map[string]any{"table_name": tableName, "revision": discardRevision, "seat": int64(0), "turn": turn, "tile": tile}
		for _, login := range []string{"new-login", "login", "login", "login"} {
			select {
			case args := <-pushes:
				if !reflect.DeepEqual(args, []any{login, event}) {
					return fmt.Errorf("出牌推送没有使用当前路由与轮次：%v", args)
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		result, err = call(dhmjrpc.SnapshotCommand, uint64(2), "login")
		if err != nil {
			return err
		}
		snapshot := result.(map[string]any)
		out := snapshot["discards"].([]any)[0].([]any)
		if len(out) != 1 || out[0] != tile || snapshot["revision"].(uint64) < discardRevision {
			return errors.New("其他座位未看到已执行的出牌")
		}
		verified = true
		cancel()
		self.Exit()
		return nil
	}})
	if !verified || (err != nil && !errors.Is(err, context.Canceled)) {
		t.Fatalf("凑桌联调未通过：%v", err)
	}
}
