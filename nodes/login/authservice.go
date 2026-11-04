package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	// authServiceName 是 login 节点内发布的认证 Service 名称。
	authServiceName = "auth"
	// authenticateCommand 接收 username、password 两个 string，成功返回 UserID、AccessToken 和过期时间。
	authenticateCommand runtime.CommandID = 1
)

var (
	// errInvalidLoginInput 表示用户名或密码不满足示例输入边界。
	errInvalidLoginInput = runtime.NewStatusError(runtime.ErrorClassDomain, "login.invalid_request", "invalid login input")
	// errInvalidCredentials 统一表示账号不存在或密码错误，不区分两者。
	errInvalidCredentials = runtime.NewStatusError(runtime.ErrorClassDomain, "login.invalid_credentials", "invalid credentials")
	// errAccountStoreUnavailable 表示数据库不可用或持久化账号数据损坏。
	errAccountStoreUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "login.store_unavailable", "account storage is unavailable")
)

// authService 验证密码并签发平台凭据；数据库池由宿主拥有，不保存连接和在线状态。
type authService struct {
	db         *sql.DB
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*authService)(nil)

func newAuthService(db *sql.DB) *authService {
	service := &authService{db: db}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		authenticateCommand: service.handleAuthenticate,
	})
	return service
}

func (service *authService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	if service.db == nil {
		return errors.New("login requires a database")
	}
	return nil
}

func (service *authService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *authService) handleAuthenticate(self gsr.CommandContext, username, password string) (map[string]any, error) {
	if !validLoginInput(username, password) {
		return nil, errInvalidLoginInput
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var userID uint64
	var storedHash []byte
	err := service.db.QueryRowContext(ctx, selectAccountSQL, username).Scan(&userID, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errInvalidCredentials
	}
	if err != nil {
		_ = self.Logger().Error("login database query failed", "error", err)
		return nil, errAccountStoreUnavailable
	}
	if userID == 0 || len(storedHash) != sha256.Size {
		_ = self.Logger().Error("invalid stored login account", "user_id", userID)
		return nil, errAccountStoreUnavailable
	}
	hash := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(hash[:], storedHash) != 1 {
		return nil, errInvalidCredentials
	}
	wait, stop := context.WithTimeout(context.Background(), tokenCallTimeout)
	defer stop()
	value, err := self.CallName(wait, tokenServiceName, tokenIssueCommand, userID)
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errTokenStoreUnavailable
	}
	return result, nil
}

func (service *authService) OnStop(_ context.Context) error {
	return nil
}
