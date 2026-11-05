package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCreateAccountStoresHash 保护管理入口只存密码摘要，重复账号不会覆盖原账号。
func TestCreateAccountStoresHash(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash := sha256.Sum256([]byte("example-password"))
	mock.ExpectExec(insertAccountSQL).WithArgs("alice", hash[:]).WillReturnResult(sqlmock.NewResult(7, 1))
	id, err := createAccount(context.Background(), db, "alice", "example-password")
	if err != nil || id != 7 {
		t.Fatalf("create: id=%d err=%v", id, err)
	}
	mock.ExpectExec(insertAccountSQL).WithArgs("alice", hash[:]).WillReturnError(errors.New("duplicate account"))
	if _, err := createAccount(context.Background(), db, "alice", "example-password"); err == nil {
		t.Fatal("duplicate insert reported success")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
