package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoginConfigKeepsDatabasePrivate 保护 login 私有配置严格解析，宿主路径仍相对配置文件目录。
func TestLoginConfigKeepsDatabasePrivate(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte("nodes:\n  login: 127.0.0.1:19002\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "login.yaml")
	base := "instance: login-01\nlisten: login\nnodes_file: nodes.yaml\nlog_directory: logs\n"
	for _, entry := range []struct {
		name, extra string
		valid       bool
	}{
		{"valid", "mysql_dsn: 'demo:example@tcp(127.0.0.1:3306)/gsrdemo_login'\n", true},
		{"missing DSN", "", false},
		{"invalid Redis", "mysql_dsn: example\nredis_url: invalid\n", false},
		{"invalid TTL", "mysql_dsn: example\ntoken_ttl_seconds: -1\n", false},
		{"large TTL", "mysql_dsn: example\ntoken_ttl_seconds: 604801\n", false},
		{"unknown field", "mysql_dsn: example\nunknown: true\n", false},
		{"duplicate field", "mysql_dsn: example\nmysql_dsn: example\n", false},
	} {
		t.Run(entry.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(base+entry.extra), 0600); err != nil {
				t.Fatal(err)
			}
			config, err := loadLoginConfig(path)
			if (err == nil) != entry.valid {
				t.Fatalf("config valid=%v err=%v", entry.valid, err)
			}
			if entry.valid && (config.Node.LogDirectory != filepath.Join(directory, "logs") || config.Node.NodesFile != filepath.Join(directory, "nodes.yaml")) {
				t.Fatal("config paths are not relative to the YAML file")
			}
		})
	}
	if _, err := loadLoginConfig(filepath.Join("..", "..", "configs", "login-01.yaml")); err == nil {
		t.Fatal("public example must not contain usable database credentials")
	}
}

// TestReadPasswordBoundsInput 保护管理密码的长度、行尾处理与空格保留，不从命令行参数传入密码。
func TestReadPasswordBoundsInput(t *testing.T) {
	for _, entry := range []struct {
		input, want string
		valid       bool
	}{
		{" password \r\n", " password ", true},
		{strings.Repeat("p", 128) + "\n", strings.Repeat("p", 128), true},
		{"example", "example", true},
		{"\n", "", false},
		{strings.Repeat("p", 129) + "\n", "", false},
	} {
		password, err := readPassword(strings.NewReader(entry.input))
		if (err == nil) != entry.valid || entry.valid && password != entry.want {
			t.Fatalf("password input length=%d accepted=%v", len(entry.input), err == nil)
		}
	}
}
