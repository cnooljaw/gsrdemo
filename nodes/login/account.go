package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	login "github.com/cnooljaw/gsrdemo/common/login"
)

const (
	selectAccountSQL = "SELECT user_id, password_hash FROM accounts WHERE username = ?"
	insertAccountSQL = "INSERT INTO accounts (username, password_hash) VALUES (?, ?)"
	queryTimeout     = 2 * time.Second
)

func validLoginInput(username, password string) bool {
	if len(username) < 1 || len(username) > 64 || len(password) < 1 || len(password) > 128 {
		return false
	}
	for _, character := range username {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

// createAccount 仅供管理入口创建账号，不更新已存在的账号，也不由客户端认证触发。
// SHA-256 无盐哈希仅用于本示例，不作为生产密码存储方案。
func createAccount(ctx context.Context, db *sql.DB, username, password string) (uint64, error) {
	if !validLoginInput(username, password) {
		return 0, login.ErrInvalidRequest
	}
	hash := sha256.Sum256([]byte(password))
	wait, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	result, err := db.ExecContext(wait, insertAccountSQL, username, hash[:])
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, errors.New("invalid account ID returned by database")
	}
	return uint64(id), nil
}
