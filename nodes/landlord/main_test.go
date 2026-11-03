package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLandlordConfig 保护本节点独立 Redis 配置、共享节点表及无 SQL 配置边界。
func TestLandlordConfig(t *testing.T) {
	config, err := loadLandlordConfig("../../configs/landlord-01.yaml")
	if err != nil || config.Node.Listen != "landlord" || config.RedisKey != "gsrdemo:landlord:pending" {
		t.Fatalf("config: %+v %v", config, err)
	}
	directory := t.TempDir()
	nodes := filepath.Join(directory, "nodes.yaml")
	if err := os.WriteFile(nodes, []byte("nodes:\n  landlord: 127.0.0.1:19007\n  online: 127.0.0.1:19003\n  settlement: 127.0.0.1:19005\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body := "instance: landlord-01\nlisten: landlord\nnodes_file: nodes.yaml\nlog_directory: logs\nredis_url: redis://127.0.0.1:6379/0\nredis_key: test:landlord\n"
	for _, invalid := range []string{body + "mysql_dsn: forbidden\n", strings.Replace(body, "redis_key: test:landlord", "redis_key: ''", 1), strings.Replace(body, "redis://127.0.0.1:6379/0", "not-a-redis-url", 1)} {
		path := filepath.Join(directory, "landlord.yaml")
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadLandlordConfig(path); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
