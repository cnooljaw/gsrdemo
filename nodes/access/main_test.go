package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
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

const testNodeYAML = "instance: access-01\nlisten: access\nnodes_file: nodes.yaml\nlog_directory: logs\n"
const testGateYAML = "gate:\n  address: 127.0.0.1:7888\n  max_connections: 4\n  max_frame_bytes: 256\n  max_outbound_queue_bytes: 1024\n"

func accessConfigFile(t *testing.T, node, routes string) string {
	t.Helper()
	directory := t.TempDir()
	for name, contents := range map[string]string{"access.yaml": node, "nodes.yaml": routes} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(directory, "access.yaml")
}

// TestAccessConfigKeepsGatePrivateAndResolvesSharedHost 保护 access 私有字段严格解析且地址表/日志按配置目录解析。
func TestAccessConfigKeepsGatePrivateAndResolvesSharedHost(t *testing.T) {
	path := accessConfigFile(t, testNodeYAML+testGateYAML, "nodes:\n  access: 127.0.0.1:19001\n")
	config, err := loadAccessConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Node.Nodes["access"] != "127.0.0.1:19001" || config.Node.NodesFile != filepath.Join(filepath.Dir(path), "nodes.yaml") || config.Node.LogDirectory != filepath.Join(filepath.Dir(path), "logs") {
		t.Fatalf("shared host config lost: %+v", config.Node)
	}
	if config.Gate.Address != "127.0.0.1:7888" || config.Gate.MaxConnections != 4 || config.Gate.MaxFrameBytes != 256 || config.Gate.MaxOutboundQueueBytes != 1024 {
		t.Fatalf("private Gate config lost: %+v", config.Gate)
	}
	for name, node := range map[string]string{
		"unknown gate field":   testNodeYAML + testGateYAML + "  unknown: true\n",
		"duplicate gate field": testNodeYAML + testGateYAML + "  address: 127.0.0.1:7889\n",
		"wrong host role":      strings.Replace(testNodeYAML, "listen: access", "listen: login", 1) + testGateYAML,
	} {
		t.Run(name, func(t *testing.T) {
			path := accessConfigFile(t, node, "nodes:\n  access: 127.0.0.1:19001\n")
			if _, err := loadAccessConfig(path); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	example, err := loadAccessConfig(filepath.Join("..", "..", "configs", "access-01.yaml"))
	if err != nil || len(example.Node.Nodes) != 5 || example.Gate.MaxConnections <= 0 {
		t.Fatalf("example access config: %+v, err=%v", example, err)
	}
}

func testAddresses(t *testing.T) (string, string) {
	t.Helper()
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	return first.Addr().String(), second.Addr().String()
}

// readyMain 只观察实际 MainService 的启动结果，不替换 Gate/Agent 组装。
type readyMain struct {
	gsr.Service
	ready chan<- error
}

var _ gsr.Service = (*readyMain)(nil)

func (service *readyMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	err := service.Service.OnStart(ctx, self)
	service.ready <- err
	return err
}

func startAccess(t *testing.T) (string, string, func()) {
	t.Helper()
	clusterAddress, gateAddress := testAddresses(t)
	gateYAML := strings.Replace(testGateYAML, "127.0.0.1:7888", gateAddress, 1)
	path := accessConfigFile(t, testNodeYAML+gateYAML, "nodes:\n  access: "+clusterAddress+"\n")
	config, err := loadAccessConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- gsr.Run(ctx, gsr.Config{
			Logging: logging.Config{Directory: config.Node.LogDirectory},
			Cluster: cluster.ClusterConfig{Nodes: map[cluster.NodeID]string{"access": clusterAddress}},
		}, &readyMain{Service: NewMain("access", config.Gate), ready: ready})
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("access shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("access shutdown timed out")
		}
	}
	t.Cleanup(stop)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("access startup: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("access startup timed out")
	}
	return clusterAddress, gateAddress, stop
}

func dialAccess(t *testing.T, address string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	connection, _, err := dialer.Dial("ws://"+address+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func expectUnsupportedRequest(t *testing.T, connection *websocket.Conn, requestID uint32) {
	t.Helper()
	// 最大 uint32 是未定义编号，用框架 Envelope 走真实接入，不创建测试业务协议。
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: ^uint32(0), RequestId: requestID})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	kind, frame, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var envelope commonv1.Envelope
	if err := proto.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if kind != websocket.BinaryMessage || envelope.GetMsgId() != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) || envelope.GetRequestId() != requestID {
		t.Fatalf("unexpected error Envelope: %v", &envelope)
	}
	var response commonv1.ErrorResponse
	if err := proto.Unmarshal(envelope.GetPayload(), &response); err != nil {
		t.Fatal(err)
	}
	if response.GetCode() != commonv1.ErrorCode_ERROR_CODE_UNSUPPORTED_MESSAGE {
		t.Fatalf("unexpected error code: %v", response.GetCode())
	}
}

// TestAccessConnectionFailureIsIsolatedAndShutdownClosesResources 保护实际 Gate/Agent 接线、配置帧限制、连接隔离和停机释放。
func TestAccessConnectionFailureIsIsolatedAndShutdownClosesResources(t *testing.T) {
	clusterAddress, gateAddress, stop := startAccess(t)
	first := dialAccess(t, gateAddress)
	second := dialAccess(t, gateAddress)
	expectUnsupportedRequest(t, first, 31)
	if err := first.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := first.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{1}, 512)); err != nil {
		t.Fatal(err)
	}
	expectConnectionClosed(t, first)
	expectUnsupportedRequest(t, second, 32)
	stop()
	expectConnectionClosed(t, second)
	for _, address := range []string{clusterAddress, gateAddress} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("listener not released: %s: %v", address, err)
		}
		_ = listener.Close()
	}
}

func expectConnectionClosed(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err := connection.ReadMessage()
	if err == nil {
		t.Fatal("connection remained open")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("connection did not close before deadline")
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
		Cluster: cluster.ClusterConfig{Nodes: map[cluster.NodeID]string{"access": clusterAddress}},
	}, NewMain("access", gateOptions{
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
