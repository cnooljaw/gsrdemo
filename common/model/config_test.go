package model

import (
	"os"
	"path/filepath"
	"testing"
)

func configFiles(t *testing.T, node, routes string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "login.yaml")
	for name, content := range map[string]string{"login.yaml": node, "nodes.yaml": routes} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

const loginConfig = "instance: login-01\nlisten: login\nlog_directory: logs/login-01\nnodes_file: nodes.yaml\n"
const loginRoutes = "nodes:\n  login: 127.0.0.1:19002\n"

// TestLoadNodeConfigUsesSharedRoutesAndRelativePaths 保护节点地址只从共享文件读取，路径与启动工作目录无关。
func TestLoadNodeConfigUsesSharedRoutesAndRelativePaths(t *testing.T) {
	path := configFiles(t, loginConfig, loginRoutes)
	cfg, err := LoadNodeConfig(path, "login")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	if cfg.Nodes["login"] != "127.0.0.1:19002" || cfg.LogDirectory != filepath.Join(base, "logs/login-01") || cfg.NodesFile != filepath.Join(base, "nodes.yaml") {
		t.Fatalf("shared config or relative paths lost: %+v", cfg)
	}
	if err := os.WriteFile(filepath.Join(base, "nodes.yaml"), []byte("nodes:\n  login: 127.0.0.1:29002\n"), 0600); err != nil {
		t.Fatal(err)
	}
	updated, err := LoadNodeConfig(path, "login")
	if err != nil || updated.Nodes["login"] != "127.0.0.1:29002" {
		t.Fatal("shared route update not loaded", err)
	}
}

// TestLoadNodeConfigRejectsUnsafeOrAmbiguousInput 保护单文档、严格字段、共享文件与合法路由边界。
func TestLoadNodeConfigRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	cases := []struct{ name, node, routes string }{
		{"unknown node field", loginConfig + "unknown: true\n", loginRoutes},
		{"inline nodes", loginConfig + "nodes:\n  login: 127.0.0.1:19002\n", loginRoutes},
		{"extra node document", loginConfig + "---\n{}\n", loginRoutes},
		{"unknown routes field", loginConfig, loginRoutes + "unknown: true\n"},
		{"extra routes document", loginConfig, loginRoutes + "---\n{}\n"},
		{"duplicate route", loginConfig, loginRoutes + "  login: 127.0.0.1:29002\n"},
		{"unsafe address", loginConfig, "nodes:\n  login: 0.0.0.0:19002\n"},
		{"missing role route", loginConfig, "nodes:\n  online: 127.0.0.1:19003\n"},
		{"missing shared file", "instance: login\nlisten: login\nlog_directory: logs\nnodes_file: missing.yaml\n", loginRoutes},
		{"wrong role", "instance: login\nlisten: online\nlog_directory: logs\nnodes_file: nodes.yaml\n", loginRoutes},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := configFiles(t, test.node, test.routes)
			if _, err := LoadNodeConfig(path, "login"); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

// TestExampleHostConfigsShareOneTable 保护各实例的宿主字段共用地址表；私有字段的严格解析由各节点测试负责。
func TestExampleHostConfigsShareOneTable(t *testing.T) {
	table, err := filepath.Abs(filepath.Join("..", "..", "configs", "nodes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for instance, route := range map[string]string{
		"access-01": "access-01", "access-02": "access-02", "login-01": "login",
		"online-01": "online", "mole-01": "mole", "settlement-01": "settlement",
	} {
		var config struct {
			Node    NodeConfig     `yaml:",inline"`
			Private map[string]any `yaml:",inline"`
		}
		path := filepath.Join("..", "..", "configs", instance+".yaml")
		if err := ReadYAMLFile(path, &config); err != nil {
			t.Fatalf("%s: %v", instance, err)
		}
		cfg, err := ResolveNodeConfig(config.Node, path, route)
		if err != nil {
			t.Fatalf("%s: %v", instance, err)
		}
		if cfg.NodesFile != table || len(cfg.Nodes) != 6 || cfg.Nodes["access-02"] != "127.0.0.1:19006" {
			t.Fatal("example config uses another table", instance)
		}
	}
}
