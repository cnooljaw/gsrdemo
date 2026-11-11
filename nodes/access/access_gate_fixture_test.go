// MAC Gate、连接与节点启动的共享测试夹具。
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
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

const testNodeYAML = "instance: access-01\nlisten: access-01\nnodes_file: nodes.yaml\nlog_directory: logs\n"

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
	path := accessConfigFile(t, testNodeYAML+gateYAML, "nodes:\n  access-01: "+clusterAddress+"\n  settlement: 127.0.0.1:19005\n")
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
			Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"access-01": clusterAddress}},
		}, &readyMain{Service: NewMain("access-01", config.Gate), ready: ready})
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

func dialAccessSocket(t *testing.T, address string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second}
	connection, _, err := dialer.Dial("ws://"+address+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// accessClient 只为接入测试实现 GSR v1 的握手与认证上行，不进入业务代码。
type accessClient struct {
	*websocket.Conn
	macKey   []byte
	sequence uint64
}

func dialAccess(t *testing.T, address string) *accessClient {
	t.Helper()
	connection := dialAccessSocket(t, address)
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientPublicKey := privateKey.PublicKey().Bytes()
	request := append([]byte{1}, clientPublicKey...)
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteMessage(websocket.BinaryMessage, request); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	kind, response, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.BinaryMessage || len(response) != 33 || response[0] != 1 {
		t.Fatal("expected GSR v1 MAC handshake response")
	}
	serverPublicKey, err := ecdh.X25519().NewPublicKey(response[1:])
	if err != nil {
		t.Fatal(err)
	}
	sharedSecret, err := privateKey.ECDH(serverPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// 标签和公钥顺序遵循 GSR v1.0.2 gate/security.go 的 wire 契约。
	saltInput := append([]byte("gsr/wss/hkdf-salt/v1"), clientPublicKey...)
	saltInput = append(saltInput, response[1:]...)
	salt := sha256.Sum256(saltInput)
	macKey, err := hkdf.Key(sha256.New, sharedSecret, salt[:], "gsr/wss/c2s-hmac-sha256/v1", sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	return &accessClient{Conn: connection, macKey: macKey}
}

func (client *accessClient) authenticatedFrame(envelope []byte) []byte {
	client.sequence++
	frame := make([]byte, 8, 8+len(envelope)+16)
	binary.BigEndian.PutUint64(frame, client.sequence)
	frame = append(frame, envelope...)
	mac := hmac.New(sha256.New, client.macKey)
	_, _ = mac.Write(frame)
	return append(frame, mac.Sum(nil)[:16]...)
}

func expectUnsupportedRequest(t *testing.T, connection *accessClient, requestID uint32) {
	t.Helper()
	// 最大 uint32 是未定义编号，用框架 Envelope 走真实接入，不创建测试业务协议。
	frame, err := proto.Marshal(&commonv1.Envelope{MsgId: ^uint32(0), RequestId: requestID})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteMessage(websocket.BinaryMessage, connection.authenticatedFrame(frame)); err != nil {
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
