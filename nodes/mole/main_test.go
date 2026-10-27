package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMoleConfigHasNoDatabaseSettings 保护 mole 仅使用节点配置，拒绝遗留 mysql_dsn，不要求数据库凭据。
func TestMoleConfigHasNoDatabaseSettings(t *testing.T) {
	directory := t.TempDir()
	routes := filepath.Join(directory, "nodes.yaml")
	if err := os.WriteFile(routes, []byte("nodes:\n  mole: 127.0.0.1:7104\n  online: 127.0.0.1:7103\n  settlement: 127.0.0.1:7106\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mole.yaml")
	base := "instance: mole-01\nlisten: mole\nnodes_file: nodes.yaml\nlog_directory: logs\n"
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadMoleConfig(path)
	if err != nil || config.LogDirectory != filepath.Join(directory, "logs") {
		t.Fatalf("plain node config: %+v %v", config, err)
	}
	if err := os.WriteFile(path, []byte(base+"mysql_dsn: example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMoleConfig(path); err == nil {
		t.Fatal("mole accepted database settings")
	}
}
