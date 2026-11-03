package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSettlementConfigRequiresPrivateDatabase 保护数据库配置严格解析，公开模板不携带可用凭据。
func TestSettlementConfigRequiresPrivateDatabase(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte("nodes:\n  settlement: 127.0.0.1:19005\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "settlement.yaml")
	base := "instance: settlement-01\nlisten: settlement\nnodes_file: nodes.yaml\nlog_directory: logs\n"
	for _, entry := range []struct {
		extra string
		valid bool
	}{{"mysql_dsn: example\n", true}, {"", false}, {"mysql_dsn: example\nunknown: true\n", false}} {
		if err := os.WriteFile(path, []byte(base+entry.extra), 0600); err != nil {
			t.Fatal(err)
		}
		config, err := loadSettlementConfig(path)
		if (err == nil) != entry.valid {
			t.Fatalf("valid=%v err=%v", entry.valid, err)
		}
		if entry.valid && config.Node.LogDirectory != filepath.Join(directory, "logs") {
			t.Fatal("relative path changed")
		}
	}
	if _, err := loadSettlementConfig("../../configs/settlement-01.yaml"); err == nil {
		t.Fatal("public template contains usable credentials")
	}
}

// TestSettlementConfigSupportsAnotherNode 保护同一节点项目可通过独立配置启动第二个结算实例。
func TestSettlementConfigSupportsAnotherNode(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "nodes.yaml"), []byte("nodes:\n  settlement-02: 127.0.0.1:29005\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "settlement.yaml")
	body := "instance: settlement-02\nlisten: settlement-02\nnodes_file: nodes.yaml\nlog_directory: logs\nmysql_dsn: example\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadSettlementConfig(path)
	if err != nil || config.Node.Listen != "settlement-02" {
		t.Fatalf("独立实例配置无效: %+v %v", config, err)
	}
}
