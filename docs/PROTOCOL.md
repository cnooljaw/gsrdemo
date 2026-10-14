# 内部业务协议

## login 认证

仅供受信任节点经 GSR Cluster 调用；当前 access 和 TypeScript 尚未接入客户端登录协议。

| 项目 | 约定 |
| --- | --- |
| 目标 | Node `login`，Service `login` |
| CommandID | `1`（common/login.AuthenticateCommand） |
| 位置参数 | `username string`、`password string` |
| 成功结果 | 非零 `uint64` UserID；同账号重复认证返回相同 ID |

用户名 1..64 字节，仅允许 ASCII 字母、数字、`_`、`-`、`.`，区分大小写，不做 trim 或大小写转换。密码 1..128 字节，按原始字节哈希；管理命令只去掉输入行末的 CR/LF。用户名唯一，不自动注册。

| Domain 错误码 | 含义 |
| --- | --- |
| login.invalid_request | 用户名或密码超出输入边界 |
| login.invalid_credentials | 账号不存在或密码错误 |
| login.store_unavailable | 数据库不可用，或账号 ID/哈希数据损坏 |

调用方用 `errors.Is` 或 Class/Code 判断，不比较 Message。错误类型/参数个数由 GSR Dispatcher 返回 `runtime.invalid_command_arguments`；未知 Command 沿用框架错误。底层 SQL 错误只进入服务端日志，不作为 RPC 文本返回。SQL 等待上限为 2 秒，调用方另设有限 Call 预算。

认证只读账号，不创建登录会话、Token 或 Online 记录；Call 超时可以重试本次只读验证，不代表登录位置已经绑定。内部直接使用规范参数类型，不增加内部 Protobuf 编解码。

本示例按用户确认采用无盐 SHA-256，账号表保存 32 字节摘要，仅用于示例，不是生产密码存储方案。
