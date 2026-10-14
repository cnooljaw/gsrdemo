// Package login 定义 login 节点的内部认证命令；不包含节点实现或客户端协议。
package login

import "github.com/cnooljaw/gsr/runtime"

const (
	// ServiceName 是 login 节点内发布的认证 Service 名称。
	ServiceName = "login"
	// AuthenticateCommand 接收 username、password 两个 string，成功返回 uint64 UserID。
	AuthenticateCommand runtime.CommandID = 1
)

var (
	// ErrInvalidRequest 表示用户名或密码不满足示例输入边界。
	ErrInvalidRequest = runtime.NewStatusError(runtime.ErrorClassDomain, "login.invalid_request", "invalid login input")
	// ErrInvalidCredentials 统一表示账号不存在或密码错误，不区分两者。
	ErrInvalidCredentials = runtime.NewStatusError(runtime.ErrorClassDomain, "login.invalid_credentials", "invalid credentials")
	// ErrStoreUnavailable 表示数据库不可用或持久化账号数据损坏。
	ErrStoreUnavailable = runtime.NewStatusError(runtime.ErrorClassDomain, "login.store_unavailable", "account storage is unavailable")
)
