# 内部业务协议

## login 认证

仅供受信任节点经 GSR Cluster 调用；当前 access 和 TypeScript 尚未接入客户端登录协议。

| 项目 | 约定 |
| --- | --- |
| 目标 | Node `login`，Service `login` |
| CommandID | `1`（loginservice.go 的 authenticateCommand） |
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

当前命令、Service 名称及错误定义与 LoginService 放在 loginservice.go；真实跨节点调用方接入时，再按实际需要提取共享契约。

## online 位置目录

仅供受信任节点经 Cluster 调用，目标为 Node `online`、Service `online`。当前 access、login 和 TypeScript 的业务尚未调用 Online；命令、名称和错误定义与 Service 放在 onlineservice.go。

| CommandID | 位置参数 | 成功结果 |
| --- | --- | --- |
| `1` Bind | `userID uint64, loginID string, accessNode string, agentName string` | 被替换的位置快照；首次绑定或当前连接重复提交返回 `nil` |
| `2` Lookup | `userID uint64` | 当前的位置快照；没有记录返回 `nil` |
| `3` Unbind | `userID uint64, loginID string` | 删除匹配记录返回 `true`；记录不存在或 LoginID 不匹配返回 `false` |

位置快照为独立 `map[string]any`，包含三个 string 字段：`login_id`、`access_node`、`agent_name`。不存在记录时返回真正的 `nil`，不借用 nil/空 map 的编码差异。UserID 必须非零；三个标识均为 1..128 字节的可见 ASCII（0x21..0x7e），不裁剪、不转换大小写。

`access_node` 是可拨号的 Cluster 节点路由名，不是实例日志标签；将来多个 access 各配置不同路由名，例如 `access-01`、`access-02`，在 configs/nodes.yaml 集中维护。`agent_name` 只在其接入节点内唯一，不要求不同 access 的 Agent 名称互不重复。Online 不验证节点是否可达、不持有连接、不代替账号认证；跨节点 Source 不是身份凭据，调用方负责完成登录认证并提供有效地址。

- LoginID 是业务登录关联标识，不是 Core/Cluster Session 或 Runtime epoch。接入端每次新登录生成新的不可复用 LoginID。
- 不同 LoginID 的 Bind 按 Online 实际串行处理顺序替换，最后处理的绑定生效，不按客户端开始登录的时间排序。返回的旧地址供调用方后续处理旧连接，Online 不等待踢人 RPC，也不承诺旧连接已经停止。
- 当前 LoginID 和地址均相同的 Bind 返回 `nil`，避免调用方将自身当作旧连接踢除。当前 LoginID 相同但 AccessNode 或 AgentName 不同，返回 `online.login_id_conflict`，不修改目录。
- Unbind 仅删除自己的 LoginID，旧 access 的迟到断线通知不能删除新 access 的绑定；它不按整个节点批量删除用户。
- Bind 超时可能已经执行。调用方可 Lookup 当前记录，但查询只是当时的快照，不能将一次未命中解释为远端绝未执行，不能盲目重发 Bind。Online 不保留历史 LoginID 或删除墓碑；已被替换或解绑的 LoginID 禁止再次用于 Bind，当前绑定的重复提交规则不等于永久幂等承诺。
- Lookup 可重复查询；同一 LoginID 的 Unbind 可以安全重复。旧连接通知与游戏命令的身份校验在后续 access/房间接入中实现，目录替换本身不构成对旧连接的强制撤权。
- 接入突然退出可能遗留旧位置，后续新登录覆盖；Online 重启后目录为空，需要后续接入业务重新绑定。Lookup 命中不保证客户端此刻仍然在线。内存目录不持久化，也不提供房间、积分或心跳状态。

Domain 错误 `online.invalid_request` 表示字段越界或非法，`online.login_id_conflict` 表示当前 LoginID 地址冲突；非法操作不修改状态。参数数量/类型和未知命令沿用 GSR 的 `runtime.invalid_command_arguments` / `runtime.unknown_command`。调用方设置有限 Call 预算；Send 成功不代表目录已经修改。
