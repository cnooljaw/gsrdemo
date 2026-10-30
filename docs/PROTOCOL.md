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

mole 已实现下述节点内接口；settlement 已实现通用银子结算接口与 MySQL 适配；真实数据库和独立进程验证按 README 显式启用，不能用默认测试代替。完整职责、可靠性边界和实施顺序见 [DESIGN.md 第 8、9 节](DESIGN.md#8-mole-方案与首版实现)。access 和 TypeScript 尚未接入，无新增客户端 .proto。

### mole 调用链

| 入口 | 参数/作用 | 返回/通知 |
| --- | --- | --- |
| mole / 1 ResolveEntry | UserID uint64、LoginID/AccessNode/AgentName string；仅解析进入通道，不验证身份、不分配座位 | entry_name string，调用方不自行拼接名称 |
| entry_name / 1 Enter | 同上；核对 Online、复用本地账，缓存缺失直接查询 Settlement，再请求目录预留、Join | attack_id/name、seat int64、members/targets/hits 列表、server_ms int64、silver 余额视图 |
| entry_name / 3 GetOperation | UserID uint64、HitID string；地址通过 ResolveEntry 获取 | 只读查询 Settlement 原操作；缺失返回 nil，不与提交共用等待队列 |
| AttackName / 2 Hit | UserID uint64、LoginID/ActionID string、SpawnSeq uint64、洞位 int64（0～8） | hit_id/action_id/status/debit/credit/spawn_seq/hole；正常 status=accepted，附 balance/available int64、revision uint64 |
| AttackName / 3 Snapshot | UserID uint64、LoginID string | 目标、成员、座位、服务端时间和本人命中记录；不包含权威钱包余额 |
| AttackName / 4 Leave | UserID uint64、LoginID string | bool；释放匹配成员，不取消已接纳的记账操作 |
| mole-accounting / 1 LoadBalance | UserID uint64 | 缓存余额视图，未加载时报不可用；不查询远端 |
| mole-accounting / 2 GetOperation | UserID uint64、HitID string | 本地 accepted/committed/rejected 记录，缓存缺失返回 nil |
| mole-accounting / 4 PoolSnapshot | 无参数 | hits/debit/credit/net int64，仅含唯一已入账操作；初始化未完成时报不可用 |
| mole-accounting / 6 ResumeBalance | UserID uint64 | 复用本地余额视图并取消延迟回收；缓存缺失返回 nil，未初始化等错误仍返回错误 |
| mole-submission / 11 State | 无参数 | lane uint64、ready/blocked bool；有在途批次时附 batch_id string、sequence uint64；不访问外部存储 |

余额视图字段：balance int64 为内存显示余额，confirmed_balance int64 为最近确认余额，available int64 为保守可用额，version uint64 为结算余额版本，revision uint64 为节点内存修订号。Enter 先用本地 Accounting / 6 ResumeBalance，只有明确的 nil 缓存缺失才直接查询 Settlement，再用本地 Accounting / 3 SetBalance 应用。SetBalance 在实际回调中检查用户是否有 pending：有则忽略快照对确认基线的更新，返回当前本地视图；无则按版本初始化/更新。不依据版本推测快照已经覆盖哪些 HitID。Accounting / 5 InitPool（pool molePool、sequence/lane uint64）仅用于本地初始化；Accounting / 14 StopLane 暂停指定通道，均不供客户端调用。

AttackName / 1 Join、10 Timer、11 BalanceCorrection，以及 mole / 10 Departed、11 BalanceCorrection、12 Reserve、13 DiscardGroup、14 Health 均为内部协调入口，客户端不得直接调用。Join/Departed 带成员实例标识，隔离同 LoginID 离开再进入后的旧清理。Accounting / 10 Hit 接收本地不可变 hitEvent 和 retry bool，返回 memoryHitResult 值快照；事件只含 HitID/UserID/ActionID/AttackName/Sequence/Hole/Debit/Credit/Rule，不含登录/连接地址。11 Timer、12 ForgetBalance、13 Settled（batchReceipt、lane uint64）、14 StopLane，以及 Submission / 1 Batch、10 Timer 也只用于本地协作，引用字段传独立快照。

正常链路：AttackService 校验并固定扣返 → 本地 Call Accounting 完成内存预检查/接纳 → 立即返回 accepted 和余额，同时完成目标。Accounting 跨组归批并 Send Submission，后者独立等待远端持久结算。accepted 不是 committed，正常客户端不需要等待第二个奖励结果。

推送到 AccessNode + AgentName 的命令为 100，参数为 LoginID string、kind string、payload map[string]any。kind 为 snapshot 或 balance_correction；后者是绝对余额视图。仅在回执导致显示余额不一致时校正，不重复发奖励，不复活目标；按当前 UserID 路由/成员发送，Agent 须核对当前 LoginID。重复响应/推送按 revision 收敛，不能每收到一次再加减；修订号只在当前节点缓存生命周期内有效，重新进入以新快照建立基线。

稳定 Domain 错误：mole.invalid_request、mole.stale_login、mole.conflict、mole.busy、mole.target_unavailable、mole.online_unavailable、mole.accounting_unavailable、mole.insufficient_silver、mole.operation_unknown。已知内存拒绝由原 Hit Call 返回，目标仍有效时可释放占用。Call 超时不代表未执行，不能撤销扣返或生成新操作；沿用原 ActionID/HitID 重试。重试缓存缺失返回 operation_unknown，只能查询原操作，不能重新接纳扣款。

ActionID 在当前 LoginID 内不复用；相同内容在有界请求记录保留期间返回原结果，改内容拒绝，不重新抽奖。HitID=随机组实例 + 服务端候选序号，全局不复用；不能仅由 SpawnSeq 生成。OperationID 沿用 HitID。LoginID 仅保留在请求键和成员表，AccessNode/AgentName 只在成员表；它们都不属于银子操作身份。组内 Timer 与命中共用串行 owner，服务端检查目标实例及截止时间。

显示余额 = 确认余额 + 所有待确认扣返的净变化；可用额 = 确认余额 − 所有待确认 Debit（最少为零）。待确认返还可显示但不可消费。同 HitID 不重复占用；重连保留待扣，热点不足不逐锤远程查询。回执更新较新确认版本，同时移除对应终局操作，再计算剩余内存变化；超时保留，旧结果不覆盖新确认版本。该预检查不取代 settlement 最终合法性校验。

mole 不访问 SQL。尚未保存到业务 Redis 的内存事件可因崩溃丢失；已保存批次重启后按原身份和内容恢复结算、统计，再开放进入。余额和原操作仍可向 settlement 查询。UI 先展示内存结果，后续采用余额校正，不产生反向财务操作。

Mole 的进入通道固定 8 个；调用方应使用 ResolveEntry 返回的地址。一个慢进入只占住该通道，目录和其他通道继续处理。提交按通道保持一个在途批次和最多 4,096 项待确认额度。Submission 原 2/3 查询入口移除且不复用；临时错误原 ID 退避重试，确定性错误保留批次、暂停该通道；State 可用于定位故障，不是财务成功证明。PoolSnapshot 需要所有通道就绪，不把部分合计冒充全池统计；它不可用不影响已就绪通道的命中。

### 通用银子结算接口

目标 Node 为 settlement，version 固定 uint64=2。写入调用 `common/settlement.WriteService(identity)`：SettleBatch 使用 BatchID，SettleSingle/SettleGroup 使用 OperationID；返回 settlement-write-00～03。只读调用 ReadService(identity)：GetBatch 使用 BatchID、GetOperation 使用 OperationID、GetBalance 使用 UserID 十进制字符串，返回 settlement-read-00～01。FNV-1a 32 位散列后取模，函数与完整入口清单在 common/settlement/routing.go；入口数固定，调用方同步升级，不保留旧 Service=settlement 的转发兼容层。所有写入口返回持久事务提交后的回执，不根据游戏类型分派玩法。金额使用 int64 最小银子单位，非负 Debit/Credit，UserID 为非零 uint64。仅供受信任业务节点调用。

| CommandID | 接口 | 请求与业务成功边界 |
| --- | --- | --- |
| 1 | SettleBatch | version、batch_id、operations []any；每项是独立的单人操作，逐笔成功或拒绝 |
| 6 | SettleSingle | version + 一笔单人操作；该笔整体成功或拒绝 |
| 7 | SettleGroup | version、operation_id、business、business_ref、detail、changes []any；全部参与用户整体成功或拒绝 |
| 2 | GetBatch | BatchID string；返回原回执，缺失返回 nil |
| 3 | GetOperation | UserID uint64、OperationID string；仅参与用户可查询，缺失或非参与用户返回 nil |
| 4 | GetBalance | UserID uint64；返回 user_id uint64、amount int64、version uint64，未初始化钱包返回错误 |

单人操作为 operation_id/business/business_ref string、user_id uint64、items []any、detail map[string]any；items 每项含 reason string、debit/credit int64，不重复携带 UserID。批次内单人项不带 version。多人接口使用 changes，每项含 user_id/reason/debit/credit；同一 UserID 可出现多条，参与用户须为 2～64 名。两类操作都允许 1～512 条明细（多人至少两名用户）。SettleBatch 不接受 changes，SettleSingle 不要求调用方组装多人结构。mole 每锤仍是一项 Operation，items 分为 hammer_cost 扣款、hammer_reward 返还，即使返还为零也保留分类。旧 version=1 写请求拒绝，调用方与 settlement 须同步升级。

OperationID 是全局业务身份；地鼠每锤一个，棋牌一局共同使用一个。BusinessRef 是业务提供的稳定引用，settlement 不解析洞位、局号、概率或牌型。Detail 仅作通用归档，不要求 rule/action_id 等玩法字段；允许有界的 map/列表、string、bool、nil、int64/uint64，不接受浮点数或 Go 对象。调用方负责按业务生成合法的银子变化，settlement 检查账户和资金合法性。

CommandID=5（原 Summary）已移除且不复用，返回 ErrUnknownCommand。

标识为 1..128 字节可见 ASCII；请求只接受已定义字段、规范参数类型。批次 1～500 笔、整个写请求的 JSON 表示不超过 256KiB；detail 不超过 64KiB、深度不超过 8、元素检查不超过 2,048 项。Reason 为 1～64 字节小写 ASCII 字母、数字或下划线，仅表示通用资金分类；不在 Settlement 中解释玩法。批次内 OperationID 重复、负数、整笔扣款/返还总额溢出或畸形结构拒绝，不写账。

幂等摘要基于标准库 JSON 的规范化内部值对象取 SHA-256：固定字段、明细 map 键排序、所有变化按 UserID、Reason、Debit、Credit 排序；不合并或去重明细。重排明细身份不变，改变分类或金额构成即改变身份；批次操作顺序有意义。同 OperationID/BatchID 相同内容返回已有结果，不重复扣返；内容不同返回 identity_conflict。单人/多人接口自动生成 single-/group- 加 OperationID 的 SHA-256 十六进制作为内部 BatchID。相同操作通过其他入口/其他批次重提也必须去重。

回执为 batch_id string、results []any、balances []any。每项结果含 operation_id/status/reason/origin_batch string；status 为 committed 或 rejected，reason 是结果失败原因，与资金分类 Reason 分开，可为空、insufficient_silver、balance_overflow、version_overflow。origin_batch 是第一次保存该操作的批次，业务凭此识别跨批重提的旧操作。balances 为本批涉及用户在本次提交后的余额和版本；原批次重试返回历史原回执，不能覆盖调用方较新确认版本。单人/多人接口也返回这一回执结构，results 只有一项。

数据库事务按稳定用户顺序用 FOR UPDATE NOWAIT 锁定余额，遇行锁冲突整体回滚并返回 store_unavailable，调用方按原身份重试，不跳过任何钱包。按请求操作顺序先按 UserID 汇总每笔明细，再检查完整 Debit，返还不能抵付本笔扣款。多人操作先检查所有参与者再整体修改；每笔成功 Operation 对每名用户只递增一次版本，批量单人各项独立判定。只对本次新成功的操作集合更新余额与版本，批量写入余额、操作索引、批次原始明细及回执；提交成功才返回结果。行锁竞争、SQL/提交失败返回 store_unavailable，表示结果未确认，不当作财务拒绝；按原身份和内容重试即可。GetBatch 是显式查询能力，不是提交前置步骤。

稳定 Domain 错误：settlement.invalid_request、settlement.identity_conflict、settlement.wallet_not_found、settlement.store_unavailable。钱包必须显式初始化；缺失钱包拒绝请求且不自动创建/发银子。用户中心决定创建钱包和初始银子，Settlement 只执行初始化账务；Demo 的 -init-wallet 模拟该管理入口，不自动接入 login 注册。initialize(ctx, userID, amount) 与普通 settle(ctx, batch) 分开：确定性 OperationID=wallet-init-{UserID}，BatchID=init-加该 OperationID 的 SHA-256 十六进制；先查初始化凭据，再在同一事务创建零余额钱包、执行 initial_balance 分类的 wallet_init 操作、保存索引/明细/回执并提交。相同金额重试返回原回执，不重置当前余额；不同金额或已有钱包缺少初始化凭据返回冲突。唯一键竞争等 SQL 错误回滚，由调用方按原 ID 重试，不增加内部重试循环。wallet_init 不接受普通 RPC 提交。

settlement 不内置归批 Timer 或游戏状态。地鼠选择每 100ms/数量阈值归批；棋牌可立即调用多人接口。mole 的各通道累计统计与最多一个待处理批次保存在业务 Redis，不调用结算汇总。固定 4 条提交通道按 UserID % 4 分配，通道 0 为 mole-submission，其余追加 -01～-03；每条序号只用于自己的业务检查点，不传给 Settlement；同一批次的 ID、内容和序号在重试时保持不变。只有 committed 且 origin_batch 等于当前 batch_id 的操作新增统计。Redis 统计和检查点更新成功后才向 Accounting 交付回执。mole 不访问 SQL；尚未保存到 Redis 的内存操作仍可丢失，已保存批次在重启时原样补交。余额、操作幂等索引和完整批次明细归 settlement，冷热归档和短 ID 尚待下一阶段实施。

结算写/读入口只接受各自命令；内部命令 9 仅供本地监督 Service 的非阻塞存活检查。每入口 Mailbox=4，满时返回 runtime.mailbox_full，仍按原身份退避重试，不能换 ID 重做。6 个入口最多保留 30 个执行/排队 Call，低于 GSR v1.0.2 每入站连接 32 个并发名额，避免慢入口占满同连接。进程内监督 Service 不持有钱包，只在执行 Service 已退出后重新发布同名实例，不自动重放请求。Call 超时仍不证明事务未执行。
