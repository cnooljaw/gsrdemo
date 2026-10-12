# GSR 业务验证项目方案（修订版）

日期：2026-10-12。本文是实施方案；尚未通过运行验证。恢复策略等新增取舍按本文建议实施，不把它们作为 GSR 框架契约。

## 1. 范围与边界

主目录为 `gsrdemo`，一个 Git 仓库管理多个独立项目：每个节点角色一个 Go module，共享代码放独立 `common` module，`typeclient` 是独立 TypeScript 项目。同角色的多个节点实例共用项目，以配置区分。节点依赖 `github.com/cnooljaw/gsr` 和 common，不导入其他节点的实现包。`../gsr`、`../../skynet_bt/nbgameserver` 与 `../GoServerActorFsm` 严格只读；源码、协议、配置和生成结果全部放在本项目。GSR 1.0 为本地完成基线，尚未发布；每个节点 go.mod 以占位版本 v0.0.0 加本地 replace 引用只读 ../gsr。本地 go.work 只组装本仓库 module，关闭 workspace 也沿用各节点的本地依赖声明。依赖及参考 revision 见 [REFERENCES.md](REFERENCES.md)。本地 replace 不冻结源码，GSR checkout 变化需核对基线；以后正式发布再改为固定发布版本。

首版仅使用整数积分，不涉及真钱。先用 TypeScript 命令行客户端和机器人，完成“用户名密码登录 → 打地鼠 → 持久化积分 → 故障恢复”，再加入三人斗地主。Redis 不进入首版闭环。

不新增 UserManager、通用 GameService 基类、推送代理、通用 worker 框架、消息队列或分布式事务框架。

## 2. 节点与 Service

每个进程一个 Runtime、一个 Cluster。六种节点角色足够，按阶段启动实际需要的角色。

| 节点 | 内部运行单元 | 职责 |
| --- | --- | --- |
| access-01 | Gate、每连接一个 Agent | 唯一客户端接入、连接状态机、协议准入、认证身份、已确认位置/阶段快照、客户端写回 |
| login-01 | 少量有界 AuthService | 账号校验、第三方验证、身份映射 |
| online-01 | OnlineService | 用户位置与迁移状态机、绑定/进入/离开协调、房间阶段投影 |
| mole-01 | RoomDirectory、每组一个 MoleRoomService、SettlementRecoveryService | 分组与座位、共享地鼠、个人计分、持久化全组局末结果、恢复结算 |
| landlord-01 | RoomDirectory、每房间一个 LandlordRoomService、SettlementRecoveryService | 房间创建、座位/手牌/回合、持久化局末结果、恢复结算 |
| settlement-01 | SettlementService | 积分、结算记录、幂等账本 |

MainService 显式创建并命名 Service，开放 Cluster/Gate。RoomDirectory 只创建/查找房间、为打地鼠分配可加入的等待组、维护本节点生命周期索引、进行有界的超时巡检，不中转命中和出牌。结算恢复 Service 是有独立等待和重试需求的业务运行单元，不是新节点。

第一轮启动 access、login、online、mole、settlement 五个进程；阶段 1 可先验收前四个进程。完整闭环后增加 access-02，再增加 landlord。

## 3. 状态所有权

| owner | 权威状态 | 禁止承担的职责 |
| --- | --- | --- |
| Agent | 本连接 UserID、LoginID、连接状态机；持有已确认位置、游戏路由和房间阶段快照 | 积分余额、手牌、命中判定、自行确认位置变化 |
| Login | 账号和第三方身份到 UserID 的映射 | 在线目录、游戏状态 |
| Online | 当前接入/大厅/房间位置、进行中的位置迁移；持有房间阶段的版本投影 | 游戏阶段自行推进、游戏规则、积分修改 |
| RoomService | 该房间全部游戏状态、玩家当前 LoginID/推送地址、绑定令牌、回合和状态版本 | 持久化积分余额修改 |
| SettlementRecoveryService | 待结算任务的提交、退避、状态查询和本地完成标记 | 重新计算结果、修改余额 |
| SettlementService | 持久化积分、已提交结算和账本 | 决定胜负、接受客户端奖励值 |

MySQL 是外部依赖；`*sql.DB` 连接池由进程宿主管理，在 Runtime 停止后关闭。连接池可以并发使用，业务对象和可变游戏状态不能跨 Service 共享。跨消息传递的是独占快照。

Online 记录包含：UserID、LoginID、接入 Node/AgentName、大厅/房间位置、游戏 Node/RoomID/RoomName、BindingToken、LocationVersion、房间阶段及 RoomStateVersion、连接状态、目录操作 OperationID、流程状态。RoomID 跨重启不复用；RoomName 含 RoomID，不能让旧路由命中新建房间。

LoginID 是随机且不可复用的业务登录关联标识，不是 Core Session，也不向 Runtime 引入 epoch/lease。绑定令牌是 RoomService 的业务比较并更新凭据，不是 ServiceID。

用户是有状态的。客户端始终只连 access，Agent 依据连接状态、已确认位置及房间阶段决定准入协议，再由 Room 验证业务操作。Online 协调位置变化并持有低频阶段投影；两者通过有版本的快照同步，不共同修改一份可变 User 对象。状态、允许协议表、消息同步及失败用例见 [用户状态契约](USER-SESSION.md)。

## 4. 登录、进房与连接切换

### 4.1 登录

1. 客户端始终只连接 access 的 Gate，由 Agent 调用 login 节点验证用户名密码，取得统一 UserID；进房、游戏和结算查询不切换客户端连接目标。
2. Agent 在已经 Running 的协议 Handler 中生成 LoginID，注册连接私有名称，例如 `agent/<LoginID>`；注册成功后才发布推送地址。不在 Agent.OnStart 中提前注册。
3. Agent 调用 Online.Bind，携带由服务端产生的 UserID、LoginID、接入地址和 OperationID。
4. Online 串行决定该用户的切换顺序。尚未进房的绑定可直接完成；已有房间的绑定必须先完成房间连接替换，再确认新绑定。
5. Agent 收到已确认结果后进入可操作状态；超时只表示结果未知，使用同一 OperationID 查询或重试，不能自行当成失败并重新创建一次迁移。

“后登录生效”按 Online 接纳并完成绑定的顺序定义，不按客户端时间或随机 LoginID 大小定义。认证成功与绑定成功分开返回；绑定未确认的 Agent 不能操作游戏。

密码使用带随机盐的 Argon2id，参数随哈希保存；根据部署硬件测量成本，限制认证实例数量、输入长度和请求速率，避免哈希并发耗尽内存。参考 [OWASP 密码存储建议](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)。首版用管理命令创建测试账号，不先建设注册/找回密码产品流程。

### 4.2 进房与重连的协调

控制链路为：`Agent → Online → 游戏节点 RoomDirectory/RoomService → Online 确认 → Agent`。这是对原方案调用链的细化：Online 协调低频的位置变化，热点游戏命令仍从 Agent 直达房间。

同一用户最多一个未完成的位置变化。流程状态至少区分 `active`、`transitioning`、`uncertain`、`disconnected`。每次操作保存旧位置、目标位置和 OperationID，收到可确认结果后才切换为 active。

RoomService 提供业务 `CompareAndBind`：请求包含 RoomID、UserID、OperationID、ExpectedBindingToken、目标 LoginID 和推送地址。只在当前令牌匹配时替换玩家绑定，同时产生新令牌。同一 OperationID、相同内容重试不重复应用；内容不同返回冲突。

初次入座也使用房间分配的令牌；移除玩家后仍保留对应代次信息或操作墓碑，不能简单重置为空而允许迟到请求重新匹配。房间销毁后新房间使用新 RoomID。幂等记录有明确保留范围，过期操作被拒绝，不能重新执行。

例如 A→B 已完成，随后 B→C 已完成，迟到的 A→B 请求因旧令牌不匹配不能把 C 改回 B。不能把“LoginID 不同就覆盖”作为重连规则。

Call 超时后，Online 保留该用户的 uncertain 操作，查询目标房间实际绑定/操作结果，或以相同令牌和 OperationID 重试；确认结果后才允许下一次位置变化。一次“尚未找到”不能证明先前请求不会迟到执行。其他用户可以继续绑定，每次 Handler 的外部等待必须有有限预算。

离开也通过 Online 协调：房间移除确认后再清理目录，Agent 收到确定结果才返回大厅并移除游戏路由。首个完整 demo 只在 filling、settled 阶段允许离开；进行中及未完成结算阶段保留本局位置。斗地主断线只标记 disconnected，保留座位并按游戏规则托管。位置清理是带 LoginID/操作令牌条件的更新，禁止无条件按 UserID 删除。

### 4.3 旧连接与操作授权

Agent 从自身已认证状态填入 UserID/LoginID，客户端不能选择别人的身份或任意房间路由。RoomService 校验 RoomID、玩家身份、当前 LoginID、局号和相关操作约束。

新绑定成功的含义是房间已替换 LoginID，旧连接之后到达的操作、断线及离开通知均不能改变新绑定。替换之前已经执行的合法旧操作不回滚。踢旧连接的 Send 只是通知，不承担授权正确性。

断线通知在 Agent.Handle 中发起有界清理，然后 Exit；清理失败允许目录暂时滞后，由后续重连/巡检纠正。Agent.OnStop 负责关闭连接，不依赖停机阶段还能发起新的 Cluster Call。

## 5. Online 故障与重启

Online 不可用时拒绝新绑定、进房、跨房间迁移和重连绑定；已确认的 Agent 依靠缓存路由继续操作原房间。Online 不被每次命中/出牌查询。断线清理失败不意味着房间释放座位。

首版建议采用保守且明确的重启策略：Online 重启后不立刻把空目录当作“所有用户已离线”。关闭接入的新登录/进房入口，协调停止旧 access 和游戏进程，确认旧进程退出，再启动 Online、游戏节点及 access。进行中的房间终止，客户端重新认证；游戏节点启动恢复已持久化待结算结果。login、settlement 无需随之重启。

此策略牺牲 Online 重启时的游戏可用性，但避免空目录、新房间和仍活着的旧房间同时拥有同一用户。若某个旧游戏进程无法确认退出，保持新绑定入口关闭，不把网络不可达当作进程已死亡。

普通 access 重启无需此全局流程：RoomService 保留原 LoginID/座位；用户在另一 access 登录，由 Online 通过房间比较并更新完成重连。普通游戏节点崩溃不恢复进行中的房间；其重启确认旧房间已消失后，按条件清理对应目录，再允许相关用户进入新房间。

保留房间的 Online 在线重建是后续可用性改进，需要冻结位置迁移、隔离旧在途命令、枚举权威房间绑定并处理期间变更；首版不把“启动后拉取一次列表”当成完整重建。

## 6. 游戏命令、幂等和推送

热点链路：`Client → Agent → Cluster.Call(RoomName, Command) → Agent 回复`。

广播链路：`RoomService → Cluster.Send(access Node, AgentName, Push Command) → Agent.Handle → protocolDispatcher.Push → Client`。RoomService 不逐个 Call Agent；推送请求带目标 LoginID，Agent 拒绝不匹配的旧推送。

Agent 在协议 Handler 调用业务节点前，按 [状态与协议表](USER-SESSION.md) 默认拒绝不适用的消息。Room 阶段由 Room 推进，向 Agent 推送可见状态、向 Online Send 低频投影；两处接收者匹配 LoginID/RoomID/BindingToken，再比较房间版本。Room 推送不能改变 Agent 的位置路由。阶段可能暂时滞后，快照查询必须始终保留；Room 再检查当前阶段，保证结束后的命中/出牌不生效。Online 不进入每次命中/出牌的热点链路。

每条写命令定义自己的重复语义：

| 命令 | 去重/过期依据 | 超时后的处理 |
| --- | --- | --- |
| Bind / Enter / Leave / Reconnect | OperationID、内容、绑定令牌 | 查询操作结果；原 ID 条件重试 |
| StartRound（打地鼠为服务端满员触发） | RoomID、成员集合、当前局状态 | 客户端查询快照；不重新触发第二局 |
| Hit | UserID、RoundID、MoleInstanceID、HitOperationID | 原 ID 查询/重试；每实例最多记分一次 |
| Play / Pass / Ready | RoundID、TurnID/准备版本、OperationID | 查询结果和快照；拒绝旧回合，不改写为新回合命令 |
| SubmitSettlement | SettlementID、规范化结果内容 | 查结算；原 ID 重试 |

协议 Envelope.request_id 只关联客户端响应，不作为跨重连业务幂等键。操作结果缓存有界；局结束或窗口结束后仍拒绝旧局号/旧回合，不因缓存淘汰再次执行。只读查询可重试。

快照和推送带 RoomID、RoundID、StateVersion；版本在权威状态变化时单调增加。客户端在新快照后丢弃旧版本，发现增量缺口时请求快照；首版优先发小型可独立应用的完整可见状态。响应和推送到达顺序不作为业务正确性的依据。重连清空旧客户端待处理操作，以新快照恢复。

斗地主的公共状态与私人手牌分别生成，各 Agent 只收到本人可见内容，不能广播全部手牌后交给客户端隐藏。

慢客户端由 Gate 有界出站队列限制；Agent 写回失败时关闭该连接并执行条件清理。Room 的 Send admission 失败记录指标，停止向已失败的连接堆积重试；缺失状态由后续快照修复。一个客户端的出站堵塞不串行等待其他玩家。

## 7. 多人同组打地鼠与 Timer

打地鼠采用最小多人同组玩法，借鉴只读 `GoServerActorFsm` 的分组、共享地鼠时间线、广播和快照；`nbgameserver/gameofshrew/` 仅作补充业务参考。不复刻概率池、倍率、计费、地图动画或历史功能。完整流程必须包括各玩家登录、分组入座、开局、地鼠出现/消失、命中、分数推送、局结束、全组待结算持久化、积分到账及查询；结算不可省略。

### 7.1 组与状态所有权

- 一组对应一个 RoomID 和一个 MoleRoomService，不额外引入 GroupService/AttackService 或每玩家 PlayerService。该 Service 唯一拥有成员、座位、当前登录绑定、共享目标、局期限及每人本局分数。
- 首版默认三人一组，GroupSize 可配置且必须至少为 2，满员自动开始；未满员处于 filling，地鼠快照为空，命中拒绝。局期限从实际开局起算。
- RoomDirectory 将新玩家分配到可加入的等待组；组满或开局后新玩家进入另一组，不在进行中的局补新人。组实际成员准入由 MoleRoomService 确认，目录不能仅靠缓存人数越过容量检查。
- filling 阶段可离开，按显式空闲座位分配，不按成员数生成座位号。开局冻结本局参与者集合；进行中断线保留座位、分数及结算资格，重连只替换业务绑定，不重置目标、分数或局期限。进行中不跨组迁移。
- 所有成员离线也按期限结束、保存结果并结算。最小流程一组完成一局；新局另建 RoomID，不在未保存结果上覆盖状态。结束后的房间按保留期退出，目录清理沿用条件更新规则。

### 7.2 共享目标与命中

首版使用固定洞位集合、单个活跃地鼠、单次命中消失、每次合法命中奖励 10 个整数积分。时间窗口与有限局时长由服务端配置，所有同组成员看到同一目标实例；不同时复刻多种血量、锤子、连击或倍率。

RoomService 使用服务端单调时间保存局期限及命中窗口。MoleInstanceID 采用局内不复用的 spawn 序号；RoomID、RoundID 与实例号共同识别目标，旧局/旧洞位实例不能命中新实例。

命中依次校验当前绑定、局号、目标实例、尚未命中、服务端处理时刻处于窗口内。客户端不能决定时间、命中、分数或奖励；首版不做客户端时间补偿。每次 Hit 只请求一个实例，避免批量命中出现部分更新。

同组竞争命中按房间 Mailbox 的处理顺序确定，首个合法命中令共享目标失效，并只给该玩家加分；其他玩家命中同一实例返回已命中状态，不增加积分。用户自己的同一 HitOperationID 重试返回原结果，不再次计分；同 ID 改请求内容返回冲突。拒绝命中的请求自身不改变状态；处理前因时间到期发生的状态推进单独记录。

目标变化、命中者、个人分数及组内计分板向本组广播，不能发往另一组。快照包含成员/座位、共同目标、各人分数、RoomID、RoundID、StateVersion、局期限与结算状态；重连恢复同一局，乱序推送由版本及实例号处理。

### 7.3 Timer 与结束推进

Timer 只发送 Command；每个 Timer 消息带 RoundID/目标实例或调度代次，旧局 Timer 被忽略。取消 Timer 不能撤回已进入 Mailbox 的消息。

GSR After 是一次性触发，Mailbox 满时触发可能丢失。因此：

- 每次房间请求先按服务端时间推进到当前状态，再处理命令；超时地鼠即使没有收到消失消息也不能命中。
- RoomDirectory 使用自己的低流量控制 Mailbox，按有界批次轮询到期房间，发送幂等 AdvanceToNow；投递失败保留待检查项并退避，下次继续，不只依赖房间的下一条 Timer。
- 巡检自用 Timer 在每次处理结束后重新登记，限制最多一条待执行巡检，客户端不能直接向该入口灌流量；登记失败、巡检停滞或目录 Service panic 进入告警和节点恢复流程。
- 房间数量、巡检批次、局末保存等待均有上限；持续过载时拒绝新局，不能承诺在无限过载下仍准点触发。

### 7.4 全组结算

组状态为 `filling → running → result_unsaved → settlement_pending → settled`。同一局只冻结一次全组结果、产生一个 SettlementID；包含所有开局参与者（包括零分及已断线成员），按 UserID 排序记录每人的最终积分增量。

MoleRoomService 持久化一份全组 pending 结果，Recovery 提交同一份冻结结果，Settlement 在一个事务中更新全组积分账户及账本。部分玩家到账、另一部分未到账不能被作为已完成结果；同一组重复提交不重复加分。个人增量为本局合法命中累计奖励，打地鼠奖励总和不要求为零。

保存或提交尚未确认时，客户端只能看到未保存/待结算状态，不宣称积分到账。成功后按每人结果推送，并允许查询同一 SettlementID 下的本人到账结果。持久化、幂等冲突及恢复边界沿用第 9 节，不能因为多人玩法最小化而省略。

## 8. 三人斗地主

优先参考只读 `nbgameserver/gameofhlddz/`，不以包含四人玩法的 `gameofsrddz/` 作为首选。参考实现用于核对业务过程，demo 的具体规则以本文及切片前固定的规则表为准。

三个座位、准备、服务端洗牌发牌、首轮固定地主、出牌/不出、回合切换、胜负、积分、断线超时默认动作、重连快照。首版不做匹配、排行、赛事、叫分、春天倍率和高级托管。

牌型和比较使用普通 Go 纯函数及表驱动单测，覆盖单张/对子/三张、三带、顺子、连对、飞机、四带、炸弹和火箭；对子/翅膀选择、2/王是否允许进入序列等歧义写成规则表，并明确采用哪一种变体。规则表和积分公式在开发斗地主切片前固定，不能以“基本规则”代替。

RoomService 校验手牌实际拥有、多重集合数量、当前轮次、当前领出状态和能否压过上一手。非法操作本身不改变手牌、回合和 StateVersion；处理请求前因服务端时间到期执行的自动动作单独记为合法状态变化，测试在固定时刻比较非法操作前后状态。不出不能用于没有可压目标的新领出；轮到玩家时按 deadline 校验，避免迟到请求先于排队的超时命令执行。

断线玩家仍保留座位，超时有可压目标则不出，否则打出规则允许的最小单张；自动动作与手动动作使用同一状态检查，一回合最多推进一次。重连不重置回合期限。不同房间各自拥有 Service，不互相同步等待。

积分使用有符号整数，首版允许负积分，三人结算增量总和为零。具体基础分在规则配置中固定；不引入余额门槛和资金预冻结。打地鼠为明确的奖励积分公式。

## 9. 持久化与结算恢复

### 9.1 最小数据模型

| 表 | 写入 owner | 关键约束 |
| --- | --- | --- |
| accounts | Login | UserID 主键、规范化用户名唯一、密码哈希 |
| external_identities（阶段 4） | Login | provider/issuer/subject 唯一，映射到 UserID |
| pending_settlements | 对应游戏节点业务 | SettlementID 主键、(GameKind, RoundID) 唯一、冻结结果、待提交/已提交标记 |
| settlements | Settlement | SettlementID 主键、(GameKind, RoundID) 唯一、规范结果及已提交结果 |
| point_accounts | Settlement | UserID 主键、整数积分 |
| point_ledger | Settlement | (SettlementID, UserID) 唯一、积分增量及记账信息 |

同一个 MySQL 实例可承载这些表，但按 owner 划分写权限；游戏节点不能写 point_accounts/point_ledger/settlements。Room 只插入并核对冻结结果，Recovery 只更新提交标记，重复保存不能把已提交标记重置为待提交。所有游戏节点共用的 pending 表按 GameKind/游戏 Node 范围扫描，首版部署保持每个范围只有一个恢复 owner；不提供多实例并行领取任务，复制游戏节点以后再设计领取机制。误重复提交仍受 Settlement 数据库唯一约束保护。

数据库 schema 用版本化 SQL 明确迁移，普通应用启动不自动建表。初始积分为零，由 Settlement 在首次记账事务中建立积分账户，或通过其管理接口初始化；Login 不直接写余额。

### 9.2 流程与提交边界

1. RoomService 冻结局末结果，生成一次 SettlementID，进入 `result_unsaved`。
2. 使用有限数据库等待写入 pending_settlements，保存局结果与同一 SettlementID；重复保存也验证内容。提交结果不明时查询同一记录，禁止另造 ID。
3. 保存确认后进入 `settlement_pending`，客户端可看到“局已结束、积分待结算”。此时不宣称积分已到账。
4. SettlementRecoveryService 启动先扫描持久化记录，之后分批周期扫描；Send 唤醒只用于降低延迟，唤醒丢失不影响恢复。
5. 恢复 Service 通过有限 Call 提交冻结结果。SettlementService 校验合法字段、重复用户、整数溢出及游戏对应约束，在一个事务内插入结算、按稳定 UserID 顺序锁定/更新账户、插入账本，全部提交后才返回成功。
6. 相同 ID、相同规范内容返回原始已提交结果；相同 ID、不同内容返回 `settlement.conflict`。同局不同 ID 也拒绝再次记账，不能仅靠调用者承诺只生成一次 ID。
7. Call/Commit 超时或回复丢失时，恢复 Service 查询 SettlementID；尚无记录可能只是原事务仍在途，继续使用原 ID 提交，由唯一约束和事务防止重复记账。
8. 已确认提交后才把 pending 标记为已提交。此标记失败可继续重试，同一结算不会重复加分。客户端可按 SettlementID 查询完成结果，推送不是唯一完成凭据。

规范内容包括协议/规则版本、GameKind、RoundID、按 UserID 排序的积分增量及决定结果的必要字段；不包含 LoginID、推送地址、重试次数和临时连接信息。保存规范字节并比较完整内容，摘要只作为辅助。无序集合、浮点数或随重试改变的时间戳不能造成伪冲突。

MySQL 死锁需要有限次数整事务重试，并保持同一业务 ID；采用短事务和稳定的账户更新顺序降低冲突。参考 [MySQL InnoDB 死锁处理](https://dev.mysql.com/doc/refman/8.4/en/innodb-deadlocks-handling.html)。数据库提交后的回复丢失由业务幂等处理。

### 9.3 故障与容量

已经保存的待结算结果必须在游戏节点重启后补齐。不恢复崩溃前未结束的局；结果冻结但尚未持久化时崩溃，也不承诺补齐该结果，客户端状态必须体现这个边界。

MySQL 暂不可用时，未保存结果保留在原房间的有限内存中，按退避重试；超过未保存房间数量/等待阈值则停止接受新局并报警，不假报成功。已经保存的任务不因重试次数或等待时长被删除。

恢复 Service 使用有界批次、单批有限外部等待、带抖动的退避，错误冲突进入人工检查而非无限高速重试。队列积压以数据库记录为准，不能无限扩展内存 Mailbox。

RoomService.OnStop 不承担最后一次跨节点结算；可靠性来自运行期已提交的 pending 记录。单房间 panic 后未保存结果可能丢失，已保存结果由恢复 Service 接续，其他房间继续运行。

## 10. GSR 接入约束

- Go main 通过 `gsr.Run` 启动，业务只使用当前 `self.Cluster()/Logger()`；不手工注入 Runtime 或 ClusterClient。
- Service 显式实现 Handle，具名持有 gsr.Dispatcher；Agent 的 HandleClientMessage 使用 protocol.Dispatcher，两种入口共用一个 runner。
- 当前 Cluster ValueCodec 不直接恢复业务 Go struct。跨节点边界使用规范标量或业务 Protobuf `[]byte`；本项目建议复杂命令统一编码为 `[]byte`，普通函数处理领域结构体，Handler 显式解码/校验，响应同样编码。不扩展 GSR codec。
- CommandID 与客户端 msg_id 分开定义，业务目录不占用框架 ErrorResponse 的 9001。业务拒绝、未确认和已完成状态有明确业务响应，不把所有错误包装成 internal。
- Cluster 入站的 Source 不能证明远端 Agent/UserID 身份。内部 Cluster 仅开放在受信任网络；认证身份由受信任 access 传入，Room 再检查当前业务绑定。客户端提供的 UserID/LoginID 不作为凭据。
- 等待链不能成环：Agent→Online→Room、Agent→Room、Recovery→Settlement；Room 不 Call Online/Agent，Settlement 不 Call Room。业务 Service 不启动 goroutine，不保存 Handler 上下文做异步工作。
- 任一外部 Call/SQL 都有有限预算；配置整个流程的总预算，避免多次子调用各自耗尽一次完整客户端等待。返回超时不等于操作没有执行。
- Mailbox、客户端队列、活跃房间、认证实例、数据库池和恢复批次均配置上限。房间结束后按业务保留期 Exit，避免每一局永久留存一个 Service。

## 11. 第三方登录

阶段 4 只接一个真实提供商，具体提供商在实施该切片前选定。按提供商协议验证凭据，不能相信客户端直接上报的身份。若采用 OIDC，验证签名、issuer、audience、有效期及相应流程的 nonce 等要求，身份以 issuer/subject 映射。参考 [OIDC ID Token Validation](https://openid.net/specs/openid-connect-core-1_0.html#IDTokenValidation)。

若要求同一人的密码登录和第三方登录返回同一 UserID，必须提供由已认证账号发起的关联流程，并验证新第三方凭据。不能按同名/邮箱猜测并自动合并；第三方身份唯一约束防止并发关联到两个账号。

慢第三方请求只阻塞对应 AuthService，实例数和排队有界；不得因提升并发引入无限 goroutine。

## 12. 验收

| 阶段 | 交付 | 必须通过的验证 |
| --- | --- | --- |
| 0 | 多 module 工程、显式启动组装、typeclient 诊断 CLI | 多进程命名 Call 和 Agent Push 真实接线；gsr 无修改 |
| 1 | 密码登录、Online、最小多人同组打地鼠状态切片 | 四进程、至少三客户端同组；满员开局；竞争命中只奖励一人；不同组隔离；错误登录无房间；重复/过期目标不加分。此阶段尚无结算，不算完整 demo |
| 2 | MySQL 积分、待结算保存、恢复 Service；首轮完整 demo | 五进程、多客户端从分组登录到全组同事务结算到账闭环；重复/冲突、提交后丢回复、进程强杀、MySQL 故障无假成功 |
| 3 | access-02、跨接入重连、目录时序 | 竞争登录、旧断线/旧命令/迟到绑定无效；Online 超时与协调重启规则 |
| 4 | 三人斗地主 | 三个 TS 客户端跑完一局；非法操作不改变权威状态；私人手牌不泄露 |
| 5 | 一家第三方登录及账号关联 | 真实验证；正确关联同一 UserID；错误签名/issuer/audience/过期拒绝 |
| 6 | 故障与负载 | 隔离故障、背压、约 10,000 条/秒实际业务混合负载 |

必测故障：两次登录竞争、旧断线晚到、旧绑定请求晚到、绑定已执行回复丢失、access 突然退出、跨 access 重连、单 RoomService panic、Online 不可用、Online 重启空目录、游戏节点重启、Settlement/MySQL 不可用、慢客户端、Timer admission 失败、Call 超时但操作已执行。

负载报告区分客户端请求、推送、Cluster 消息及结算事务吞吐；约 10,000 条/秒是待测目标，先测真实业务比例，不人为凑 20% 跨节点。报告 p50/p95/p99、MailboxFull、CPU、GC、内存、出站积压和待结算数量/最老等待时间，不能只报成功消息计数。

## 13. 当前通信切片

切片 A 使用独立 demo.diagnostic.v1 协议验证接线；各角色当前暴露命名 Probe Service，access Agent 接收客户端请求后用总计 2 秒预算依次 Call login/online/mole/settlement。复杂请求和返回值均为 Protobuf bytes；mole 使用客户端不可指定的连接私有地址和令牌向 Agent Send Push。Agent 的普通 Command 和客户端协议共用 Mailbox，响应 request_id 与主动 Push request_id=0 分开。

Probe 的 ok 只代表四个诊断 Handler 返回，不代表业务成功或 Send 送达；CLI 另等实际 Push，且校验 operation_id/text/node。空输入返回 invalid，远端等待/调用失败返回 unavailable 和 failed_node。当前只有开发 Plain Gate，不提供认证或游戏接口；本节不改变第 4～9 节未来业务契约。
