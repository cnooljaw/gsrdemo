package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr/runtime"
)

// TestPasswordAuthOverCluster 保护真实远程接线、稳定 UserID、错误凭据、输入拒绝和安全数据库错误。
func TestPasswordAuthOverCluster(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	address := startLogin(t, db)
	hash := sha256.Sum256([]byte("example-password"))
	for _, password := range []string{"example-password", "example-password", "wrong"} {
		mock.ExpectQuery(selectAccountSQL).WithArgs("alice").WillReturnRows(
			sqlmock.NewRows([]string{"user_id", "password_hash"}).AddRow(7, hash[:]))
		value, err := callLogin(t, address, "alice", password)
		if password == "wrong" {
			if !errors.Is(err, errInvalidCredentials) {
				t.Fatalf("wrong password: %v", err)
			}
		} else if err != nil || !validAuthResult(value, 7) {
			t.Fatalf("authenticate: value=%v err=%v", value, err)
		}
	}
	mock.ExpectQuery(selectAccountSQL).WithArgs("missing").WillReturnRows(sqlmock.NewRows([]string{"user_id", "password_hash"}))
	if _, err := callLogin(t, address, "missing", "example-password"); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("missing account: %v", err)
	}
	for _, args := range [][]any{{"", "password"}, {"alice", ""}, {" alice", "password"}, {"alice", strings.Repeat("p", 129)}} {
		if _, err := callLogin(t, address, args...); !errors.Is(err, errInvalidLoginInput) {
			t.Fatalf("invalid input: %v", err)
		}
	}
	if _, err := callLogin(t, address, uint64(7), "password"); !errors.Is(err, runtime.ErrInvalidCommandArguments) {
		t.Fatalf("wrong wire argument: %v", err)
	}
	mock.ExpectQuery(selectAccountSQL).WithArgs("alice").WillReturnError(fmt.Errorf("private SQL error"))
	if _, err := callLogin(t, address, "alice", "password"); !errors.Is(err, errAccountStoreUnavailable) || strings.Contains(err.Error(), "private SQL") {
		t.Fatalf("database error: %v", err)
	}
	mock.ExpectQuery(selectAccountSQL).WithArgs("broken").WillReturnRows(
		sqlmock.NewRows([]string{"user_id", "password_hash"}).AddRow(7, []byte{1}))
	if _, err := callLogin(t, address, "broken", "password"); !errors.Is(err, errAccountStoreUnavailable) {
		t.Fatalf("corrupt account: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
