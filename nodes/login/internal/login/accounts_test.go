package login

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAccountsVerifyPasswordWithoutExposingHash 保护测试账号的随机盐哈希、错误密码和用户名规范化。
func TestAccountsVerifyPasswordWithoutExposingHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.local.json")
	if err := CreateAccount(path, "Alice", "correct-password"); err != nil {
		t.Fatal(err)
	}
	accounts, err := loadAccounts(path)
	if err != nil {
		t.Fatal(err)
	}
	userID := authenticate(accounts, "alice", "correct-password")
	if userID == "" || authenticate(accounts, "alice", "wrong-password") != "" || authenticate(accounts, "unknown", "correct-password") != "" {
		t.Fatal("password verification")
	}
	if err := CreateAccount(path, "alice", "another-password"); err == nil {
		t.Fatal("duplicate username")
	}
	if err := CreateAccount(path, "bob", "correct-password"); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAccounts(path)
	if err != nil || loaded["bob"].UserID == userID || loaded["bob"].Salt == loaded["alice"].Salt {
		t.Fatal("reused account identity or salt")
	}
}

// TestAccountStoreRejectsContentBeyondReadLimit 保护账号文件不能因为读取上限而忽略后面的多余对象。
func TestAccountStoreRejectsContentBeyondReadLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.local.json")
	content := "[]" + strings.Repeat(" ", 1<<20) + "[]"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAccounts(path); err == nil {
		t.Fatal("extra object beyond read limit accepted")
	}
}
