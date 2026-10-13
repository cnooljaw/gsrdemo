package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cnooljaw/gsrdemo/common/model"
	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/argon2"
)

const hashMemory = 19 * 1024
const hashIterations = 2
const hashThreads = 1

var usernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

type account struct {
	UserID     string `yaml:"user_id"`
	Username   string `yaml:"username"`
	Salt       string `yaml:"salt"`
	Hash       string `yaml:"hash"`
	Memory     uint32 `yaml:"memory"`
	Iterations uint32 `yaml:"iterations"`
	Threads    uint8  `yaml:"threads"`
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func loadAccounts(path string) (map[string]account, error) {
	accounts := make(map[string]account)
	if path == "" {
		return accounts, nil
	}
	var records []account
	if err := model.ReadYAMLFile(path, &records); err != nil {
		return nil, err
	}
	if len(records) > 512 {
		return nil, errors.New("account capacity exceeded")
	}
	users := make(map[string]bool)
	for _, record := range records {
		salt, saltErr := base64.RawStdEncoding.DecodeString(record.Salt)
		hash, hashErr := base64.RawStdEncoding.DecodeString(record.Hash)
		if !usernamePattern.MatchString(record.Username) || record.UserID == "" || users[record.UserID] || len(salt) != 16 || len(hash) != 32 || saltErr != nil || hashErr != nil || record.Memory != hashMemory || record.Iterations != hashIterations || record.Threads != hashThreads {
			return nil, errors.New("invalid account or hash parameters")
		}
		if _, exists := accounts[record.Username]; exists {
			return nil, errors.New("duplicate username")
		}
		users[record.UserID] = true
		accounts[record.Username] = record
	}
	return accounts, nil
}

func authenticate(accounts map[string]account, username, password string) string {
	username = normalizeUsername(username)
	if !usernamePattern.MatchString(username) || len(password) < 8 || len(password) > 128 {
		return ""
	}
	record, exists := accounts[username]
	// 未知账号仍计算同成本哈希，且响应不区分用户名和密码错误。
	salt := make([]byte, 16)
	expected := make([]byte, 32)
	if exists {
		salt, _ = base64.RawStdEncoding.DecodeString(record.Salt)
		expected, _ = base64.RawStdEncoding.DecodeString(record.Hash)
	}
	hash := argon2.IDKey([]byte(password), salt, hashIterations, hashMemory, hashThreads, 32)
	if subtle.ConstantTimeCompare(hash, expected) != 1 || !exists {
		return ""
	}
	return record.UserID
}

// CreateAccount 是离线管理入口；只保存带随机盐的 Argon2id 哈希，不输出密码或哈希。
func CreateAccount(path, username, password string) error {
	username = normalizeUsername(username)
	if !strings.HasSuffix(path, ".local.yaml") || !usernamePattern.MatchString(username) || len(password) < 8 || len(password) > 128 {
		return errors.New("requires .local.yaml, username 3..32 and password 8..128 bytes")
	}
	accounts, err := loadAccounts(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if accounts == nil {
		accounts = make(map[string]account)
	}
	if _, exists := accounts[username]; exists {
		return errors.New("username already exists")
	}
	if len(accounts) >= 512 {
		return errors.New("account capacity exceeded")
	}
	userID, err := model.NewID()
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash := argon2.IDKey([]byte(password), salt, hashIterations, hashMemory, hashThreads, 32)
	records := make([]account, 0, len(accounts)+1)
	for _, record := range accounts {
		records = append(records, record)
	}
	records = append(records, account{UserID: userID, Username: username, Salt: base64.RawStdEncoding.EncodeToString(salt), Hash: base64.RawStdEncoding.EncodeToString(hash), Memory: hashMemory, Iterations: hashIterations, Threads: hashThreads})
	file, err := os.CreateTemp(filepath.Dir(path), "accounts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	encoder := yaml.NewEncoder(file)
	encoder.SetIndent(2)
	writeErr := encoder.Encode(records)
	if err := errors.Join(writeErr, encoder.Close(), file.Close()); err != nil {
		return fmt.Errorf("save accounts: %w", err)
	}
	return os.Rename(file.Name(), path)
}
