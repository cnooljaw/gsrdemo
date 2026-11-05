# 业务协议与节点调用

本文分为客户端 C/S 协议和节点间 RPC 两类，最后附主要调用链。只列当前接线及必要的停用编号；节点职责、存储与恢复设计见 [DESIGN.md](DESIGN.md)，运行和生成命令见 [README.md](../README.md)。

## 1. 客户端 C/S 协议

### 1.1 传输与通用约定

客户端只连接 access。业务定义以 [game.proto](../nodes/access/proto/demo/v1/game.proto) 为准，是否可调用还取决于当前 [Agent 注册表](../nodes/access/agent_handlers.go)及各游戏组件的 ProtocolHandlers；保留生成类型不代表入口仍然启用。

| 项目 | 当前约定 |
| --- | --- |
| 传输 | GSR v1.0.2 MAC WebSocket；握手、序号和 MAC 由 Gate 处理，业务使用 Protobuf Envelope；MAC 不加密载荷 |
| Envelope | `msg_id uint32`、`request_id uint32`、`payload bytes` |
| 请求与响应 | 请求使用非零 request_id，响应回同一 ID；服务端推送 request_id=0 |
| 身份与路由 | 客户端不填写 UserID、LoginID、节点或 Service 地址，由 Agent 根据当前连接补齐 |
| 进入准入 | 游戏请求要求已登录；一个连接不能同时进入地鼠和斗地主；游戏操作还要求已有对应路由 |
| 64 位整数 | 银子、UserID、SpawnSeq、Turn、版本和修订号在 TS 中使用 bigint，不先转 Number 再计算 |
| 消息交错 | 推送可先于对应响应到达；按当前路由、修订号和轮次处理，不用到达顺序推断业务先后 |
| 超时 | 超时不表示未执行；客户端不自动重发命中或出牌，不换操作 ID 掩盖未知结果 |

以下表格按实际业务顺序排列。请求/响应类型遵循 `.proto` 中的独立消息定义，空请求也有自己的类型。

### 1.2 认证与登录

| 接口 | 请求 ID → 响应 ID | 请求字段 | 成功响应与边界 |
| --- | --- | --- | --- |
| PasswordAuth | 1003 → 1004 | `username string`、`password string` | `user_id uint64`、`access_token string`、`expires_at_ms int64`；只完成认证，不建立登录 |
| Login | 1001 → 1002 | `access_token string`（字段 3） | `user_id uint64`；验证 Token、绑定 Online 后成功，同连接只成功登录一次 |

用户名为 1～64 字节，仅允许 `A-Z`、`a-z`、`0-9`、`_-.`，密码为 1～128 字节。Token 是 43 字符的 Base64URL 字符串；有有效 Token 时可直接 Login，无需再次提交密码。Login 的字段 1/2 及名称 username/password 已 reserved，不接受旧密码登录。

认证或 Login 失败时尝试返回 ErrorResponse，再封闭后续客户端业务，留出 200ms 发送窗口后关闭当前连接；窗口不保证送达。失败连接不会替换已有登录。普通断线不撤销 Token，Token 过期也不自动中断已登录连接。

### 1.3 地鼠协议

处理入口：[gameagent.Mole](../nodes/access/gameagent/mole.go)。

| 接口 | 请求 ID → 响应 ID | 请求字段 | 当前成功响应 |
| --- | --- | --- | --- |
| Enter | 1011 → 1012 | 空 | `snapshot Snapshot`、`silver Balance`，建立组路由和余额基线 |
| Hit | 1021 → 1022 | `action_id string`、`spawn_seq uint64`、`hole int64`（0～8） | `hit Hit`、可选 `silver Balance`；明确目标失效时只带 `rejection="target_unavailable"` |
| Snapshot | 1031 → 1032 | 空 | `snapshot Snapshot`，当前组和本人命中记录 |
| Leave | 1041 → 1042 | 空 | `left bool`；成功后清除接入路由，已接纳账务继续处理 |
| SnapshotPush | 服务端 → 1061 | 无请求 | `snapshot Snapshot`，当前组快照 |
| BalancePush | 服务端 → 1062 | 无请求 | `silver Balance`，绝对余额校正，不是再次奖励 |

只有 Domain 错误 `mole.target_unavailable` 转为 HitResponse.rejection，表示本次未记账，可刷新快照后以新 ActionID 操作新目标；此时没有 hit/silver。其他错误使用 ErrorResponse。正常命中 `hit.status=accepted`；已明确拒绝的原请求重提可返回 `hit.status=rejected` 且无 silver，不能据此显示奖励。accepted 是内存接纳，不是财务入账完成；客户端继续玩法，不逐锤查询结算状态。

| 数据 | 字段 | 客户端使用约定 |
| --- | --- | --- |
| Snapshot | `attack_name string`、`server_ms int64`、`seat int64`；`members[]`、`targets[]`、`hits[]` | members 是组成员，hits 只含本人记录；不包含权威钱包余额 |
| Member | `user_id uint64`、`seat int64` | 座位从 0 开始 |
| Target | `spawn_seq uint64`、`hole int64`、`deadline_ms int64`、`claimed bool` | 操作当前目标实例，不能只按洞位命中 |
| Hit | `hit_id/action_id/status string`、`debit/credit int64`、`spawn_seq uint64`、`hole int64` | 同一已接纳操作不重复加减；ActionID 在当前登录内不复用 |
| Balance | `balance int64`、`available int64`、`revision uint64`；可选 `confirmed ConfirmedSilver` | balance 是显示余额，available 不含待确认奖励；按新 revision 采纳绝对值 |
| ConfirmedSilver | `amount int64`、`version uint64` | 确认金额和数据库版本一起出现；省略不等于零，不能覆盖已知确认值 |

Enter 返回当前余额基线；即时 Hit 的 silver 可不含 confirmed。回执改变显示余额时才推送校正，不重复奖励、不复活目标。revision 仅在当前节点缓存生命周期内有效，重进用新基线重建；算法与恢复边界见[地鼠设计](DESIGN.md#5-地鼠即时玩法与后台入账)。

### 1.4 斗地主协议

处理入口：[gameagent.Landlord](../nodes/access/gameagent/landlord.go)。下表的快照均为本人可见的 `LandlordSnapshot`。

| 接口 | 请求 ID → 响应 ID | 请求字段 | 当前成功响应 |
| --- | --- | --- | --- |
| Enter | 1101 → 1102 | 空 | `snapshot`；随机入桌或重登回原桌 |
| Ready | 1111 → 1112 | 空 | `snapshot`；三人准备后发牌 |
| Call | 1171 → 1172 | `turn uint64`、`yes bool` | `snapshot`；当前 calling 操作者叫/不叫 |
| Rob | 1181 → 1182 | `turn uint64`、`yes bool` | `snapshot`；当前 robbing 操作者抢/不抢 |
| Play | 1121 → 1122 | `turn uint64`、`cards int64[]` | `snapshot`；空 cards 表示不出 |
| Snapshot | 1131 → 1132 | 空 | `snapshot`，当前阶段与本人手牌 |
| Leave | 1141 → 1142 | 空 | 空 `LandlordLeaveResponse`；当前 Handler 不填其中预留的 snapshot 字段，响应成功即完成离开 |
| Push | 服务端 → 1161 | 无请求 | `snapshot`，含普通游戏更新或局末结算结果 |

| 快照字段组 | 字段与类型 | 含义 |
| --- | --- | --- |
| 标识与阶段 | `room_name string`、`phase string` | phase 为 waiting/calling/robbing/playing/finished |
| 座位 | `seat/turn_seat/last_seat/landlord_seat/candidate_seat/winner_seat int64` | 座位从 0 开始；未确定的地主/候选/胜方为 -1，last_seat 无值时也为 -1 |
| 轮次与时间 | `turn uint64`、`deadline_ms int64`、`redeals int64` | 请求回传当前 turn，重发牌不归零；无计时阶段 deadline_ms=0 |
| 玩家 | `players[]`：`user_id uint64`、`seat/remaining/call_choice/rob_choice int64`、`ready bool` | 不暴露他人手牌；choice 为 0 未决定、-1 否、1 是 |
| 牌面 | `hand/bottom/last_cards int64[]` | hand 仅本人手牌；bottom 确定地主后公开；last_cards 为上一手牌 |
| 倍率 | `multiplier int64` | 全桌统一倍率 |
| 结算 | 可选 `settlement LandlordSettlement` | 无本局结算操作时不携带 |
| LandlordSettlement | `operation_id/status/reason string`、`debit/credit/balance int64`、`version uint64` | status 为 pending/committed/rejected；仅终态的 balance/version 才是有效确认值 |

牌编码为四种花色的 `0x01..0x0d`、`0x11..0x1d`、`0x21..0x2d`、`0x31..0x3d`，加小王 `0x4e`、大王 `0x4f`；比较顺序为 3..K、A、2、小王、大王。每张牌使用实际不重复编码。具体牌型、叫抢和超时规则见[斗地主设计](DESIGN.md#6-斗地主牌桌与局末结算)与 [cards.go](../nodes/landlord/cards.go)。

进行中 Leave 只断开推送并保留牌位，超时动作继续。正常局末不先推送 finished + pending，取得有效结算回执后才推送终态；主动 Snapshot 仍可能读到 pending，其默认零余额不代表已确认余额。客户端使用终态快照，不再追加逐局查账。

### 1.5 错误与保留编号

| 项目 | 当前约定 |
| --- | --- |
| 通用错误 | S2C 9001 `ErrorResponse{code, message}`，关联原 request_id；不是正常业务响应 |
| 客户端错误类别 | 1=INVALID_REQUEST、2=UNSUPPORTED_MESSAGE、3=INTERNAL；使用框架安全文案，不直接暴露内部 Domain Code 或 SQL 诊断 |
| 错误的执行含义 | 收到错误或连接中断不能一概解释为操作未执行；明确的 target_unavailable 业务拒绝另见地鼠表 |
| 1051 / 1052 | 旧地鼠 Operation 类型仍生成，access 不注册请求 Handler；编号不复用 |
| 1151 / 1152 | 旧斗地主 Operation 类型仍生成，access 不注册请求 Handler；编号不复用 |

## 2. 节点间 RPC 调用

### 2.1 地址、参数与完成语义

表中位置参数按实际 Handler 顺序列出。`U=UserID uint64`、`L=LoginID string`、`N=AccessNode string`、`A=AgentName string`、`M=Membership string`，均由服务端填写；`map` 表示 `map[string]any`，`list` 表示 `[]any`。

| 项目 | 约定 |
| --- | --- |
| 定位 | `Node + ServiceName + CommandID`；节点来自 [nodes.yaml](../configs/nodes.yaml)，动态组名/桌名使用后端返回值，不自行拼接 |
| 编码 | 直接使用 GSR Cluster ValueCodec；整数为规范 int64/uint64，集合为 map/list，不额外包装内部 Protobuf |
| 调用者 | 受信任内部节点/管理工具；Cluster Source 不充当用户身份，也不能用它推导回执地址 |
| Call | 有限等待，返回结果或错误；超时后远端可能已执行，不能盲目新建身份重试 |
| Send | nil 只表示本地接纳，不代表远端执行或客户端收到；需要完成结果的结算另走 Command 20 |
| 等待预算 | access 的后端 Call 最多 3s；mole 协调 Call 500ms；landlord 协调 Call 1s；Auth SQL 2s、Token I/O/本地 Call 500ms；结算 SQL 2s |
| 标识 | U 非零；Online/游戏/结算的字符串标识通常为 1～128 字节可见 ASCII，不裁剪、不改大小写；各入口额外限制另列 |

### 2.2 login：认证与凭据

Node 为 `login`，入口见 [AuthService](../nodes/login/authservice.go)、[TokenService](../nodes/login/tokenservice.go)。

| Service / Command | 正常调用方与方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| `auth / 1` PasswordAuth | Agent，Cluster Call | `username string, password string` | map：`user_id uint64, access_token string, expires_at_ms int64` |
| `token / 1` Issue | Auth，本地 Call；受信任签发入口 | `user_id uint64` | 同上，每次签发新凭据 |
| `token / 2` Verify | Agent，Cluster Call | `access_token string` | `user_id uint64` |
| `token / 3` Revoke | 受信任管理调用，Call | `access_token string` | bool；删除为 true，缺失为 false |

Verify 不续期，Revoke 不踢已登录连接。账号不存在和密码错误统一拒绝；接口不负责创建账号或钱包。Token 的 Redis 格式、TTL 和重启边界归[认证设计](DESIGN.md#32-密码认证与-token-登录)，不在协议文档重复展开。

### 2.3 online：当前位置

Node/Service 均为 `online`，见 [OnlineService](../nodes/online/onlineservice.go)。

| Command | 调用方与方式 | 顺序参数 | 成功结果与边界 |
| --- | --- | --- | --- |
| 1 Bind | Agent，Call | `U, L, N, A` | 被替换的位置 map；首次绑定或当前同地址重复提交返回 nil |
| 2 Lookup | 游戏进入协调，Call | `U` | 当前 map；无记录为 nil |
| 3 Unbind | Agent，Call；未确认登录的清理可 Send | `U, L` | bool；仅删除匹配的当前 LoginID |

位置 map 含 `login_id/access_node/agent_name string`。不同 LoginID 按目录实际处理顺序替换；当前 LoginID 绑定不同地址报冲突，不修改状态。已被替换或解绑的旧 LoginID 不得再次 Bind；有限的当前重复提交规则不是永久幂等。

Bind 超时可能已经生效，access 不自动重发，只尝试清理候选 LoginID 并关闭连接；Lookup 只给当时快照，不证明远端从未执行。位置命中不保证连接仍然存活；旧连接清理不会删除新绑定，Online 重启后需要显式登录重建。

### 2.4 mole：进入与组内操作

Node 为 `mole`，正常调用者为 access Agent。入口见 [MoleService](../nodes/mole/moleservice.go)、[Entry](../nodes/mole/moleentryservice.go)、[AttackService](../nodes/mole/attackservice.go)。

| Service / Command | 方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| `mole / 1` ResolveEntry | Call | `U, L, N, A` | map：`entry_name string`，当前返回 mole-entry；不分组、不作 Online 身份核对 |
| `entry_name / 1` Enter | Call | `U, L, N, A` | 地鼠快照 map，附 `silver` 内部余额视图；Entry 完成 Online、余额和 Join 协调 |
| `AttackName / 2` Hit | Call | `U, L, actionID string, spawnSeq uint64, hole int64` | Hit 字段 map；accepted 附 `balance/available int64, revision uint64`；已拒绝原记录可无余额 |
| `AttackName / 3` Snapshot | Call | `U, L` | 与 C/S Snapshot 对应的快照 map |
| `AttackName / 4` Leave | Call | `U, L` | bool；不取消已接纳账务 |

内部余额视图字段为 `balance/confirmed_balance/available int64`、`version/revision uint64`；access 在 C/S 边界把 confirmed_balance/version 转成 ConfirmedSilver。Entry 只在缓存明确缺失时查询 `settlement-read / 4`，有 pending 时普通余额查询不重置确认基线。

同 LoginID/ActionID 的相同目标请求在记录保留期间返回原结果，冲突拒绝；服务端生成 HitID 并固定抽样，OperationID 沿用 HitID。本地记账超时不撤销原记录；重试缓存缺失返回 operation_unknown，停止重发并由受控工具核查。客户端不经 Entry 查询原 HitID。

### 2.5 landlord：凑桌与牌桌操作

Node 为 `landlord`，正常调用者为 access Agent。入口见[目录](../nodes/landlord/landlordservice.go)与[牌桌](../nodes/landlord/attackservice.go)。

| Service / Command | 方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| `landlord / 1` Resolve | Call | `U, L` | map：`room_name/membership string`，服务端预留桌位 |
| `RoomName / 1` Join | Call | `U, L, N, A, M` | 本人桌快照 map；核对 Online 及目录成员实例 |
| `RoomName / 2` Ready | Call | `U, L` | 本人桌快照；查询钱包存在且余额至少 20，未冻结资金 |
| `RoomName / 6` Call | Call | `U, L, turn uint64, yes bool` | 本人桌快照；叫/不叫 |
| `RoomName / 7` Rob | Call | `U, L, turn uint64, yes bool` | 本人桌快照；抢/不抢 |
| `RoomName / 3` Play | Call | `U, L, turn uint64, cards list`，元素为 int64 | 本人桌快照；空列表表示不出 |
| `RoomName / 4` Snapshot | Call | `U, L` | 本人桌快照 |
| `RoomName / 5` Leave | Call | `U, L` | bool；Agent 接纳 true 后清路由，并返回空 C/S LeaveResponse |
| `landlord / 13` Discard | Call | `roomName string` | 无业务结果；仅确认桌已消失时清理路由再重新 Resolve |

快照 map 与 C/S LandlordSnapshot 字段对应。Join 超时可能已加入，不能换桌重试；只有明确桌不存在/关闭等失败才走 Discard。目录的 CheckRoute/Seal/Release，以及桌 Tick，属于节点内协作，不是客户端或普通跨节点业务入口。

### 2.6 通用银子结算接口

Node 取配置 `settlement_node`，默认 `settlement`。入口见[异步提交](../nodes/settlement/settlementsubmitservice.go)、[查询](../nodes/settlement/settlementqueryservice.go)、[受控 Call 写入](../nodes/settlement/settlementservice.go)。

| Service / Command | 调用方式 | 顺序参数 | 完成结果 |
| --- | --- | --- | --- |
| `settlement-submit / 1` Batch | 游戏 Send | `request map, replyNode string, replyService string` | 稍后回 Command 20，id 为原 BatchID |
| `settlement-submit / 6` Single | 游戏 Send | 同上 | 稍后回 Command 20，id 为原 OperationID |
| `settlement-submit / 7` Group | 游戏 Send | 同上 | 稍后回 Command 20，id 为原 OperationID |
| `settlement-write / 1,6,7` | 受控管理/验收 Call | `request map` | 直接返回完整回执；游戏写入不走此接口 |
| `settlement-read / 2` GetBatch | 受控 Call | `batchID string` | 原回执，缺失为 nil；不是提交前置查询 |
| `settlement-read / 3` GetOperation | 受控 Call | `U, operationID string` | OperationResult；不存在或 U 不是参与用户时为 nil |
| `settlement-read / 4` GetBalance | 游戏准入/初始化或受控 Call | `U` | map：`user_id uint64, amount int64, version uint64`；钱包缺失报错 |

#### 写请求结构

所有顶层字段必须按下表完整提供，不接受额外字段；version 的类型和值固定为 `uint64(2)`。

| 对象 | 字段 | 说明 |
| --- | --- | --- |
| Batch 请求 | `version uint64, batch_id string, operations list` | operations 为 1～500 笔独立单人操作，保持顺序；每项不带 version |
| Single 请求 | `version uint64` + 单人操作的所有字段 | 一笔单人操作 |
| 单人操作 | `operation_id/business/business_ref string, user_id uint64, items list, detail map` | items 1～512 项，不重复携带 user_id |
| 单人明细 item | `reason string, debit int64, credit int64` | Debit/Credit 均非负，按完整 Debit 检查能力 |
| Group 请求 | `version uint64, operation_id/business/business_ref string, changes list, detail map` | 一笔多人整体操作，2～64 名不同参与用户，1～512 条 changes |
| 多人明细 change | `user_id uint64, reason string, debit int64, credit int64` | 同一用户可有多条，先汇总再整体检查；不允许部分资金变化 |

地鼠每锤一个单人 Operation，items 保留 `hammer_cost` 与 `hammer_reward` 两类，即使奖励为零也保留；斗地主每局一个 Group，三人的 changes 分类为 `game_win_loss`。settlement 不解释概率、牌型或业务输赢规则，detail 仅作归档。

| 校验项 | 约束 |
| --- | --- |
| 身份与分类 | OperationID、BatchID、business、business_ref 为 1～128 字节可见 ASCII；同批 OperationID 不重复；business 不接受 wallet_init |
| reason | 1～64 字节小写字母、数字或下划线，是资金分类，不等于回执失败原因 |
| 金额 | int64 最小银子单位，非负；参与者、明细汇总、余额和版本均检查溢出；返还不能抵付本笔完整扣款 |
| 请求大小 | 整个写请求 JSON 不超过 256KiB |
| detail | map，允许空；JSON 不超过 64KiB、嵌套深度不超过 8、节点检查不超过 2,048 项；仅有界 map/list、UTF-8 string、bool、nil、int64/uint64，不接受浮点或 Go 对象 |

#### 回执与幂等

| 对象 | 字段 | 使用约定 |
| --- | --- | --- |
| Receipt | `batch_id string, results list, balances list` | Single/Group 同样返回此结构，results 只有一项 |
| OperationResult | `operation_id/status/reason/origin_batch string` | status 为 committed/rejected；origin_batch 为首次保存该操作的批次 |
| balances 项 | `user_id uint64, amount int64, version uint64` | 本次提交后的余额；原批次重提返回原历史回执，不能覆盖调用方较新的版本 |
| Command 20 | `id string, receipt map, code string` | 成功时 code 为空；错误时 receipt 为空 map，code 为稳定内部错误码 |

code 为空只表示收到了结算回执，财务结果仍看每项 status。当前 rejected 的 reason 为 insufficient_silver、balance_overflow 或 version_overflow；committed 时为空。业务拒绝是持久终态，与 SQL/提交错误的未知结果不同；同批其他操作可成功，Group 始终整体判定。

OperationID 是全局财务身份，BatchID 是提交身份。同身份同内容复用原终态，不重复扣返；不同内容报 identity_conflict。摘要由规范化内部结构的 JSON 作 SHA-256：明细按 UserID/Reason/Debit/Credit 排序但不合并，批次操作顺序保持有意义。Single/Group 的内部 BatchID 为 `single-/group-` 加 OperationID 的 SHA-256 十六进制。

游戏 owner 注册私有回执服务名，并显式填写 replyNode/replyService；地址不进入账务摘要，不能来自客户端或依赖远程 Source。原请求与内容保持不变，约 5～5.5s 未确认时重提；有效终态只应用一次，其他操作、重复回执和迟到错误不能覆盖已确认结果。Send、路由或回执丢失可能只体现为超时，没有同步远端错误。

结算入口不做 SQL，独立执行者提交后直接回执。任务生成后排队超过 2s 不启动 SQL、尝试回 busy；不包含入口邮箱里的先前等待。并行度、事务及 MySQL 表设计见 [Settlement 设计](DESIGN.md#7-settlement并行执行与权威账务)。钱包只能由独立管理入口初始化，普通 RPC 不创建钱包或提交 wallet_init。

### 2.7 回调 access：快照、校正与旧登录替换

目标均为对应玩家的 `AccessNode / AgentName`，以 Cluster.Send 调用。Agent 先核对当前 LoginID，游戏组件另核对自身路由。

| 来源 / Command | 顺序参数 | 接收行为 | 对应 C/S 消息 |
| --- | --- | --- | --- |
| mole / 100 SnapshotPush | `L, snapshot map` | 核对当前组，推送本人快照 | 1061 |
| mole / 102 BalanceCorrection | `L, balance map` | 当前登录且仍在地鼠中才推送绝对余额 | 1062 |
| landlord / 110 SnapshotPush | `L, snapshot map` | 核对当前桌，推送本人快照或结算结果 | 1161 |
| 新登录 Agent / 101 Replaced | `oldLoginID string` | 仅匹配旧登录时清理游戏、条件 Unbind 并退出 | 关闭旧连接，无专用业务响应 |

推送 Send 接纳不等于客户端送达，不以玩家确认收包作为结算幂等或 Redis 清理条件。结算 Command 20 回给游戏 owner，不直接回给 access 或客户端。

### 2.8 内部错误与停用入口

以下是服务端稳定 Domain Code；C/S 只收到第 1.5 节的框架安全错误。参数类型/数量错误与未知命令沿用 GSR `runtime.invalid_command_arguments / runtime.unknown_command`。

| 节点/能力 | 错误码 | 调用方处置 |
| --- | --- | --- |
| access 准入/转换 | `access.login_required / enter_required / already_logged_in / invalid_backend_reply` | 未登录、路由缺失或后端回包非法时不继续业务 |
| 认证 | `login.invalid_request / invalid_credentials / store_unavailable` | 不建立登录；账号不存在与密码错误不区分 |
| Token | `auth.invalid_token / token_store_unavailable` | 不放行，后者不能降级绕过 Redis 校验 |
| Online | `online.invalid_request / login_id_conflict` | 不修改非法目录状态，不复用旧登录身份 |
| mole | `mole.invalid_request / stale_login / conflict / busy / target_unavailable / online_unavailable / accounting_unavailable / insufficient_silver / operation_unknown` | 区分明确拒绝与未知结果；target_unavailable 的 C/S 特例见 1.3 |
| landlord | `landlord.invalid_request / stale_login / invalid_turn / invalid_cards / busy / insufficient_silver` | 不盲目重复出牌；当前轮次与状态用快照核实 |
| settlement 临时故障 | `settlement.busy / store_unavailable` | 保留原请求，按游戏 owner 的限频策略重试；不能当成财务 rejected |
| settlement 确定性故障 | `settlement.invalid_request / identity_conflict / wallet_not_found` | 保留请求并暂停自动重试，核查身份、格式或钱包初始化 |

表中同一行的省略项沿用第一个完整前缀，例如 `login.store_unavailable`。本地 Redis/通知失败的恢复与退避由各游戏管理，详见设计文档，不新增通用重试器。

| 停用入口 | 当前行为 |
| --- | --- |
| mole Entry / 3 | 不再转发原 HitID 查询，编号不复用 |
| mole-submission / 2、3 | 旧查询入口已移除，编号不复用 |
| settlement / 5 Summary | 已移除，无汇总查询接口，编号不复用 |

Pulse Tick、分组预留/成员清理、Accounting 内存记账、Submission 批次交接等仅属同节点协作，使用 owner 私有类型及本地消息；不作为跨节点 RPC 契约，也不向客户端开放。完整注册就近见[地鼠 MainService](../nodes/mole/main_service.go)、[斗地主目录](../nodes/landlord/landlordservice.go)及各 Service 源码。

## 3. 主要调用链

以下只串联前文已定义的入口；箭头中的 Call/Send 区分等待结果和提交通知，本地步骤单独标明。

### 3.1 认证、登录与旧连接替换

```text
客户端 PasswordAuth 1003
  → Agent Call login/auth/1
  → Auth 验证 MySQL 密码，本地 Call token/1
  ← 1004：平台 Token（尚未登录）

客户端 Login 1001
  → Agent Call login/token/2
  → Agent Call online/online/1 Bind
  → 若有旧位置：Send 旧 AccessNode/AgentName/101（不等待清理）
  ← 1002：登录成功

旧 Agent 独立处理 101：尽力离开游戏、条件 Unbind、退出
```

### 3.2 地鼠进入与一次命中

```text
1011 Enter
  → Agent Call mole/mole/1
  → Agent Call mole-entry/1
  → Entry Call online/online/2
  → 本地复用余额；仅缺失时 Call settlement-read/4
  → 本地目录预留与 AttackService Join
  ← 1012：组快照和余额基线

1021 Hit
  → Agent Call mole/AttackName/2
  → Attack 固定 HitID、成本和奖励，本地 Call Accounting
  ← 1022：accepted 与内存余额；客户端继续玩法
```

### 3.3 地鼠后台入账与差异校正

```text
Accounting 本地 Send 原批次给 Submission
  → Submission 保存 Redis 原批次
  → Send settlement-submit/1（原 BatchID + 回执地址）
  → 执行者提交 MySQL，Send 游戏私有回执地址 / 20
  → Submission 校验回执，完成 Redis 统计和检查点
  → 本地 Send 回执给 Accounting，应用确认余额并释放待扣
  → 仅显示有差异：MoleService → AttackService
  → Send 当前 Agent/102 → C/S 1062
```

### 3.4 斗地主一局与三人结算

```text
1101 Enter → Call landlord/landlord/1 → Call RoomName/1 Join
  → 桌核对 Online 与本地成员实例 → 返回本人快照
1111 Ready → Call RoomName/2 → Call settlement-read/4 准入
1171 Call / 1181 Rob / 1121 Play → 桌 / 6、7、3（携带当前 Turn）
  → 本桌确定胜负，固定一个 OperationID 与三人金额
  → 保存 Redis 待请求，Send settlement-submit/7
  → MySQL 原子判定，Send 本桌私有回执地址 / 20
  → applyReceipt → Send 三人的当前 Agent/110 → C/S 1161
  → 删除 Redis 待记录；删除失败只重试删除
```

两个游戏在回执未知时都重提原 ID 和原内容，不追加玩家查账。重启只恢复已经保存的账务请求，不恢复未保存命中或进行中手牌；边界见[持久化与故障汇总](DESIGN.md#8-持久化与故障边界汇总)。
