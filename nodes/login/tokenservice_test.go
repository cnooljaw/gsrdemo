package main

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr/runtime"
)

// TestTokenServiceSurvivesAuthRestart 保护认证返回凭据，Token 远程校验/撤销不查 SQL，相同 Redis 在节点重建后仍可校验。
func TestTokenServiceSurvivesAuthRestart(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := tokenStore{client: startTokenRedis(t), prefix: "test:restart"}
	address, stop := startLoginNode(t, db, store)
	hash := sha256.Sum256([]byte("password"))
	mock.ExpectQuery(selectAccountSQL).WithArgs("alice").WillReturnRows(sqlmock.NewRows([]string{"user_id", "password_hash"}).AddRow(7, hash[:]))
	value, err := callLogin(t, address, "alice", "password")
	if err != nil || !validAuthResult(value, 7) {
		t.Fatal("未返回平台凭据", err)
	}
	token := value.(map[string]any)["access_token"].(string)
	// 停掉旧 Runtime 后再创建新节点，Redis 保留，SQL 不再设置查询期望。
	stop()
	rebuilt := startLoginWithStore(t, db, store)
	for _, target := range []string{rebuilt} {
		uid, err := callLoginCommand(t, target, tokenServiceName, tokenVerifyCommand, token)
		if err != nil || uid != uint64(7) {
			t.Fatalf("重建校验: uid=%v err=%v", uid, err)
		}
	}
	if _, err := callLoginCommand(t, rebuilt, tokenServiceName, tokenVerifyCommand, uint64(7)); !errors.Is(err, runtime.ErrInvalidCommandArguments) {
		t.Fatal("错误参数未拒绝", err)
	}
	if _, err := callLoginCommand(t, rebuilt, tokenServiceName, 99); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatal("未知命令未拒绝", err)
	}
	if removed, err := callLoginCommand(t, rebuilt, tokenServiceName, tokenRevokeCommand, token); err != nil || removed != true {
		t.Fatal("撤销失败", err)
	}
	if _, err := callLoginCommand(t, rebuilt, tokenServiceName, tokenVerifyCommand, token); !errors.Is(err, errInvalidToken) {
		t.Fatal("撤销后仍可登录", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestSlowPasswordAuthDoesNotBlockTokenLogin 保护密码 SQL 等待不阻塞独立 Token Service 的登录校验。
func TestSlowPasswordAuthDoesNotBlockTokenLogin(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	address := startLogin(t, db)
	issued, err := callLoginCommand(t, address, tokenServiceName, tokenIssueCommand, uint64(7))
	if err != nil {
		t.Fatal(err)
	}
	token := issued.(map[string]any)["access_token"].(string)
	hash := sha256.Sum256([]byte("password"))
	mock.ExpectQuery(selectAccountSQL).WithArgs("alice").WillDelayFor(300 * time.Millisecond).WillReturnRows(sqlmock.NewRows([]string{"user_id", "password_hash"}).AddRow(7, hash[:]))
	done := make(chan error, 1)
	go func() { _, err := callLogin(t, address, "alice", "password"); done <- err }()
	time.Sleep(30 * time.Millisecond)
	started := time.Now()
	uid, err := callLoginCommand(t, address, tokenServiceName, tokenVerifyCommand, token)
	if err != nil || uid != uint64(7) || time.Since(started) > 200*time.Millisecond {
		t.Fatalf("Token 校验被 SQL 阻塞: uid=%v err=%v", uid, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
