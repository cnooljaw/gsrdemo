package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMoleConfigRequiresRedisAndRejectsSQL 保护 mole 要求 Redis 检查点配置，拒绝遗留 mysql_dsn。
func TestMoleConfigRequiresRedisAndRejectsSQL(t *testing.T) {
	directory := t.TempDir()
	routes := filepath.Join(directory, "nodes.yaml")
	if err := os.WriteFile(routes, []byte("nodes:\n  mole: 127.0.0.1:7104\n  online: 127.0.0.1:7103\n  settlement: 127.0.0.1:7106\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mole.yaml")
	base := "instance: mole-01\nlisten: mole\nnodes_file: nodes.yaml\nlog_directory: logs\nredis_url: redis://127.0.0.1:6379/0\nredis_key: test:mole\n"
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadMoleConfig(path)
	if err != nil || config.Node.LogDirectory != filepath.Join(directory, "logs") {
		t.Fatalf("plain node config: %+v %v", config, err)
	}
	if err := os.WriteFile(path, []byte(base+"mysql_dsn: example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMoleConfig(path); err == nil {
		t.Fatal("mole accepted database settings")
	}
	for _, body := range []string{
		"instance: mole-01\nlisten: mole\nnodes_file: nodes.yaml\nlog_directory: logs\n",
		base + "redis_key: duplicate\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadMoleConfig(path); err == nil {
			t.Fatal("invalid checkpoint configuration accepted")
		}
	}

}

// TestMoleSettlementNodeConfig 保护业务可以仅配置非默认结算节点，缺失目标时明确拒绝启动。
func TestMoleSettlementNodeConfig(t *testing.T) {
	directory := t.TempDir()
	nodes := "nodes:\n  mole: 127.0.0.1:19004\n  online: 127.0.0.1:19003\n  settlement-02: 127.0.0.1:29005\n"
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte(nodes), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "mole.yaml")
	base := "instance: mole-01\nlisten: mole\nnodes_file: nodes.yaml\nlog_directory: logs\nredis_url: redis://127.0.0.1:6379/0\nredis_key: test:mole\n"
	for _, target := range []string{"settlement-02", "missing"} {
		if err := os.WriteFile(path, []byte(base+"settlement_node: "+target+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		config, err := loadMoleConfig(path)
		if target == "settlement-02" {
			if err != nil || config.SettlementNode != target {
				t.Fatalf("目标未保留: %+v %v", config, err)
			}
		} else if err == nil {
			t.Fatal("缺失目标被静默回退")
		}
	}
}
