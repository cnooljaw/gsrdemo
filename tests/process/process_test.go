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
	"strconv"
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
		t.Skip("通过 scripts/dev.sh test 显式启用真实进程验收")
	}
	fixture := startCluster(t, false)
	ctx, root, url, children := fixture.ctx, fixture.root, fixture.url, fixture.children
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

type clusterFixture struct {
	ctx         context.Context
	root        string
	url         string
	children    map[string]*child
	clientsFile string
}

func startCluster(t *testing.T, business bool) clusterFixture {
	t.Helper()
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	accountsFile := filepath.Join(directory, "accounts.local.yaml")
	clientsFile := filepath.Join(directory, "clients.local.yaml")
	if business {
		var credentials strings.Builder
		for _, username := range []string{"player1", "player2", "player3", "player4"} {
			admin := exec.CommandContext(ctx, filepath.Join(binDirectory, "login"), "account", "-file", accountsFile, "-username", username)
			admin.Dir = root
			admin.Stdin = strings.NewReader("process-test-password\n")
			output, err := admin.CombinedOutput()
			if err != nil {
				t.Fatalf("create fixture account: %v; %s", err, output)
			}
			fmt.Fprintf(&credentials, "- username: %s\n  password: process-test-password\n", username)
		}
		if err := os.WriteFile(clientsFile, []byte(credentials.String()), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var nodesYAML strings.Builder
	nodesYAML.WriteString("nodes:\n")
	for _, name := range []string{"login", "online", "mole", "settlement", "access"} {
		fmt.Fprintf(&nodesYAML, "  %s: %s\n", name, routes[name])
	}
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte(nodesYAML.String()), 0600); err != nil {
		t.Fatal(err)
	}
	children := make(map[string]*child)
	for _, name := range []string{"login", "online", "mole", "settlement", "access"} {
		if business && name == "settlement" {
			continue
		}
		_ = reservations[name].Close()
		if name == "access" {
			_ = reservations["gate"].Close()
		}
		cfg := fmt.Sprintf("instance: %s-test\nlisten: %s\nnodes_file: nodes.yaml\nlog_directory: %s\n", name, name, strconv.Quote(filepath.Join(directory, "logs", name)))
		if name == "access" {
			cfg += fmt.Sprintf("gate_address: %s\nenable_probe: %t\n", routes["gate"], !business)
		}
		if business && name == "login" {
			cfg += "accounts_file: accounts.local.yaml\n"
		}
		if business && name == "mole" {
			cfg += "mole:\n  group_size: 3\n  round_ms: 6000\n  window_ms: 1000\n  gap_ms: 500\n"
		}
		path := filepath.Join(directory, name+".yaml")
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}
		process := startNode(t, ctx, root, binDirectory, name, path)
		children[name] = process
	}
	url := "ws://" + routes["gate"] + "/ws"
	return clusterFixture{ctx: ctx, root: root, url: url, children: children, clientsFile: clientsFile}
}

// TestMultiplayerMole 保护真实四节点登录、协议准入、三人分组、重连和全组局末冻结。
func TestMultiplayerMole(t *testing.T) {
	if os.Getenv("GSR_PROCESS_TEST") != "1" {
		t.Skip("通过 scripts/dev.sh test 显式启用真实进程验收")
	}
	fixture := startCluster(t, true)
	events, err := runClient(fixture.ctx, fixture.root, fixture.url, "disabled-probe")
	if err == nil || !strings.Contains(events, `"code":"disabled"`) {
		t.Fatalf("business Gate enabled diagnostic: %v; %s", err, events)
	}
	client := exec.CommandContext(fixture.ctx, "node", "typeclient/dist/mole.js", "--url", fixture.url, "--accounts", fixture.clientsFile)
	client.Dir = fixture.root
	output, err := client.CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"event":"complete"`) || !strings.Contains(string(output), `"participants":3`) || !strings.Contains(string(output), `"progress":"unpersisted"`) {
		t.Fatalf("multiplayer chain: %v; %s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
