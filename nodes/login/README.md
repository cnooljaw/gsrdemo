# Login · 认证与 Token

密码认证验证身份并签发平台 Token。Access 在后续 Token 登录时校验凭据，再向 Online 绑定连接。

- [authservice.go](authservice.go) 校验密码，通过本地 Call 让 TokenService 签发凭据。[account_mysql.go](account_mysql.go) 读取 MySQL 账号。
- [tokenservice.go](tokenservice.go) 提供签发、校验和撤销。它与 AuthService 使用独立邮箱，Token 校验无需等待密码查询。
- [token_redis.go](token_redis.go) 生成随机 Token，Redis 以 Token 摘要为键保存 UserID 和到期时间。校验不延长有效期。

[认证与登录设计](../../docs/DESIGN.md#32-密码认证与-token-登录) · [运行方式](../../README.md#运行-demo)
