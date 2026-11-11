package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// TestAccessConfigKeepsGatePrivateAndResolvesSharedHost 保护 access 私有字段严格解析且地址表/日志按配置目录解析。
func TestAccessConfigKeepsGatePrivateAndResolvesSharedHost(t *testing.T) {
	path := accessConfigFile(t, testNodeYAML+testGateYAML, "nodes:\n  access-01: 127.0.0.1:19001\n")
	config, err := loadAccessConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Node.Nodes["access-01"] != "127.0.0.1:19001" || config.Node.NodesFile != filepath.Join(filepath.Dir(path), "nodes.yaml") || config.Node.LogDirectory != filepath.Join(filepath.Dir(path), "logs") {
		t.Fatalf("shared host config lost: %+v", config.Node)
	}
	if config.Gate.Address != "127.0.0.1:7888" || config.Gate.MaxConnections != 4 || config.Gate.MaxFrameBytes != 256 || config.Gate.MaxOutboundQueueBytes != 1024 {
		t.Fatalf("private Gate config lost: %+v", config.Gate)
	}
	for name, node := range map[string]string{
		"unknown gate field":   testNodeYAML + testGateYAML + "  unknown: true\n",
		"duplicate gate field": testNodeYAML + testGateYAML + "  address: 127.0.0.1:7889\n",
		"wrong host role":      strings.Replace(testNodeYAML, "listen: access-01", "listen: login", 1) + testGateYAML,
	} {
		t.Run(name, func(t *testing.T) {
			path := accessConfigFile(t, node, "nodes:\n  access-01: 127.0.0.1:19001\n")
			if _, err := loadAccessConfig(path); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	example, err := loadAccessConfig(filepath.Join("..", "..", "configs", "access-01.yaml"))
	if err != nil || len(example.Node.Nodes) != 7 || example.Gate.MaxConnections <= 0 {
		t.Fatalf("example access config: %+v, err=%v", example, err)
	}
	second, err := loadAccessConfig(filepath.Join("..", "..", "configs", "access-02.yaml"))
	if err != nil || second.Node.Listen != "access-02" || second.Node.Nodes[second.Node.Listen] == example.Node.Nodes[example.Node.Listen] || second.Gate.Address == example.Gate.Address {
		t.Fatalf("second access must use independent ports: %+v, err=%v", second, err)
	}
}

// TestAccessConnectionFailureIsIsolatedAndShutdownClosesResources 保护 MAC 接入接线、拒绝未握手客户端、配置帧限制、连接隔离和停机释放。
func TestAccessConnectionFailureIsIsolatedAndShutdownClosesResources(t *testing.T) {
	clusterAddress, gateAddress, stop := startAccess(t)
	plain := dialAccessSocket(t, gateAddress)
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: ^uint32(0), RequestId: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := plain.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	expectConnectionClosed(t, plain)
	first := dialAccess(t, gateAddress)
	second := dialAccess(t, gateAddress)
	expectUnsupportedRequest(t, first, 31)
	if err := first.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := first.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{1}, 512)); err != nil {
		t.Fatal(err)
	}
	expectConnectionClosed(t, first.Conn)
	expectUnsupportedRequest(t, second, 32)
	expectUnsupportedRequest(t, second, 33)
	stop()
	expectConnectionClosed(t, second.Conn)
	for _, address := range []string{clusterAddress, gateAddress} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("listener not released: %s: %v", address, err)
		}
		_ = listener.Close()
	}
}

// TestAccessStartupFailureReleasesCluster 保护 Gate 监听失败能向宿主返回并释放已打开的 Cluster。
func TestAccessStartupFailureReleasesCluster(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	clusterAddress, _ := testAddresses(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: t.TempDir()},
		Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"access-01": clusterAddress}},
	}, NewMain("access-01", gateOptions{
		Address: occupied.Addr().String(), MaxConnections: 4, MaxFrameBytes: 256, MaxOutboundQueueBytes: 1024,
	}))
	if err == nil {
		t.Fatal("Gate startup failure reported success")
	}
	listener, err := net.Listen("tcp", clusterAddress)
	if err != nil {
		t.Fatalf("Cluster not released after Gate startup failure: %v", err)
	}
	_ = listener.Close()
}

// TestAccessConfigSupportsIndependentRoutes 保护各接入实例使用自己的路由和端口，同时拒绝其它角色及缺失的节点路由。
func TestAccessConfigSupportsIndependentRoutes(t *testing.T) {
	for _, route := range []string{"access-01", "access-02", "access-03", "access-100"} {
		node := strings.Replace(testNodeYAML, "listen: access-01", "listen: "+route, 1)
		path := accessConfigFile(t, node+testGateYAML, "nodes:\n  "+route+": 127.0.0.1:29001\n")
		config, err := loadAccessConfig(path)
		if err != nil || config.Node.Listen != route || config.Node.Nodes[route] != "127.0.0.1:29001" {
			t.Fatalf("独立接入路由 %s: %+v %v", route, config, err)
		}
	}
	for _, route := range []string{"login", "access", "access-", "accessor", "access-1", "access-00", "access-001", "access-abc", "access-+1"} {
		node := strings.Replace(testNodeYAML, "listen: access-01", "listen: "+route, 1)
		path := accessConfigFile(t, node+testGateYAML, "nodes:\n  "+route+": 127.0.0.1:29001\n")
		if _, err := loadAccessConfig(path); err == nil {
			t.Fatalf("非法接入路由 %s 被接纳", route)
		}
	}
	node := strings.Replace(testNodeYAML, "listen: access-01", "listen: access-02", 1)
	if _, err := loadAccessConfig(accessConfigFile(t, node+testGateYAML, "nodes:\n  access-01: 127.0.0.1:29001\n")); err == nil {
		t.Fatal("路由未配置时不应退回 access-01")
	}
}

// TestAccessConfigDoesNotRequireSettlementTarget 保护接入节点不依赖结算路由，并拒绝已移除的结算目标字段。
func TestAccessConfigDoesNotRequireSettlementTarget(t *testing.T) {
	routes := "nodes:\n  access-01: 127.0.0.1:19001\n"
	path := accessConfigFile(t, testNodeYAML+testGateYAML, routes)
	if _, err := loadAccessConfig(path); err != nil {
		t.Fatalf("access 不应要求结算目标: %v", err)
	}
	path = accessConfigFile(t, testNodeYAML+testGateYAML+"settlement_node: settlement\n", routes)
	if _, err := loadAccessConfig(path); err == nil {
		t.Fatal("已移除的结算目标字段不应继续被接纳")
	}
}
