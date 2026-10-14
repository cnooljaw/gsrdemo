package main

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/mysqlclient"
	login "github.com/cnooljaw/gsrdemo/common/login"
	"github.com/go-sql-driver/mysql"
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

// TestLoginLiveMySQL 保护专用库上的账号创建、唯一性、真实 Cluster 认证和稳定身份；默认不访问数据库。
func TestLoginLiveMySQL(t *testing.T) {
	if os.Getenv("GSRDEMO_MYSQL_INTEGRATION") != "1" {
		t.Skip("set GSRDEMO_MYSQL_INTEGRATION=1 after initializing the local gsrdemo_login database")
	}
	path := filepath.Join("..", "..", "configs", "login-01.local.yaml")
	config, err := loadLoginConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := mysql.ParseDSN(config.MySQLDSN)
	if err != nil || dsn.DBName != "gsrdemo_login" || dsn.Net != "tcp" || dsn.Addr != "127.0.0.1:3306" {
		t.Fatal("live test requires the dedicated local gsrdemo_login database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := mysqlclient.Open(ctx, config.MySQLDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	username := "test_" + rand.Text()
	id, err := createAccount(ctx, db, username, "test-password")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), queryTimeout)
		defer stop()
		if _, err := db.ExecContext(cleanup, "DELETE FROM accounts WHERE user_id = ? AND username = ?", id, username); err != nil {
			t.Errorf("cleanup test account: %v", err)
		}
	})
	if _, err := createAccount(ctx, db, username, "different-password"); err == nil {
		t.Fatal("duplicate account was overwritten")
	}
	address := startLogin(t, db)
	for i := 0; i < 2; i++ {
		value, err := callLogin(t, address, username, "test-password")
		if err != nil || value != id {
			t.Fatalf("live authenticate: id=%v err=%v", value, err)
		}
	}
	if _, err := callLogin(t, address, username, "different-password"); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := callLogin(t, address, "missing_"+username, "test-password"); !errors.Is(err, login.ErrInvalidCredentials) {
		t.Fatalf("missing account: %v", err)
	}
}
