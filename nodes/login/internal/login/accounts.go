package login

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cnooljaw/gsrdemo/common/model"
	"golang.org/x/crypto/argon2"
)

const hashMemory = 19 * 1024
const hashIterations = 2
const hashThreads = 1

var usernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

type account struct {
	UserID     string `json:"user_id"`
	Username   string `json:"username"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Memory     uint32 `json:"memory"`
	Iterations uint32 `json:"iterations"`
	Threads    uint8  `json:"threads"`
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func loadAccounts(path string) (map[string]account, error) {
	accounts := make(map[string]account)
	if path == "" {
		return accounts, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 1<<20 {
		return nil, errors.New("account file exceeds 1 MiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var records []account
	if err := decoder.Decode(&records); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("accounts must contain one JSON array")
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
	if !strings.HasSuffix(path, ".local.json") || !usernamePattern.MatchString(username) || len(password) < 8 || len(password) > 128 {
		return errors.New("requires .local.json, username 3..32 and password 8..128 bytes")
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
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(records)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("save accounts: %w", err)
	}
	return os.Rename(file.Name(), path)
}
