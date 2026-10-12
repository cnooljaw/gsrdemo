package process

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type readyEvent struct {
	Event         string `json:"event"`
	Instance      string `json:"instance"`
	ListenAddress string `json:"listen_address"`
	GateAddress   string `json:"gate_address"`
}

type child struct {
	command *exec.Cmd
	done    chan error
	stderr  bytes.Buffer
	stopped bool
}

// TestFiveNodeProbe 保护真实 TS→Agent→四节点 Call、mole→Agent Push 的接线与故障可观察性。
func TestFiveNodeProbe(t *testing.T) {
	if os.Getenv("GSR_PROCESS_TEST") != "1" {
		t.Skip("通过 scripts/process-test.sh 显式启用真实进程验收")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binDirectory := os.Getenv("GSR_NODE_BIN")
	if binDirectory == "" {
		binDirectory = filepath.Join(root, "bin")
	}
	for _, name := range []string{"login", "online", "mole", "settlement", "access"} {
		if _, err := os.Stat(filepath.Join(binDirectory, name)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	t.Cleanup(cancel)
	routes := make(map[string]string)
	reservations := make(map[string]net.Listener)
	for _, name := range []string{"login", "online", "mole", "settlement", "access", "gate"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		reservations[name] = listener
		routes[name] = listener.Addr().String()
	}
	t.Cleanup(func() {
		for _, listener := range reservations {
			_ = listener.Close()
		}
	})
	directory := t.TempDir()
	children := make(map[string]*child)
	for _, name := range []string{"login", "online", "mole", "settlement", "access"} {
		_ = reservations[name].Close()
		if name == "access" {
			_ = reservations["gate"].Close()
		}
		cfg := map[string]any{
			"instance": name + "-test", "listen": name,
			"log_directory": filepath.Join(directory, "logs", name),
			"nodes":         map[string]string{"login": routes["login"], "online": routes["online"], "mole": routes["mole"], "settlement": routes["settlement"], "access": routes["access"]},
		}
		if name == "access" {
			cfg["gate_address"] = routes["gate"]
		}
		path := filepath.Join(directory, name+".json")
		content, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		process := startNode(t, ctx, root, binDirectory, name, path)
		children[name] = process
	}
	url := "ws://" + routes["gate"] + "/ws"
	// 两个同时活跃的连接应分别收到自己的响应和推送，不能串到另一个 Agent。
	type clientResult struct {
		text   string
		events string
		err    error
	}
	results := make(chan clientResult, 2)
	for _, text := range []string{"first-client", "second-client"} {
		go func(text string) {
			events, err := runClient(ctx, root, url, text)
			results <- clientResult{text: text, events: events, err: err}
		}(text)
	}
	for index := 0; index < 2; index++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("client failed: %v; events=%s", result.err, result.events)
			}
			if !strings.Contains(result.events, `"event":"complete"`) || !strings.Contains(result.events, `"text":"`+result.text+`"`) {
				t.Fatalf("missing response/push completion: %s", result.events)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	events, err := runClient(ctx, root, url, "")
	if err == nil || !strings.Contains(events, `"code":"invalid"`) {
		t.Fatalf("empty probe should be rejected: %v, %s", err, events)
	}
	stopNode(t, children["login"])
	events, err = runClient(ctx, root, url, "after-login-stop")
	if err == nil || !strings.Contains(events, `"code":"unavailable"`) || !strings.Contains(events, `"failed_node":"login"`) {
		t.Fatalf("stopped peer must fail observably: %v, %s", err, events)
	}
}

func startNode(t *testing.T, ctx context.Context, root, binDirectory, name, configPath string) *child {
	t.Helper()
	process := &child{command: exec.CommandContext(ctx, filepath.Join(binDirectory, name), "-config", configPath), done: make(chan error, 1)}
	process.command.Dir = root
	process.command.Stderr = &process.stderr
	stdout, err := process.command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { process.done <- process.command.Wait() }()
	t.Cleanup(func() { stopNode(t, process) })
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			ready <- fmt.Errorf("%s exited before readiness", name)
			return
		}
		var event readyEvent
		err := json.Unmarshal(scanner.Bytes(), &event)
		if err == nil && (event.Event != "ready" || event.Instance != name+"-test" || event.ListenAddress == "") {
			err = fmt.Errorf("bad readiness for %s: %s", name, scanner.Text())
		}
		if err == nil && name == "access" && event.GateAddress == "" {
			err = fmt.Errorf("missing gate readiness")
		}
		ready <- err
		for scanner.Scan() {
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return process
}

func stopNode(t *testing.T, process *child) {
	t.Helper()
	if process.stopped {
		return
	}
	process.stopped = true
	_ = process.command.Process.Signal(os.Interrupt)
	select {
	case err := <-process.done:
		if err != nil {
			t.Errorf("node exit: %v; %s", err, process.stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		<-process.done
		t.Errorf("node did not stop in budget")
	}
}

func runClient(ctx context.Context, root, url, text string) (string, error) {
	command := exec.CommandContext(ctx, "node", "typeclient/dist/index.js", "--url", url, "--text", text)
	command.Dir = root
	output, err := command.CombinedOutput()
	return string(output), err
}
