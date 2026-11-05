package main

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/mysqlclient"
	"github.com/go-sql-driver/mysql"
)

// TestPasswordAuthLiveMySQL 保护专用库上的账号创建、唯一性、真实 Cluster 认证和稳定身份；默认不访问数据库。
func TestPasswordAuthLiveMySQL(t *testing.T) {
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
		if err != nil || !validAuthResult(value, id) {
			t.Fatalf("live authenticate: id=%v err=%v", value, err)
		}
	}
	if _, err := callLogin(t, address, username, "different-password"); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := callLogin(t, address, "missing_"+username, "test-password"); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("missing account: %v", err)
	}
}
