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

## mole 与银子结算草案（未实现）

本节仅记录实现前契约，不代表节点已有这些命令；CommandID 和具体参数在对应节点实现时固定。完整职责、可靠性边界和实施顺序见 [DESIGN.md 第 8、9 节](DESIGN.md#8-mole-实现方案草案尚未实现)。

### mole 调用链

| 入口 | 参数/作用 | 返回/通知 |
| --- | --- | --- |
| MoleService.Enter | UserID、LoginID、AccessNode、AgentName；分组或重连，确保业务记账已加载余额缓存 | AttackID、AttackName、座位、快照；余额加载失败不允许银子操作 |
| AttackService.Hit | UserID、LoginID、ActionID、SpawnSeq、洞位；校验当前目标并固定扣返 | HitID、预测扣返或已有结果，供 UI 即时显示；不代表已入账 |
| AttackService.Snapshot | UserID、LoginID | 当前目标、成员、服务端时间、该用户待确认 HitID；不是权威钱包余额 |
| AttackService.Leave | UserID、LoginID | 释放匹配连接的成员位置；不取消已经交给记账 Service 的事件 |
| AttackService 普通确认命令 | 原 HitID、业务终局结果和原连接标识 | 完成旧 HitID，不改变不匹配的新目标或新登录 |
| MoleAccountingService 本地命中命令 | 独立事件快照：HitID、UserID、固定扣返/规则版本、组/目标引用、通知地址 | Send 只表示接纳；本地余额预检查通过后记录待扣并归批，失败或终局结果另行通知 |

ActionID 是客户端在当前 LoginID 下不复用的请求标识；相同标识和内容返回原候选结果，内容冲突拒绝。HitID 使用组随机实例 ID 和服务端候选序号，全局不复用，不能仅由 SpawnSeq 生成；拒绝后的新尝试使用新 ActionID/HitID。OperationID 直接沿用 HitID。客户端不传权威成本、奖励或余额。LoginID 仍为 string，通知地址仍使用 AccessNode + AgentName；这些是连接路由，不是账户身份。组内 Timer 和客户端请求共用串行 owner，地鼠实例和截止时间必须在服务端检查。

客户端立即播放动画，可按服务端预测扣返更新显示，并按 HitID 保留未确认变化；明确拒绝后整体撤销这一锤的预测变化，不提交反向财务操作。超时继续查询，不能直接当作失败回退。权威余额更新须结合版本和已覆盖操作标识收敛，重复消息不重复加减。

MoleAccountingService 首次接纳用户命中前调用 GetBalance 加载余额/版本，缓存未加载或加载失败不允许扣款。本地可用额为确认余额减全部待确认扣款，不计入待确认返还；相同 HitID 不重复占用，重连不清空待扣。批次终局结果到达时一起更新余额缓存并移除对应待扣，超时保留；该预检查不取代 settlement 最终校验。

### 通用银子操作与传输批次

| 字段 | 含义 |
| --- | --- |
| OperationID string | 原子业务操作身份；地鼠一锤一个，斗地主一局的多用户输赢共用一个 |
| Business / BusinessRef string | 业务类型与稳定引用，例如 mole / 组实例和目标实例；settlement 不执行其游戏规则 |
| Changes | 规范列表；每项为 UserID uint64、Debit int64、Credit int64，金额非负、使用最小银子单位 |
| Detail | 业务原始明细及规则版本；固定内容、有限大小、可追溯，不携带数据库连接或上下文 |
| BatchID string / Operations | 不可变传输批次及有序操作列表，带契约版本；封存后重试不改内容和顺序 |

远端 Handler 仅使用规范类型（uint64/int64/string/[]any/map[string]any）；Changes 和 Detail 按固定 schema 完整校验，不把 Go struct 直接传给默认 ValueCodec。具体请求摘要采用固定规范化规则，在实现时与测试一起冻结；通知地址不决定财务操作身份。

SubmitBatch 返回每项 OperationID 的已入账或终局拒绝及对应结果，并返回本批涉及用户的确认余额/版本和已处理操作标识，用于缓存与 UI 收敛；重复查询返回历史结果时不能覆盖较新余额。一次操作的所有用户变动原子执行；余额先能覆盖 Debit 才能应用 Credit，不能只检查 Credit-Debit。地鼠银子不足拒绝这一锤；斗地主某个付款用户不足，整项斗地主操作不执行；同批无关操作仍可成功。业务拒绝与 SQL/网络失败不同：后者不构成已确认终局结果。

同 OperationID/BatchID 相同内容返回已有结果，不重复扣返；不同内容拒绝。OperationID 跨批次重复也必须去重，历史成功金额不参与本批新增汇总。有限 Call 超时或断线表示未知，按原 ID 查询/重试，不重新抽奖或造新操作 ID。GetOperation/GetBatch 可取原终局结果；GetBalance 返回权威余额及版本，客户端不让旧版本覆盖新版本。

mole 统计池仅累计唯一已入账操作的扣除与返还，99% 是固定模型的数学期望，不是逐锤返还比例或短期盈利保证。银子不足、等待中、重复历史结果均不新增该池流水。待提交表属于 mole；用户银子和结算凭据属于独立 settlement。首版完整业务明细保存在结算批次 JSON 内，暂不建设外部明细导出 Outbox。
