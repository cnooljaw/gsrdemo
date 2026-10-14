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
	// loginServiceName 是 login 节点内发布的认证 Service 名称。
	loginServiceName = "login"
	// authenticateCommand 接收 username、password 两个 string，成功返回 uint64 UserID。
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

// loginService 串行验证账号；数据库池由宿主拥有，不保存连接、会话或在线状态。
type loginService struct {
	db         *sql.DB
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*loginService)(nil)

func newLoginService(db *sql.DB) *loginService {
	service := &loginService{db: db}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		authenticateCommand: service.authenticate,
	})
	return service
}

func (service *loginService) OnStart(_ context.Context, _ gsr.ServiceContext) error {
	if service.db == nil {
		return errors.New("login requires a database")
	}
	return nil
}

func (service *loginService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *loginService) authenticate(self gsr.CommandContext, username, password string) (uint64, error) {
	if !validLoginInput(username, password) {
		return 0, errInvalidLoginInput
	}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	var userID uint64
	var storedHash []byte
	err := service.db.QueryRowContext(ctx, selectAccountSQL, username).Scan(&userID, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errInvalidCredentials
	}
	if err != nil {
		_ = self.Logger().Error("login database query failed", "error", err)
		return 0, errAccountStoreUnavailable
	}
	if userID == 0 || len(storedHash) != sha256.Size {
		_ = self.Logger().Error("invalid stored login account", "user_id", userID)
		return 0, errAccountStoreUnavailable
	}
	hash := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(hash[:], storedHash) != 1 {
		return 0, errInvalidCredentials
	}
	return userID, nil
}

func (service *loginService) OnStop(_ context.Context) error {
	return nil
}
