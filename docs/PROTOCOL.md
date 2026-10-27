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

## mole 与银子结算

mole 已实现下述节点内接口；settlement 仍是预案，表中结算接口是 mole 调用方与测试结算 Service 使用的 v1 契约，不代表真实钱包已实现。完整职责、可靠性边界和实施顺序见 [DESIGN.md 第 8、9 节](DESIGN.md#8-mole-方案与首版实现)。access 和 TypeScript 尚未接入，无新增客户端 .proto。

### mole 调用链

| 入口 | 参数/作用 | 返回/通知 |
| --- | --- | --- |
| mole / 1 Enter | UserID uint64、LoginID/AccessNode/AgentName string；核对 Online、优先复用本地账，仅缓存缺失查询确认余额、分组/重连 | attack_id/name、seat int64、members/targets/hits 列表、server_ms int64、silver 余额视图 |
| AttackName / 2 Hit | UserID uint64、LoginID/ActionID string、SpawnSeq uint64、洞位 int64（0～8） | hit_id/action_id/status/debit/credit/spawn_seq/hole；正常 status=accepted，附 balance/available int64、revision uint64 |
| AttackName / 3 Snapshot | UserID uint64、LoginID string | 目标、成员、座位、服务端时间和本人命中记录；不包含权威钱包余额 |
| AttackName / 4 Leave | UserID uint64、LoginID string | bool；释放匹配成员，不取消已接纳的记账操作 |
| mole-accounting / 1 LoadBalance | UserID uint64 | 缓存余额视图，未加载时报不可用；不查询远端 |
| mole-accounting / 2 GetOperation | UserID uint64、HitID string | 本地 accepted/committed/rejected 记录，缓存缺失返回 nil |
| mole-accounting / 4 PoolSnapshot | 无参数 | hits/debit/credit/net int64，仅含唯一已入账操作；初始化未完成时报不可用 |
| mole-accounting / 6 ResumeBalance | UserID uint64 | 复用本地余额视图并取消延迟回收；缓存缺失返回 nil，未初始化等错误仍返回错误 |
| mole-submission / 2 GetBalance | UserID uint64 | settlement 确认 amount int64、version uint64；仅供 Enter 缓存缺失时初始化使用 |
| mole-submission / 3 GetOperation | UserID uint64、HitID string | 查询 settlement 已保存的原操作结果；不存在返回 nil |

余额视图字段：balance int64 为内存显示余额，confirmed_balance int64 为最近确认余额，available int64 为保守可用额，version uint64 为结算余额版本，revision uint64 为节点内存修订号。Enter 先用本地 Accounting / 6 ResumeBalance，只有明确的 nil 缓存缺失才查询 Submission，再用本地 Accounting / 3 SetBalance 应用。SetBalance 在实际回调中检查用户是否有 pending：有则忽略快照对确认基线的更新，返回当前本地视图；无则按版本初始化/更新。不依据版本推测快照已经覆盖哪些 HitID。Accounting / 5 InitPool 也仅用于本地初始化。

AttackName / 1 Join、10 Timer、11 BalanceCorrection，以及 mole / 10 Departed、11 BalanceCorrection 均为内部协调入口，客户端不得直接调用。Join/Departed 带成员实例标识，隔离同 LoginID 离开再进入后的旧清理。Accounting / 10 Hit 接收本地不可变 hitEvent 和 retry bool，返回 memoryHitResult 值快照；事件只含 HitID/UserID/ActionID/AttackName/Sequence/Hole/Debit/Credit/Rule，不含登录/连接地址。11 Timer、12 ForgetBalance、13 Settled，以及 Submission / 1 Batch、10 Timer 也只用于本地协作，引用字段传独立快照。

正常链路：AttackService 校验并固定扣返 → 本地 Call Accounting 完成内存预检查/接纳 → 立即返回 accepted 和余额，同时完成目标。Accounting 跨组归批并 Send Submission，后者独立等待远端持久结算。accepted 不是 committed，正常客户端不需要等待第二个奖励结果。

推送到 AccessNode + AgentName 的命令为 100，参数为 LoginID string、kind string、payload map[string]any。kind 为 snapshot 或 balance_correction；后者是绝对余额视图。仅在回执导致显示余额不一致时校正，不重复发奖励，不复活目标；按当前 UserID 路由/成员发送，Agent 须核对当前 LoginID。重复响应/推送按 revision 收敛，不能每收到一次再加减；修订号只在当前节点缓存生命周期内有效，重新进入以新快照建立基线。

稳定 Domain 错误：mole.invalid_request、mole.stale_login、mole.conflict、mole.busy、mole.target_unavailable、mole.online_unavailable、mole.accounting_unavailable、mole.insufficient_silver、mole.operation_unknown。已知内存拒绝由原 Hit Call 返回，目标仍有效时可释放占用。Call 超时不代表未执行，不能撤销扣返或生成新操作；沿用原 ActionID/HitID 重试。重试缓存缺失返回 operation_unknown，只能查询原操作，不能重新接纳扣款。

ActionID 在当前 LoginID 内不复用；相同内容在有界请求记录保留期间返回原结果，改内容拒绝，不重新抽奖。HitID=随机组实例 + 服务端候选序号，全局不复用；不能仅由 SpawnSeq 生成。OperationID 沿用 HitID。LoginID 仅保留在请求键和成员表，AccessNode/AgentName 只在成员表；它们都不属于银子操作身份。组内 Timer 与命中共用串行 owner，服务端检查目标实例及截止时间。

显示余额 = 确认余额 + 所有待确认扣返的净变化；可用额 = 确认余额 − 所有待确认 Debit（最少为零）。待确认返还可显示但不可消费。同 HitID 不重复占用；重连保留待扣，热点不足不逐锤远程查询。回执更新较新确认版本，同时移除对应终局操作，再计算剩余内存变化；超时保留，旧结果不覆盖新确认版本。该预检查不取代 settlement 最终合法性校验。

mole 不连接数据库。未提交的内存事件/批次在进程崩溃后可能丢失；重启只查询 settlement 已保存的操作、余额和汇总，不声称能补交已丢失批次。UI 先展示内存结果，后续采用余额校正，不产生反向财务操作。

### 通用银子操作与传输批次

目标节点/名称均为 settlement。命令：1 SubmitBatch（规范 map）、2 GetBatch（BatchID string）、3 GetOperation（UserID uint64、OperationID string）、4 GetBalance（UserID uint64）、5 Summary（Business string）。缺失批次/操作返回 nil；余额为 amount int64、version uint64，汇总为 hits/debit/credit int64。mole 直接 SubmitBatch；超时后按原 BatchID/原内容重提，由 settlement 返回已保存结果，不造新 ID。GetBatch 保留为显式查询接口，不是提交前置步骤。

| 字段 | 含义 |
| --- | --- |
| OperationID string | 原子业务操作身份；地鼠一锤一个，斗地主一局的多用户输赢共用一个 |
| Business / BusinessRef string | 业务类型与稳定引用，例如 mole / 组实例和目标实例；settlement 不执行其游戏规则 |
| Changes | 规范列表；每项为 UserID uint64、Debit int64、Credit int64，金额非负、使用最小银子单位 |
| Detail | 业务原始明细及规则版本；固定内容、有限大小、可追溯，不携带数据库连接或上下文 |
| BatchID string / Operations | 不可变传输批次及有序操作列表，带契约版本；封存后重试不改内容和顺序 |

远端 Handler 仅使用规范类型（uint64/int64/string/[]any/map[string]any）；Changes 和 Detail 按固定 schema 完整校验，不把 Go struct 直接传给默认 ValueCodec。具体请求摘要采用固定规范化规则，在实现时与测试一起冻结；通知地址不决定财务操作身份。

实际 v1 字段使用 snake_case：批次为 version uint64=1、batch_id、operations []any；操作为 operation_id、business、business_ref、changes []any、detail map；变化为 user_id uint64、debit/credit int64，明细包含 rule/action_id string。响应为 batch_id、results []any（operation_id/status/origin_batch）、balances []any（user_id/amount/version）；操作和余额均须完整且不重复，拒绝部分或畸形回复，保留待扣并按原批次重试。origin_batch 标明首次处理该操作的批次，重复操作不得再次计入 mole 统计池。

SubmitBatch 返回每项 OperationID 的已入账或终局拒绝及对应结果，并返回本批涉及用户的确认余额/版本和已处理操作标识，用于缓存与 UI 收敛；重复查询返回历史结果时不能覆盖较新余额。一次操作的所有用户变动原子执行；余额先能覆盖 Debit 才能应用 Credit，不能只检查 Credit-Debit。地鼠银子不足拒绝这一锤；斗地主某个付款用户不足，整项斗地主操作不执行；同批无关操作仍可成功。业务拒绝与 SQL/网络失败不同：后者不构成已确认终局结果。

同 OperationID/BatchID 相同内容返回已有结果，不重复扣返；不同内容拒绝。OperationID 跨批次重复也必须去重，历史成功金额不参与本批新增汇总。有限 Call 超时或断线表示未知，按原 ID 查询/重试，不重新抽奖或造新操作 ID。GetOperation/GetBatch 可取原终局结果；GetBalance 返回权威余额及版本，客户端不让旧版本覆盖新版本。

mole 统计池仅累计唯一已入账操作的扣除与返还，99% 是固定模型的数学期望，不是逐锤返还比例或短期盈利保证。银子不足、等待中、重复历史结果均不新增该池流水。mole 仅有易失内存缓冲；用户银子、操作幂等凭据和完整明细的 SQL 全归独立 settlement。首版完整业务明细保存在结算批次 JSON 内，暂不建设外部明细导出 Outbox。
