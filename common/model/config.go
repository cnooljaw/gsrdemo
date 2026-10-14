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
}

// LoadNodeConfig 合并节点 YAML 与 nodes_file 指定的地址表；文件路径相对节点配置目录。
func LoadNodeConfig(path, role string) (NodeConfig, error) {
	if path == "" {
		return NodeConfig{}, errors.New("config path is required")
	}
	var cfg NodeConfig
	if err := ReadYAMLFile(path, &cfg); err != nil {
		return cfg, err
	}
	return ResolveNodeConfig(cfg, path, role)
}

// ResolveNodeConfig 从节点配置所在目录读取地址表并校验宿主字段，供节点私有配置复用。
// 返回新的地址表，不保留输入中的 Nodes；角色私有字段由节点自己的类型解析。
func ResolveNodeConfig(cfg NodeConfig, path, role string) (NodeConfig, error) {
	if path == "" {
		return cfg, errors.New("config path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
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
	cfg.LogDirectory = configPath(base, cfg.LogDirectory)
	return cfg, nil
}

func configPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// ReadYAMLFile 将一个不超过 1 MiB 的 YAML 文档读入调用方类型，拒绝未知字段、重复键和多文档。
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
