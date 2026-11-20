# 业务协议与节点调用

本文分为客户端 C/S 协议和节点间 RPC 两类，最后附主要调用链。只列当前接线；节点职责、存储与恢复设计见 [DESIGN.md](DESIGN.md)，运行和生成命令见 [README.md](../README.md)。

## 1. 客户端 C/S 协议

### 1.1 传输与通用约定

客户端只连接 access。消息编号和字段定义见 [game.proto](../nodes/access/proto/demo/v1/game.proto)，客户端请求由 [Agent](../nodes/access/agent_handlers.go) 和各游戏组件注册。

| 项目 | 当前约定 |
| --- | --- |
| 传输 | GSR v1.0.4 MAC WebSocket；握手、序号和 MAC 由 Gate 处理，业务使用 Protobuf Envelope；MAC 不加密载荷 |
| Envelope | `msg_id uint32`、`request_id uint32`、`payload bytes` |
| 请求与响应 | `KIND_REQUEST` 使用非零 `request_id`，`KIND_RESPONSE` 携带原 `request_id` |
| 服务端推送 | `KIND_PUSH` 使用 `request_id=0`，按所属游戏的事件规则更新状态 |
| 身份与路由 | 客户端不填写 UserID、LoginID、节点或 Service 地址，由 Agent 根据当前连接补齐 |
| 进入准入 | 游戏请求要求已登录；一个连接不能同时进入地鼠和斗地主；游戏操作还要求已有对应路由 |
| 64 位整数 | 银子、UserID、SpawnSeq、Turn、版本和修订号在 TS 中使用 bigint，不先转 Number 再计算 |
| 消息交错 | 推送与其他请求响应可能交错；按当前路由、事件序号和余额修订号处理，不用到达顺序推断业务先后 |
| 超时 | 超时不表示未执行；客户端不自动重发命中或出牌，不换操作 ID 掩盖未知结果 |

客户端按下表指定预期的响应编号。只需确认成功的操作共用空消息 `1005 ActionAck`，请求失败时使用[通用错误](#16-错误)。

### 1.2 认证与登录

| 接口 | 请求 ID → 响应 ID | 请求字段 | 成功响应与边界 |
| --- | --- | --- | --- |
| PasswordAuth | 1003 → 1004 | `username string`、`password string` | `user_id uint64`、`access_token string`、`expires_at_ms int64`；只完成认证，不建立登录 |
| Login | 1001 → 1002 | `access_token string` | `user_id uint64`；验证 Token、绑定 Online 后成功，同连接只成功登录一次 |

用户名为 1～64 字节，仅允许 `A-Z`、`a-z`、`0-9`、`_-.`，密码为 1～128 字节。Token 是 43 字符的 Base64URL 字符串；有有效 Token 时可直接 Login，无需再次提交密码。

未响应的认证或 Login 失败时，错误策略先封闭后续客户端业务，Dispatcher 经 Gate SendAndClose 尝试写出唯一 ErrorResponse 后有界关闭；接纳不保证送达。未登录新连接在密码认证或 Token 校验阶段失败，不会修改其他连接的在线绑定；Bind 后的失败边界见[失败、替换与断线](DESIGN.md#33-失败替换与断线)。普通断线不撤销 Token，Token 过期也不自动中断已登录连接。

### 1.3 地鼠协议

处理入口：[comMole](../nodes/access/commole.go)。

| 接口 | 请求 ID → 响应 ID | 请求字段 | 业务响应 |
| --- | --- | --- | --- |
| Enter | 1011 → 1012 | 空 | `snapshot Snapshot`、`silver Balance`，建立组路由和余额基线 |
| Hit | 1021 → 1022 | `action_id string`、`spawn_seq uint64`、`hole int64`（0～8） | 接纳或重放时返回 `hit Hit` 和可选 `silver Balance`。目标失效时只带 `rejection="target_unavailable"` |
| Snapshot | 1031 → 1032 | 空 | `snapshot Snapshot`，当前组和本人命中记录 |
| Leave | 1041 → 1005 | 空 | `ActionAck`；接入已清除组路由，已接纳账务继续处理 |

| 推送 | 消息 ID | 主要字段与含义 |
| --- | --- | --- |
| SnapshotPush | 1061 | `snapshot Snapshot`，当前组快照 |
| BalancePush | 1062 | `silver Balance`，用绝对余额校正显示值 |

只有 Domain 错误 `mole.target_unavailable` 转为 HitResponse.rejection，表示本次未记账，可刷新快照后以新 ActionID 操作新目标；此时没有 hit/silver。其他错误使用 ErrorResponse。正常命中 `hit.status=accepted`；已明确拒绝的原请求重提可返回 `hit.status=rejected` 且无 silver，不能据此显示奖励。accepted 是内存接纳，不是财务入账完成；客户端继续玩法，不逐锤查询结算状态。

| 数据 | 字段 | 客户端使用约定 |
| --- | --- | --- |
| Snapshot | `attack_name string`、`server_ms int64`、`seat int64`；`members[]`、`targets[]`、`hits[]` | members 是组成员，hits 只含本人记录；不包含权威钱包余额 |
| Member | `user_id uint64`、`seat int64` | 座位从 0 开始 |
| Target | `spawn_seq uint64`、`hole int64`、`deadline_ms int64`、`claimed bool` | 操作当前目标实例，不能只按洞位命中 |
| Hit | `hit_id/action_id/status string`、`debit/credit int64`、`spawn_seq uint64`、`hole int64` | 同一已接纳操作不重复加减；ActionID 在当前登录内不复用 |
| Balance | `balance int64`、`available int64`、`revision uint64`；可选 `confirmed ConfirmedSilver` | balance 和 available 非负，revision 从 1 开始；balance 是显示余额，available 不含待确认奖励，按新 revision 采纳绝对值 |
| ConfirmedSilver | `amount int64`、`version uint64` | 确认金额和数据库版本一起出现；省略不等于零，不能覆盖已知确认值 |

Enter 返回当前余额基线；即时 Hit 的 silver 可不含 confirmed。access 校验内部回包的 balance、available、revision 及成对的确认余额/版本，缺失或类型错误不会作为默认零值下发。回执改变显示余额时才推送校正，不重复奖励、不复活目标。revision 仅在当前节点缓存生命周期内有效，重进用新基线重建；算法与恢复边界见[地鼠设计](DESIGN.md#5-地鼠即时玩法与后台入账)。

### 1.4 斗地主协议

处理入口：[comLandlord](../nodes/access/comlandlord.go)。下表的快照均为本人可见的 `LandlordSnapshot`。

| 接口 | 请求 ID → 响应 ID | 请求字段 | 当前成功响应 |
| --- | --- | --- | --- |
| Enter | 1101 → 1102 | 空 | `snapshot`；随机入桌或重登回原桌 |
| Ready | 1111 → 1005 | 空 | `ActionAck`；准备、发牌通过事件通知 |
| Call | 1171 → 1005 | `turn uint64`、`yes bool` | `ActionAck`；叫/不叫结果广播全桌 |
| Rob | 1181 → 1005 | `turn uint64`、`yes bool` | `ActionAck`；抢/不抢结果广播全桌 |
| Play | 1121 → 1005 | `turn uint64`、`cards int64[]` | `ActionAck`；空 cards 表示不出，出牌事件广播全桌 |
| Trustee | 1201 → 1005 | `enabled bool` | `ActionAck`；本人开启或取消托管，状态由事件广播 |
| Snapshot | 1131 → 1132 | 空 | `snapshot`，当前阶段与本人手牌 |
| Leave | 1141 → 1005 | 空 | `ActionAck`；牌桌已接纳离开且接入已清除桌路由，目录可稍后回收 |

事件均带 `room_name` 和牌桌内递增的 `revision`，普通事件包括操作本人；具体字段以 [game.proto](../nodes/access/proto/demo/v1/game.proto) 为准。

| 推送 | 消息 ID | 主要字段与含义 |
| --- | --- | --- |
| Seat | 1191 | `user_id, seat, joined, ready`；坐下、准备取消或回收座位 |
| Ready | 1192 | `user_id, seat, ready`；玩家准备 |
| Deal | 1193 | `hand, turn, turn_seat, start_ms, deadline_ms, server_ms, redeals`；每人只收到自己的手牌，等待服务器开放叫地主 |
| Bid | 1194 | `user_id, rob, yes, phase, turn, turn_seat, start_ms, can_act, deadline_ms, server_ms, candidate_seat, landlord_seat, multiplier, bottom, redealt`；叫抢及确定地主 |
| Play | 1195 | `user_id, cards, remaining, phase, turn, turn_seat, start_ms, can_act, deadline_ms, server_ms, last_cards, last_seat, winner_seat, multiplier`；空 cards 为不出 |
| Settlement | 1196 | `settlement`；每人只收到自己的实际扣返和确认余额 |
| Trustee | 1197 | `user_id, trustee, start_ms, deadline_ms, server_ms`；托管状态和当前时间信息 |
| Turn | 1198 | `phase, turn, turn_seat, deadline_ms, server_ms`；动画等待结束，服务器开放当前轮次 |

| 快照字段组 | 字段与类型 | 含义 |
| --- | --- | --- |
| 标识与阶段 | `room_name string`、`phase string` | phase 为 waiting/calling/robbing/playing/finished |
| 座位 | `seat/turn_seat/last_seat/landlord_seat/candidate_seat/winner_seat int64` | 座位从 0 开始；未确定的地主/候选/胜方为 -1，last_seat 无值时也为 -1 |
| 轮次与时间 | `turn uint64`、`deadline_ms/start_ms/server_ms int64`、`can_act bool`、`redeals int64`、`revision uint64` | turn 用于操作校验，revision 为事件基线；动画等待时 can_act=false、start_ms 为预计开放时间；开放后 deadline_ms 为人工截止或托管自动执行时间；无对应时间点时为 0 |
| 玩家 | `players[]`：`user_id uint64`、`seat/remaining/call_choice/rob_choice int64`、`ready/trustee bool` | 不暴露他人手牌；choice 为 0 未决定、-1 否、1 是 |
| 牌面 | `hand/bottom/last_cards int64[]` | hand 仅本人手牌；bottom 确定地主后公开；last_cards 为上一手牌 |
| 倍率 | `multiplier int64` | 全桌统一倍率 |
| 结算 | 可选 `settlement LandlordSettlement` | 无本局结算操作时不携带 |
| LandlordSettlement | `operation_id/status/reason string`、`debit/credit/balance int64`、`version uint64` | status 为 pending/committed；pending 的 debit/credit 为 0，committed 的 debit/credit 为实际扣返，balance/version 为确认值 |

牌编码为四种花色的 `0x01..0x0d`、`0x11..0x1d`、`0x21..0x2d`、`0x31..0x3d`，加小王 `0x4e`、大王 `0x4f`；比较顺序为 3..K、A、2、小王、大王。每张牌使用实际不重复编码。具体牌型、叫抢和超时规则见[斗地主设计](DESIGN.md#6-斗地主牌桌与局末结算)与 [cards.go](../nodes/landlord/cards.go)。

进行中 Leave 只断开推送并保留牌位，超时动作继续。出牌事件先通知动作；局末结算确认后单独发送 Settlement，主动 Snapshot 仍可能读到 pending，其默认零余额不代表已确认余额。成功响应表示牌桌已执行动作，不表示广播一定送达；失败、断线或超时可能结果未知，不盲目重复出牌。

客户端以 Enter/Snapshot 的 revision 建立基线，对同桌事件按序应用，忽略已包含的旧事件，发现缺口请求 Snapshot；同一事件序号对每名在线玩家均有对应消息，发牌和结算的内容按本人生成。缓冲过多事件、场景不一致或等待预计更新超时也可恢复场景；最后一条事件丢失不能单靠序号发现。TS 演示客户端按事件驱动，恢复时最多保留 128 条在途事件。计时由牌桌服务器判定：发牌后等待 3 秒、首次或重新领出前等待 0.5 秒，客户端须等 Turn 推送或快照显示 `can_act=true` 才提交操作；托管玩家仍不能手动操作。客户端以服务器时间点与 `server_ms` 的差值及本地单调经过时间估计恢复等待，不依赖本机日历时间。`turn` 仍只用于操作校验，不充当事件序号。

### 1.5 定海麻将协议

客户端登录后通过 Access 调用麻将目录。同一连接只能进入一种游戏。

| 操作 | 请求 → 成功响应 | 请求字段 | 成功结果 |
| --- | --- | --- | --- |
| Enter | 1301 → 1302 | 空 | `DhmjEnterResponse.snapshot`，加入随机桌或恢复原座 |
| Ready | 1311 → 1005 | 空 | `ActionAck`，目录确认准备 |
| Snapshot | 1321 → 1322 | 空 | `DhmjSnapshotResponse.snapshot`，用于显式恢复 |
| Leave | 1331 → 1005 | 空 | `ActionAck`，目录接纳离开 |
| Discard | 1341 → 1005 | `turn uint64, tile int64` | `ActionAck`，本次出牌已执行 |
| Respond | 1351 → 1005 | `window_id uint64, action int64` | `ActionAck`，本人的选择已记录，窗口可能仍在等其他座位 |
| SelfWin | 1361 → 1005 | `turn uint64` | `ActionAck`，牌桌确认自摸 |
| Kong | 1371 → 1005 | `turn uint64, tile int64` | `ActionAck`，牌桌接受暗杠或补杠；补杠可能进入抢杠窗口 |
| PassSelf | 1381 → 1005 | `turn uint64` | `ActionAck`，放弃当前自选动作，进入出牌轮次 |
| Trustee | 1391 → 1005 | `enabled bool` | `ActionAck`，托管状态已设置 |

`DhmjSnapshot` 使用[麻将目录快照](#210-dhmj定海麻将凑桌与准备)的字段，`discards` 按座位组织，每座的牌放在 `tiles` 中。`hand` 只含本人暗手，`settlement` 也只带本人的资金结果。重登后通过 Enter 恢复原座，再使用 Snapshot 补场景。

Access 取当前连接的 UserID 和 LoginID，牌桌按服务器轮次、响应窗口和可选动作裁决。Respond 的动作编号见[麻将目录 RPC](#210-dhmj定海麻将凑桌与准备)。`ActionAck` 只确认当前请求，不附牌面或结算数据。请求超时后，可用 Snapshot 确认当前状态。

Access 支持推送 `1396 DhmjSettlementPush`，字段为 `table_name` 和本人 `settlement`，`request_id` 为 0。麻将目录在结算确认后向各在线玩家发送本人回执。未收到推送时，可通过快照恢复结果。

`1392 DhmjDiscardPush` 发给同桌在线玩家，`request_id` 为 0。字段为 `table_name string, revision/turn uint64, seat/tile int64`，`turn` 是本次出牌使用的轮次。开局后快照返回当前 `revision`。出牌、摸牌和补花共用序号，第一条事件从 1 开始。客户端可忽略快照已包含的事件，发现缺口时可重新请求快照。

摸牌和补花推送发给同桌在线玩家。两条消息都带 `table_name string, revision uint64, seat/remaining int64`，`remaining` 是当前牌墙余量，`request_id` 为 0。

| 推送 | 公开字段 | 本人字段 |
| --- | --- | --- |
| 1393 `DhmjDrawPush` | `turn uint64`，此次摸牌开启的轮次 | `tile int64`，补花后最终摸到的普通牌 |
| 1394 `DhmjFlowerPush` | `flowers int64[]`、`from_draw bool`，按顺序列出花牌，标记是否来自本次摸牌 | `replacements int64[]`，逐项对应花牌，0 表示终局未补牌 |

来自摸牌的补花链用 Flower 展示，用 Draw 将最终普通牌加入暗手一次。起手花替换按 Flower 更新本人暗手。

四人准备后发送 `1395 DhmjDealPush`，字段为 `table_name string, revision uint64, seat/dealer_seat/remaining int64, player_ids uint64[], hand int64[]`，`request_id` 为 0。`player_ids` 按 0～3 号座位排列，`seat` 表示接收者，`hand` 为本人 13 张起手牌。发牌是牌桌首条事件，先于补花和首摸。

`1397 DhmjTurnPush` 在操作阶段、活动座位、窗口、截止时间或活动座位的操作权变化时发给同桌在线玩家，`request_id` 为 0。牌桌在一次推进的其他事件发出后发送最终生效的阶段。快照的 `operation_phase` 与此处的 `phase` 对应。

| 字段 | 含义 |
| --- | --- |
| `table_name/phase string, revision/turn uint64, seat/active_seat int64` | 同桌事件序号，接收者座位和当前活动座位。阶段为 `discard`（出牌）、`self`（自选）、`respond`（响应）或 `finished`（终局） |
| `server_ms/deadline_ms int64` | 服务端当前时间与截止时间。终局截止时间为 0 |
| `window_id uint64, window_tile int64, rob_kong bool` | 响应窗口编号、牌值及是否抢杠。窗口外为 0 或 false |
| `offers/concealed_kongs/added_kongs int64[], can_self_win/can_discard bool` | 发给接收者本人的操作选项。响应用 `offers`，自摸和自杠候选仅在本人自选阶段提供 |

Access 已注册以下裁决推送，麻将节点的发送端见[后续工作](DESIGN.md#11-后续工作)。两条消息都带 `table_name string, revision uint64`，`request_id` 为 0。

| 推送 | 业务字段 |
| --- | --- |
| 1398 `DhmjMeldPush` | `seat/meld_index int64, meld DhmjMeld`。`seat` 是副露所属座位，`meld_index` 从 0 开始，补杠更新原碰的位置 |
| 1399 `DhmjRoundEndPush` | `round_end DhmjRoundEnd`。含 `kind string, optional source_seat/tile int64, rob_kong bool, winners list`，每个赢家为 `seat/point int64` |

`round_end.kind` 为 `draw`、`discard_hu`、`self_hu`、`kong_flower` 或 `eight_flowers`。点炮、抢杠和直杠开花带 `source_seat`。流局和八花省略胡牌张 `tile`。一炮多响的赢家放在同一条终局消息里，已确认的资金结果通过 1396 发给本人。`DhmjSnapshot` 也提供可选的 `round_end` 字段。

### 1.6 错误

| 项目 | 当前约定 |
| --- | --- |
| 通用错误 | S2C 9001 `ErrorResponse{code, message}`，关联原 request_id；不是正常业务响应 |
| 客户端错误类别 | 1=INVALID_REQUEST、2=UNSUPPORTED_MESSAGE、3=INTERNAL；使用框架安全文案，不直接暴露内部 Domain Code 或 SQL 诊断 |
| 错误的执行含义 | 收到错误或连接中断不能一概解释为操作未执行；明确的 target_unavailable 业务拒绝另见地鼠表 |

## 2. 节点间 RPC 调用

### 2.1 地址、参数与完成语义

表中位置参数按实际 Handler 顺序列出。`U=UserID uint64`、`L=LoginID string`、`N=AccessNode string`、`A=AgentName string`、`M=Membership string`，均由服务端填写；`map` 表示 `map[string]any`，`list` 表示 `[]any`。

| 项目 | 约定 |
| --- | --- |
| 定位 | `Node + ServiceName + CommandID`；节点来自 [nodes.yaml](../configs/nodes.yaml)，动态组名/桌名使用后端返回值，不自行拼接 |
| 命令归属 | CommandID 从属于 Service，不同 Service 可重复；公开命令由各自 Service 文件生成到 `common/rpc/<service>`，维护约定见 [Go 代码规范](GO-CODE-STYLE.md#331-service-命令与共享契约)，当前入口见 [Service 契约与寻址](DESIGN.md#23-service-契约与寻址) |
| 生成边界 | 名称和编号同步的检查不能代替参数、结果和完整接线验证；Service 公私标记不提供网络隔离，具体规则见 [共享契约边界](GO-CODE-STYLE.md#331-service-命令与共享契约) |
| 编码 | 直接使用 GSR Cluster ValueCodec；整数为规范 int64/uint64，集合为 map/list，不额外包装内部 Protobuf |
| 调用者 | 受信任内部节点/管理工具；Cluster Source 不充当用户身份 |
| Call | 有限等待，返回结果或错误；超时后远端可能已执行，不能盲目新建身份重试 |
| Send | nil 只表示本地接纳，不代表远端执行或客户端收到；结算通过 Call 获取完成结果 |
| 等待预算 | access 的后端 Call 最多 3s；mole/landlord 的游戏协调和结算等待分别见[地鼠故障边界](DESIGN.md#54-检查点与故障边界)、[斗地主恢复](DESIGN.md#63-重试清理与恢复)；Auth SQL 2s、Token I/O/本地 Call 500ms；结算 SQL 2s |
| 标识 | U 非零；Online/游戏/结算的字符串标识通常为 1～128 字节可见 ASCII，不裁剪、不改大小写；各入口额外限制另列 |

### 2.2 login：认证与凭据

Node 为 `login`，入口见 [AuthService](../nodes/login/authservice.go)、[TokenService](../nodes/login/tokenservice.go)，共享命令分别见 [Auth 契约](../common/rpc/auth/commands.gen.go)、[Token 契约](../common/rpc/token/commands.gen.go)。

| Service / Command | 正常调用方与方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| `auth / 1` PasswordAuth | Agent，Cluster Call | `username string, password string` | map：`user_id uint64, access_token string, expires_at_ms int64` |
| `token / 1` Issue | Auth，本地 Call；受信任签发入口 | `user_id uint64` | 同上，每次签发新凭据 |
| `token / 2` Verify | Agent，Cluster Call | `access_token string` | `user_id uint64` |
| `token / 3` Revoke | 受信任管理调用，Call | `access_token string` | bool；删除为 true，缺失为 false |

Verify 不续期，Revoke 不踢已登录连接。账号不存在和密码错误统一拒绝；接口不负责创建账号或钱包。Token 的 Redis 格式、TTL 和重启边界归[认证设计](DESIGN.md#32-密码认证与-token-登录)，不在协议文档重复展开。

### 2.3 online：当前位置

Node/Service 均为 `online`，见 [OnlineService](../nodes/online/onlineservice.go)，共享命令见 [生成契约](../common/rpc/online/commands.gen.go)。

| Command | 调用方与方式 | 顺序参数 | 成功结果与边界 |
| --- | --- | --- | --- |
| 1 Bind | Agent，Call | `U, L, N, A` | 被替换的位置 map；首次绑定或当前同地址重复提交返回 nil |
| 2 Lookup | 游戏进入协调，Call | `U` | 当前 map；无记录为 nil |
| 3 Unbind | Agent，Call；未确认登录的清理可 Send | `U, L` | bool；仅删除匹配的当前 LoginID |

位置 map 含 `login_id/access_node/agent_name string`。不同 LoginID 按目录实际处理顺序替换；当前 LoginID 绑定不同地址报冲突，不修改状态。已被替换或解绑的旧 LoginID 不得再次 Bind；有限的当前重复提交规则不是永久幂等。

Bind 超时可能已经生效，access 不自动重发，只尝试清理候选 LoginID 并关闭连接；Lookup 只给当时快照，不证明远端从未执行。位置命中不保证连接仍然存活；旧连接清理不会删除新绑定，Online 重启后需要显式登录重建。

### 2.4 mole：进入与组内操作

Node 为 `mole`。access Call 固定 [Entry](../nodes/mole/moleentryservice.go) 和 [MoleService](../nodes/mole/moleservice.go)，共享常量见 [Entry 契约](../common/rpc/moleentry/commands.gen.go)与 [Mole 契约](../common/rpc/mole/commands.gen.go)。`G=逻辑组标识 string`，沿用快照字段 `attack_name`，不是动态 ServiceName。

| Service / Command | 方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| `mole-entry / 1` Enter | Call | `U, L, N, A` | 地鼠快照 map，附 `silver`；Entry 协调身份核对、缺失余额查询与完整入座 |
| `mole / 2` Hit | Call | `U, L, G, actionID string, spawnSeq uint64, hole int64` | Hit 字段 map；accepted 附 `balance/available int64, revision uint64`；已拒绝原记录可无余额 |
| `mole / 3` Snapshot | Call | `U, L, G` | 与 C/S Snapshot 对应的快照 map |
| `mole / 4` Leave | Call | `U, L` | true；成员不存在也幂等成功，旧登录返回 stale_login；不取消已接纳账务 |

access 核对 Snapshot 回包的 `attack_name` 与当前逻辑组一致；成功 Leave 后清理组标识。断线即使未收到组名，也按当前 UserID、LoginID 向固定 `mole / 4` 尽力 Leave。非法回包处理见[失败与断线](DESIGN.md#33-失败替换与断线)。

内部余额视图字段为 `balance/confirmed_balance/available int64`、`version/revision uint64`；access 在 C/S 边界把 confirmed_balance/version 转成 ConfirmedSilver。只有 MoleService 明确返回 `mole.balance_required` 才查询 `settlement-read / 4`，已有内存账不接受查询结果覆盖。

同 LoginID/ActionID 的相同目标请求在记录保留期间返回原结果，冲突拒绝；服务端生成 HitID 并固定抽样，OperationID 沿用 HitID。客户端结果未知不换 ID 重抽，不经 Entry 查询原 HitID。组、成员和内存账的同步规则及恢复边界见[地鼠设计](DESIGN.md#5-地鼠即时玩法与后台入账)。

### 2.5 landlord：凑桌与牌桌操作

Node 为 `landlord`，正常调用者为 access Agent。入口见[目录](../nodes/landlord/landlordservice.go)与[牌桌](../nodes/landlord/attackservice.go)。

共享命令分别见 [目录契约](../common/rpc/landlord/commands.gen.go)、[AttackService 契约](../common/rpc/landlordattack/commands.gen.go)。目录含固定 ServiceName，牌桌只生成命令；共享编号不改变各 Handler 的实际调用边界。

| Service / Command | 方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| `landlord / 1` Resolve | Call | `U, L` | map：`room_name/membership string`，本次预留有效 30 秒；重试会生成新 Membership 并更新期限 |
| `RoomName / 1` Join | Call | `U, L, N, A, M` | 本人桌快照 map；核对 Online、目录成员实例及预留期限；过期返回 stale_login |
| `RoomName / 2` Ready | Call | `U, L` | true；钱包至少 20，未冻结资金；失败保持原桌状态，发牌后再 Ready 返回 busy |
| `RoomName / 6` Call | Call | `U, L, turn uint64, yes bool` | true；叫/不叫 |
| `RoomName / 7` Rob | Call | `U, L, turn uint64, yes bool` | true；抢/不抢 |
| `RoomName / 3` Play | Call | `U, L, turn uint64, cards list`，元素为 int64 | true；空列表表示不出 |
| `RoomName / 9` Trustee | Call | `U, L, enabled bool` | true 表示已达到请求的托管状态；状态变化时广播 Trustee |
| `RoomName / 4` Snapshot | Call | `U, L` | 本人桌快照 |
| `RoomName / 5` Leave | Call | `U, L` | true 表示本桌已接纳离开，Agent 随后清路由并返回 C/S `ActionAck`；回收边界见[设计](DESIGN.md#63-重试清理与恢复) |
| `landlord / 5` Discard | Call | `roomName string` | 无业务结果；仅确认桌已消失时清理路由再重新 Resolve |

快照 map 与 C/S LandlordSnapshot 字段对应。access 核对 Join、Snapshot 回包的 `room_name`，并将 Ready、Call、Rob、Play、Trustee、Leave 的 `true` 转为 `ActionAck`。异常回包走[非法回包流程](DESIGN.md#33-失败替换与断线)。Join 超时可能已加入，只有明确目标不存在、已关闭或已崩溃才走 Discard 并重新选桌。目录和牌桌之间的本地消息见[节点内协作](#29-节点内协作)。

Join 失败仍可能伴随旧成员回收的更新推送；该推送不代表本次进入成功，Call 继续返回原错误。成功 Join 广播入座事件，回收通知规则见[设计](DESIGN.md#63-重试清理与恢复)。

### 2.6 通用银子结算接口

mole、landlord 通过 `settlement_node` 指定目标（默认 `settlement`），dhmj 固定调用 `settlement`。入口见[Call 写入池](../nodes/settlement/settlementwriteservice.go)与[Call 读取池](../nodes/settlement/settlementreadservice.go)。

| Service / Command | 调用方式 | 顺序参数 | 完成结果 |
| --- | --- | --- | --- |
| `settlement-write / 1` SettleBatch | Call，地鼠使用 | Batch 请求 map | 完整回执或错误；各笔单人操作分别判定 |
| `settlement-write / 6` SettleSingle | Call | Single 请求 map | 完整回执或错误；一笔单人操作 |
| `settlement-write / 7` SettleEqualGroup | Call，斗地主使用 | EqualGroup 请求 map | 输方各自封顶、赢家均分；返回每人的实际扣返和余额 |
| `settlement-write / 8` SettleTransferGroup | Call | TransferGroup 请求 map | 按付款明细比例封顶；返回实际付款明细、每人实际扣返和余额 |
| `settlement-write / 9` SettleTransferGroupWithFees | Call，定海麻将使用 | TransferGroupWithFees 请求 map | 同一事务先扣全员手续费，再按付款明细封顶；回执单列实扣手续费 |
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
| EqualGroup 请求 | `version uint64, operation_id/business/business_ref string, changes list, detail map` | 一笔多人整体操作，2～64 名不同参与用户，每人一条 change |
| 多人明细 change | `user_id uint64, reason string, debit int64, credit int64` | 输家正 Debit、赢家零 Debit；具体均分约束见下文 |
| TransferGroup 请求 | `version uint64, operation_id/business/business_ref string, transfers list, detail map` | 一笔多人操作；每项为 `from_user_id uint64, to_user_id uint64, amount int64` |
| TransferGroupWithFees 请求 | TransferGroup 的全部字段 + `fees list` | 每项为 `user_id uint64, amount int64`，列出全部 2～64 名参与者，每人一项，金额可为 0 |

地鼠每锤一个单人 Operation，items 保留 `hammer_cost` 与 `hammer_reward` 两类，即使奖励为零也保留；斗地主每局一个 EqualGroup，三人的 changes 分类为 `game_win_loss`。settlement 不解释概率、牌型或业务输赢规则，detail 仅作归档。

Group 接口的规则及限制：

- **7 均分**：采用 EqualGroup 请求结构，每人一条 change；正 Debit 表示输家且 Credit 必须为 0，Debit 为 0 的条目表示赢家（理论 Credit 可为 0，非参与者不应列入）。至少一输一赢，理论总扣款等于总返还；赢家理论 Credit 按理论奖池均分，余数按赢家 UserID 从大到小分配。每个输家按事务内余额独立封顶，实际扣款合计重新均分赢家，余数采用相同顺序；余额不足不由其他输家补足。
- **8 明细**：`transfers` 为 1～512 条，不同付款人与收款人均为非零 UserID，amount 为正，每对付款人/收款人最多一条，全部参与者为 2～64 人。每位付款人按事务开始时的钱包余额独立封顶，实付款按原应付比例向下取整，余数按收款 UserID 从大到小分配；同组收入不能提前用于付款。请求明细顺序不影响身份，实际明细按付款、收款 UserID 升序返回。
- **9 明细加手续费**：每人实扣手续费为应收手续费与事务内锁定余额的较小值。扣费后的剩余余额用于接口 8 的付款分配，同组入账不能抵付手续费或本组付款。付款双方都必须列在 `fees` 中，仅收手续费时可传空 `transfers`。费用按 UserID 升序返回。
- 牌局实际付款合计等于返还合计。接口 9 的总实扣等于牌局实付与手续费之和。所有人实际扣返均为零时仍返回 committed，钱包余额和版本不变。每个发生实际扣款或返还的钱包版本增加一次。余额、版本或请求金额溢出返回错误并回滚，不保存财务终态。

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
| Group 的 OperationResult | 基础字段 + `actual_changes list`。8/9 增加 `actual_transfers list`，9 增加 `actual_fees list` | `actual_changes` 每项为 `user_id uint64, debit/credit int64`；实际付款和费用沿用请求字段，amount 可以为 0。接口 9 的 debit 含手续费，减去该用户的 `actual_fees.amount` 即为牌局实付 |
| balances 项 | `user_id uint64, amount int64, version uint64` | 本次提交后的余额；原批次重提返回原历史回执，不能覆盖调用方较新的版本 |

Call 成功返回回执后，财务结果仍看每项 status。1/6 的 rejected reason 为 insufficient_silver、balance_overflow 或 version_overflow；committed 时为空。7～9 对余额不足采用封顶，成功结果为 committed；其余额/版本溢出返回错误，不写入 rejected。接口 3 仅返回基本 OperationResult。

OperationID 是全局财务身份，BatchID 是提交身份。同身份同内容复用原终态，不重复扣返；不同内容或切换接口策略报 identity_conflict。摘要由规范化内部结构的 JSON 作 SHA-256：单人及均分明细按 UserID/Reason/Debit/Credit 排序但不合并，批次操作顺序保持有意义。接口 8/9 的付款明细按付款、收款 UserID 排序，接口 9 的费用按 UserID 排序。Single、均分、付款明细、付款明细加手续费的内部 BatchID 分别使用 `single-`、`group-equal-`、`group-transfer-`、`group-transfer-fees-` 加 OperationID 的 SHA-256 十六进制。

斗地主由本桌直接 Call 写池，校验原 OperationID、终态和三名参与者余额后推送各自的结算事件，再清理待记录。结果未知保留原请求，确认后不再调用结算；等待预算、重试及恢复边界见[斗地主恢复设计](DESIGN.md#63-重试清理与恢复)。

地鼠由 Submission 直接 Call 写池并校验 BatchID、逐项结果和全部参与者余额；结果未知按原批次退避重试。有效回执先完成 Redis 检查点，再通过节点内消息交付 MoleService。等待与失败边界见[地鼠检查点设计](DESIGN.md#54-检查点与故障边界)。

写、读分别共用一个池名，由 GSR 在对应池内选择成员并返回结果，不传回执地址。Call 超时后必须按原 ID 和内容重试；池化作用见[入口与执行边界](DESIGN.md#71-入口与执行边界)，Call 与 SQL 的计时关系见[调用与超时](DESIGN.md#72-调用与超时)。钱包只能由独立管理入口初始化，普通 RPC 不创建钱包或提交 wallet_init。

### 2.7 回调 access：快照、校正与旧登录替换

目标均为对应玩家的 `AccessNode / AgentName`，以 Cluster.Send 调用。业务命令同属动态 Agent Service，共享常量见 [Agent 契约](../common/rpc/accessagent/commands.gen.go)，不包含固定 ServiceName。Agent 先核对当前 LoginID，游戏组件另核对自身路由。

| 来源 / Command | 顺序参数 | 接收行为 | 对应 C/S 消息 |
| --- | --- | --- | --- |
| mole / 100 SnapshotPush | `L, snapshot map` | 核对当前组，推送本人快照 | 1061 |
| mole / 102 BalanceCorrection | `L, balance map` | 当前登录且仍在地鼠中才推送绝对余额 | 1062 |
| landlord / 111 SeatPush | `L, event map` | 核对当前桌，转发坐下、准备取消或离座 | 1191 |
| landlord / 112 ReadyPush | `L, event map` | 转发准备事件 | 1192 |
| landlord / 113 DealPush | `L, event map` | 转发本人的发牌 | 1193 |
| landlord / 114 BidPush | `L, event map` | 转发叫抢事件 | 1194 |
| landlord / 115 PlayPush | `L, event map` | 转发出牌事件 | 1195 |
| landlord / 116 SettlementPush | `L, event map` | 转发本人的结算回执 | 1196 |
| landlord / 117 TrusteePush | `L, event map` | 转发托管状态和操作时间 | 1197 |
| landlord / 118 TurnPush | `L, event map` | 转发动画结束后的轮次开放 | 1198 |
| dhmj / 119 DhmjSettlementPush | `L, payload map`，含 `table_name string, settlement map` | 核对当前登录、牌桌和回执 UserID，再转发 committed 回执 | 1396 |
| dhmj / 120 DhmjDiscardPush | `L, payload map`，含 `table_name, revision, seat, turn, tile` | 核对当前登录和牌桌，校验出牌字段并转发 | 1392 |
| dhmj / 121 DhmjDrawPush | `L, payload map`，字段同 C/S | 核对当前登录和牌桌，按接收座位控制 `tile` | 1393 |
| dhmj / 122 DhmjFlowerPush | `L, payload map`，字段同 C/S | 核对当前登录和牌桌，按接收座位控制 `replacements` | 1394 |
| dhmj / 123 DhmjDealPush | `L, payload map`，字段同 C/S | 核对当前登录、牌桌、接收座位和名单中的本人身份，再转发本人起手牌 | 1395 |
| dhmj / 124 DhmjTurnPush | `L, payload map`，字段同 C/S | 核对当前登录、牌桌和接收座位，拒绝阶段或本人选项不一致的消息 | 1397 |
| 麻将副露接收端 / 125 DhmjMeldPush | `L, payload map`，字段同 C/S | 核对当前登录和牌桌，转发生效的副露 | 1398 |
| 麻将终局接收端 / 126 DhmjRoundEndPush | `L, payload map`，字段同 C/S | 核对当前登录和牌桌，转发完整终局裁决 | 1399 |
| 新登录 Agent / 101 Replaced | `oldLoginID string` | 仅匹配旧登录时清理游戏、条件 Unbind 并退出 | 关闭旧连接，无专用业务响应 |

推送 Send 接纳不等于客户端送达，不以玩家确认收包作为结算幂等或 Redis 清理条件。结算 Call 的回执由游戏处理，再按各自协议返回或推送客户端结果。

### 2.8 内部错误

以下是服务端稳定 Domain Code 及相关 GSR 错误；C/S 只收到第 1.5 节的框架安全错误。参数类型/数量错误与未知命令沿用 GSR `runtime.invalid_command_arguments / runtime.unknown_command`。

| 节点/能力 | 错误码 | 调用方处置 |
| --- | --- | --- |
| access 准入 | `access.login_required / enter_required / already_logged_in` | 拒绝当前请求；认证或登录请求失败时关闭当前连接 |
| access 游戏进入 | 保留原 RPC 错误 | 进入超时、取消、链路故障或未分类 Handler 失败时封闭后续请求并由 Gate 有界写出后关闭；明确业务拒绝、邮箱满或目标消失不按未知结果处理。已知路由的清理及无路由边界见[失败、替换与断线](DESIGN.md#33-失败替换与断线) |
| access 回包转换 | `access.invalid_backend_reply` | 客户端请求先封闭后续业务，经 Gate 尝试写出唯一安全错误后关闭，并尽力清理已知游戏路由和 Online；生命周期见[失败、替换与断线](DESIGN.md#33-失败替换与断线) |
| 认证 | `login.invalid_request / invalid_credentials / store_unavailable` | 不建立登录；账号不存在与密码错误不区分 |
| Token | `auth.invalid_token / token_store_unavailable` | 不放行，后者不能降级绕过 Redis 校验 |
| Online | `online.invalid_request / login_id_conflict` | 不修改非法目录状态，不复用旧登录身份 |
| mole | `mole.invalid_request / stale_login / conflict / busy / target_unavailable / online_unavailable / accounting_unavailable / insufficient_silver / operation_unknown` | 区分明确拒绝与未知结果；target_unavailable 的 C/S 特例见 1.3 |
| landlord | `landlord.invalid_request / stale_login / invalid_turn / invalid_cards / busy / insufficient_silver` | 不盲目重复出牌；当前轮次与状态用快照核实 |
| landlord 保存待结算请求 | `landlord.invalid_request / operation_conflict` | 分别表示保存内容无效、相同 ID 内容冲突；暂停本桌自动提交，保留原请求及已有记录；详见[斗地主恢复边界](DESIGN.md#63-重试清理与恢复) |
| mole 结算调用接线 | `runtime.unknown_command / runtime.invalid_command_arguments` | Submission 按确定性错误暂停，保留原请求；恢复边界见[地鼠故障边界](DESIGN.md#54-检查点与故障边界) |
| landlord 结算调用接线 | `runtime.unknown_command / runtime.invalid_command_arguments` | 暂停本桌自动提交，保留原请求；恢复边界见[斗地主恢复边界](DESIGN.md#63-重试清理与恢复) |
| settlement 临时故障 | `settlement.store_unavailable` | 保留原请求，按游戏 owner 的限频策略重试；框架 ErrMailboxFull 同样退避，不能当成财务 rejected |
| settlement 确定性故障 | `settlement.invalid_request / identity_conflict / wallet_not_found / balance_overflow / version_overflow` | 保留请求并暂停自动重试，核查身份、格式、钱包或数值范围；后两项为新 Group 接口的错误 |

表中同一行的省略项沿用第一个完整前缀，例如 `login.store_unavailable`。本地 Redis/通知失败的恢复与退避由各游戏管理，详见设计文档，不新增通用重试器。

### 2.9 节点内协作

节点内协作使用本地 Call 或 Send。地鼠的 Entry、MoleService 和 Submission 通过消息协作，组与内存账之间直接调用普通函数。

| 调用方 → Service / Command | 方式 | 顺序参数 | 结果与来源限制 |
| --- | --- | --- | --- |
| Entry → `mole / 1` Enter | Call | `U, L, N, A, seed map`；seed 可为 nil，否则为 `amount int64, version uint64` | 完整快照及 silver；缺账户且无 seed 返回 balance_required，不改状态；仅非零本地来源 |
| Pulse → `mole / 5` Tick | Send | 无 | 先归批，再推进一批到期刷新；仅非零本地来源 |
| MoleService → `自身 ServiceID / 12` Refresh | Send | 无 | 分批刷新剩余到期组；仅接受自身来源，最多一条续批在途；预算与失败边界见[刷新调度](DESIGN.md#54-检查点与故障边界) |
| Submission → `mole / 6` InitAccounting | Send | `pool molePool, sequence uint64` | 初始化恢复统计；仅非零本地来源 |
| Submission → `mole / 7` SettlementReceipt | Send | `receipt batchReceipt` 独立快照 | 应用回执与余额校正；仅非零本地来源 |
| Submission / Pulse → `mole / 8` Pause | Send | 无 | 暂停新记账；仅非零本地来源 |
| MoleService → `mole-submission / 1` Submit | Send | `batch moleBatch` 独立快照 | 接纳原批次，不代表完成结算 |
| Pulse → `mole-submission / 2` Tick | Send | 无 | 恢复、结算重试及回执交付 |

内部验收入口为 `mole / 9` Balance（`U` → 余额视图）、`mole / 10` Pool（无参数 → 确认统计）、`mole / 11` OperationStatus（`U, hitID string` → 状态 map 或 nil）及 `mole-submission / 3` State。它们未接入客户端 MsgID，也不形成网络访问隔离。

以下斗地主调用从 AttackService 发往本节点目录；牌桌 Tick 由本地 Pulse Send，继续拒绝远程来源。

| Service / Command | 方式 | 顺序参数 | 结果 |
| --- | --- | --- | --- |
| `landlord / 3` CheckRoute | Call | `U, L, roomName string, M` | bool；当前预留身份匹配且尚未过期，或已解除期限 |
| `landlord / 4` Seal | Send | `roomName string, sealed bool` | 封桌接纳，无业务回执 |
| `landlord / 2` Release | Call | `U, L, roomName string, M, waiting bool` | false：同桌有新预留，保留原座位；true：旧成员可移除 |
| `landlord / 6` ReconcileReservations | Call | `roomName string, members [3]landlordMember`；元素只含 UserID/LoginID/Membership，空位 UserID=0 | bool；true：目录仍占位，false：没有成员或有效预留；仅接收非零本地来源 |
| Pulse → `RoomName / 8` Tick | Send | 无 | 推进超时、结算重试及清理；仅非零本地来源 |

6 号使用 Go 值类型身份快照，不传手牌、不做内部 Protobuf 编解码。入座确认、到期回收、重登保护和空桌退出见[斗地主设计](DESIGN.md#61-凑桌规则与可见性)及[恢复边界](DESIGN.md#63-重试清理与恢复)。完整注册就近见[地鼠 MainService](../nodes/mole/main_service.go)、[斗地主目录](../nodes/landlord/landlordservice.go)及各 Service 源码。

### 2.10 dhmj：定海麻将凑桌与准备

Node 和 Service 均为 `dhmj`。目录持有座位、准备和登录路由，开局后通过本地 Call 访问牌桌。Access 通过 [comDhmj](../nodes/access/comdhmj.go) 调用目录，命令定义见[共享命令](../common/rpc/dhmj/commands.gen.go)。

| Command | 方式 | 顺序参数 | 成功结果 |
| --- | --- | --- | --- |
| 1 Join | Call | `U, L, N, A` | 校验 Online 后分配座位，重登恢复原座并更新当前登录。返回本人快照 |
| 2 Ready | Call | `U, L` | true。钱包至少 3000，四人准备后创建牌桌 |
| 3 Snapshot | Call | `U, L` | 本人快照，字段见下方 |
| 4 Leave | Call | `U, L` | true。等待中或结算已确认时释放在线座位。开局后至结算确认前，断开当前登录并保留座位；重登期限见[麻将设计](DESIGN.md#10-定海麻将随机房与局末结算) |
| 5 Discard | Call | `U, L, turn uint64, tile int64` | true。当前座位出牌 |
| 6 Respond | Call | `U, L, windowID uint64, action int64` | true。提交对当前窗口的选择，裁决可能仍在等待其他座位 |
| 7 SelfWin | Call | `U, L, turn uint64` | true。确认自摸 |
| 8 Kong | Call | `U, L, turn uint64, tile int64` | true。暗杠或发起补杠，补杠可能开启抢杠窗口 |
| 9 PassSelf | Call | `U, L, turn uint64` | true。放弃当前自选动作，进入出牌轮次 |
| 10 Trustee | Call | `U, L, enabled bool` | true。设置托管状态 |

Respond 动作编号为 1 右吃、2 中吃、3 左吃、4 直杠、5 碰、6 胡、7 过。两类轮次编号分别使用快照中的 `turn` 和 `window_id`。目录按当前登录查出牌桌和座位，调用者无需提供座位。

目录与牌桌之间的回收和事件消息仅在本节点使用。

| 调用方 → 目标 | 命令 | 顺序参数 | 行为 |
| --- | --- | --- | --- |
| 牌桌 → 目录 | Send 11 | `tableName, operationID string, receipt [4]roundSilver` | 按发送者 ServiceID 核对牌桌，转发在线玩家的本人回执并开始计算离线座位的重登窗口 |
| 目录 Pulse → 目录 | Send 12 | 无 | 回收到期离线座位，重试关闭已空牌桌 |
| 牌桌 → 目录 | Send 13 | `tableName string, event roundEvent` | 目录核对来源，转发发牌、出牌、摸牌、补花和轮次；私有牌面和选项只随本人消息发送 |
| 目录 → 牌桌 | Send 9 | 无 | 记录关闭意图，结算待记录清理完成后牌桌退出 |

| 本人快照 | 字段 |
| --- | --- |
| 等待、进行中及结束 | `table_name string, seat int64, phase string, players list`。`players` 元素含 `user_id uint64, seat int64, ready bool` |
| 开局后公开状态 | `active_seat int64, turn/window_id uint64, window_tile/server_ms/deadline_ms int64`。`discards/hand_counts/trustees` 分别按座位返回弃牌列表、暗手数量和托管标志。无对应窗口或截止时为 0。`operation_phase string, rob_kong bool` 与轮次推送的阶段和抢杠标记一致 |
| 开局后公开副露与花牌 | `seat_tiles list` 按 0～3 号座位排列，每项含 `flowers list`、`melds list`。副露项含 `kind string, source_seat int64, tiles list`，种类为 `eat`、`pong`、`direct_kong`、`concealed_kong`、`added_kong`。暗杠牌值公开，来源为本人座位；补杠保留原碰的来源。牌值为 int64 |
| 开局后本人状态 | `hand` 为本人暗手。`offers` 为本人尚未提交的窗口可选动作编号，有选项时也可过。`self_action/can_self_win/can_discard bool` 表示本人能否自选、自摸或出牌。`concealed_kongs/added_kongs` 为暗杠、补杠候选牌值。牌值和动作编号均为 int64 |
| 终局结算 | 可选 `settlement` map。已生成请求时返回 `operation_id/status string`。committed 时包含本人 `user_id uint64, fee/paid/received/balance int64, version uint64`。`paid` 与 `received` 只统计本局实际付款，手续费单列 |

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
  → Agent Call mole/mole-entry/1
  → Entry Call online/online/2
  → 本地 Call mole/1 Enter
  → 仅缺余额：Call settlement-read/4，再本地 Call mole/1
  ← 1012：一次内存回调完成选组、入座、账户初始化并返回快照和 silver

1021 Hit
  → Agent Call mole/mole/2，携逻辑组标识
  → 私有组校验目标、固定 HitID 与扣返，普通函数调用内存账
  ← 1022：首次接纳返回 HitID 和内存余额，客户端可继续
```

### 3.3 地鼠后台入账与差异校正

```text
MoleService/accounting.flush 本地 Send 原批次给 Submission
  → Submission 保存 Redis 原批次
  → Call settlement-write/1（原 BatchID 和内容）
  → 池成员提交 MySQL，直接返回完整回执或错误
  → Submission 校验回执，完成 Redis 统计和检查点；失败保留原请求/已收到的回执
  → 本地 Send 完整回执给 MoleService/accounting.applyReceipt，应用确认余额并释放待扣
  → 仅显示余额有差异时：Send 当前 Agent/102 → C/S 1062
```

### 3.4 斗地主一局与三人结算

```text
1101 Enter → Call landlord/landlord/1 → Call RoomName/1 Join
  → 桌核对 Online 与本地成员实例 → 返回 1102 本人快照
1111 Ready → Call RoomName/2 → Call settlement-read/4 准入
  → 广播准备，全员就绪时发牌 → 返回 1005 ActionAck
1171 Call / 1181 Rob / 1121 Play → 桌 / 6、7、3（携带当前 Turn）
  → 桌更新状态、Send 事件给三名玩家
  → 最后一手固定 OperationID 与三人金额，并在本次回调尝试结算
  ← 1005 ActionAck：操作已执行

局末尝试结算（最后一手发起，待确认时由 Pulse 推进）
  → 保存 Redis 待请求后 Call settlement-write/7
    ├─ 有效回执 → applyReceipt → Send 当前 Agent/116 → C/S 1196 个人结算结果
    │  → 删除 Redis 待记录，删除失败只重试删除
    └─ 结果未知 → 保留同一 OperationID 和金额，按原请求重试
```

两个游戏在回执未知时都重提原 ID 和原内容，不追加玩家查账。重启只恢复已经保存的账务请求，不恢复未保存命中或进行中手牌；边界见[持久化与故障汇总](DESIGN.md#8-持久化与故障边界汇总)。


### 3.5 定海麻将终局

```text
牌桌结束 → Pulse 唤醒牌桌 → 从规则计算理论付款
  → 保存本局请求到 Redis → Call settlement-write/9
  → 校验完整回执 → 本人快照可读实扣费用、扣返与余额
  → 清理 Redis 待记录，失败时重试删除
  → Send 通知目录 → Send access Agent/119 → 推送本人结算 1396
```
