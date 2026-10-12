package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

// NodeConfig 是节点宿主的最终配置，路由名不代表 Core ServiceID。
type NodeConfig struct {
	Instance     string            `json:"instance"`
	Listen       string            `json:"listen"`
	Nodes        map[string]string `json:"nodes"`
	LogDirectory string            `json:"log_directory"`
	GateAddress  string            `json:"gate_address,omitempty"`
	EnableProbe  bool              `json:"enable_probe,omitempty"`
	AccountsFile string            `json:"accounts_file,omitempty"`
	Mole         MoleConfig        `json:"mole,omitempty"`
}

// MoleConfig 只配置组容量和服务端时间，不接收客户端奖励参数。
type MoleConfig struct {
	GroupSize int `json:"group_size"`
	RoundMS   int `json:"round_ms"`
	WindowMS  int `json:"window_ms"`
	GapMS     int `json:"gap_ms"`
}

// DefaultMoleConfig 是最小三人同组玩法的默认时间线。
func DefaultMoleConfig() MoleConfig {
	return MoleConfig{GroupSize: 3, RoundMS: 15000, WindowMS: 1000, GapMS: 500}
}

// LoadNodeConfig 读取并校验单个 JSON 配置文件；路径按进程工作目录解释。
func LoadNodeConfig(path, role string) (NodeConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return NodeConfig{}, err
	}
	cfg, decodeErr := DecodeNodeConfig(file, role)
	return cfg, errors.Join(decodeErr, file.Close())
}

// DecodeNodeConfig 拒绝未知字段、额外对象和非本地/私网开发路由。
func DecodeNodeConfig(reader io.Reader, role string) (NodeConfig, error) {
	var cfg NodeConfig
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cfg, errors.New("config must contain exactly one JSON object")
	}
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
	return cfg, nil
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
