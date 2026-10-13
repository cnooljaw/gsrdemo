package model

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// NodeConfig 是节点宿主的最终配置，路由名不代表 Core ServiceID。
type NodeConfig struct {
	Instance     string            `yaml:"instance"`
	Listen       string            `yaml:"listen"`
	NodesFile    string            `yaml:"nodes_file"`
	Nodes        map[string]string `yaml:"-"`
	LogDirectory string            `yaml:"log_directory"`
	GateAddress  string            `yaml:"gate_address,omitempty"`
	EnableProbe  bool              `yaml:"enable_probe,omitempty"`
	AccountsFile string            `yaml:"accounts_file,omitempty"`
	Mole         MoleConfig        `yaml:"mole,omitempty"`
}

// MoleConfig 只配置组容量和服务端时间，不接收客户端奖励参数。
type MoleConfig struct {
	GroupSize int `yaml:"group_size"`
	RoundMS   int `yaml:"round_ms"`
	WindowMS  int `yaml:"window_ms"`
	GapMS     int `yaml:"gap_ms"`
}

// DefaultMoleConfig 是最小三人同组玩法的默认时间线。
func DefaultMoleConfig() MoleConfig {
	return MoleConfig{GroupSize: 3, RoundMS: 15000, WindowMS: 1000, GapMS: 500}
}

// LoadNodeConfig 合并节点 YAML 与 nodes_file 指定的地址表；文件路径相对节点配置目录。
func LoadNodeConfig(path, role string) (NodeConfig, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return NodeConfig{}, err
	}
	var cfg NodeConfig
	if err := ReadYAMLFile(path, &cfg); err != nil {
		return cfg, err
	}
	if cfg.NodesFile == "" {
		return cfg, errors.New("nodes_file is required")
	}
	base := filepath.Dir(path)
	cfg.NodesFile = configPath(base, cfg.NodesFile)
	var routes struct {
		Nodes map[string]string `yaml:"nodes"`
	}
	if err := ReadYAMLFile(cfg.NodesFile, &routes); err != nil {
		return cfg, err
	}
	cfg.Nodes = routes.Nodes
	if cfg.Instance == "" || cfg.LogDirectory == "" || cfg.Listen != role || cfg.Nodes[role] == "" {
		return cfg, errors.New("instance, log_directory and matching listen route are required")
	}
	for name, address := range cfg.Nodes {
		if name == "" {
			return cfg, errors.New("empty node route")
		}
		if err := validateAddress(address); err != nil {
			return cfg, fmt.Errorf("node %s: %w", name, err)
		}
	}
	if role != "access" && cfg.GateAddress != "" {
		return cfg, errors.New("only access may open Gate")
	}
	if role != "access" && cfg.EnableProbe {
		return cfg, errors.New("only access may enable diagnostic client protocols")
	}
	if role != "login" && cfg.AccountsFile != "" {
		return cfg, errors.New("only login may load accounts")
	}
	if role == "mole" {
		if cfg.Mole == (MoleConfig{}) {
			cfg.Mole = DefaultMoleConfig()
		}
		if cfg.Mole.GroupSize < 2 || cfg.Mole.GroupSize > 8 || cfg.Mole.RoundMS < 3000 || cfg.Mole.RoundMS > 60000 || cfg.Mole.WindowMS < 100 || cfg.Mole.WindowMS > 5000 || cfg.Mole.GapMS < 100 || cfg.Mole.GapMS > 5000 {
			return cfg, errors.New("invalid bounded mole configuration")
		}
	}
	if role != "mole" && cfg.Mole != (MoleConfig{}) {
		return cfg, errors.New("only mole may configure gameplay")
	}
	if role == "access" {
		if err := validateAddress(cfg.GateAddress); err != nil {
			return cfg, fmt.Errorf("gate: %w", err)
		}
		for _, target := range []string{"login", "online", "mole", "settlement"} {
			if cfg.Nodes[target] == "" {
				return cfg, fmt.Errorf("missing route %s", target)
			}
		}
	}
	cfg.LogDirectory = configPath(base, cfg.LogDirectory)
	if cfg.AccountsFile != "" {
		cfg.AccountsFile = configPath(base, cfg.AccountsFile)
	}
	return cfg, nil
}

func configPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// ReadYAMLFile 读取一个不超过 1 MiB 的 YAML 文档，拒绝未知字段、重复键和多文档。
// 节点配置、共享地址表和本地账号使用同一读取规则。
func ReadYAMLFile(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return err
	}
	if len(contents) > 1<<20 {
		return fmt.Errorf("%s exceeds 1 MiB", path)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s must contain exactly one YAML document", path)
	}
	return nil
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) || err != nil || number < 1 || number > 65535 {
		return errors.New("development address requires a loopback/private IP and port 1..65535")
	}
	return nil
}

// NodeReady 是宿主成功打开 Service/Cluster/Gate 后发布的 readiness 事件。
type NodeReady struct {
	Event         string `json:"event"`
	Instance      string `json:"instance"`
	ListenAddress string `json:"listen_address"`
	GateAddress   string `json:"gate_address,omitempty"`
}
