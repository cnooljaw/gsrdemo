# GSR Demo 业务设计

本文记录用户已明确的约束、现有实现、待审方案、执行计划与参考基线。当前已有诊断通信、密码登录、位置协调和多人地鼠状态切片；MySQL 结算及恢复仍待实现。全部消息与字段集中在 [协议规划](PROTOCOL.md)，工作规则见 [AGENTS.md](../AGENTS.md)。

现有代码和测试通过只说明已实现行为，不能视为用户确认了设计。本文中节点职责拆分、Online 位置协调与操作记录、换绑、巡检和故障策略均作为逐节点审查的输入，尚未定案；相关接口见协议规划，先按第 12 节展示方案并得到确认，再修改业务代码。逐节点确认时在对应章节记录裁决，不新增计划文档。

已明确的约束继续遵守：每节点角色一个独立项目、参考目录只读、客户端始终只连 access、状态尽量少、内部调用使用 GSR 原生参数、多人同组地鼠及完整结算、三人斗地主优先参考 gameofhlddz/。

## 1. 范围与边界

一个 Git 仓库管理多个独立项目：每节点角色一个 Go module，同角色多实例用配置区分；common 为共享 Go module，typeclient 为独立 TypeScript 项目。节点依赖 GSR/common，不导入彼此 internal。common 按实际调用提取值对象、命令及协议，不放 Service、SQL、可变游戏状态或宿主组装。三个参考目录严格只读，本地 GSR 依赖及阅读来源见本文末尾。

首版仅使用整数积分，不涉及真钱。先用 TypeScript 命令行客户端和机器人，完成“用户名密码登录 → 打地鼠 → 持久化积分 → 故障恢复”，再加入三人斗地主。Redis 不进入首版闭环。

不新增 UserManager、通用 GameService 基类、推送代理、通用 worker 框架、消息队列或分布式事务框架。

## 2. 节点与 Service

每个进程一个 Runtime、一个 Cluster。六种节点角色足够，按阶段启动实际需要的角色。当前大厅只是用户位置，由 Online 保存、Access 准入，不增加独立 lobby 节点；独立大厅业务出现后再决定其运行单元。

| 节点 | 内部运行单元 | 职责 |
| --- | --- | --- |
| access-01 | Gate、每连接一个 Agent | 唯一客户端接入、已确认会话、协议准入、待确认操作、位置/阶段快照、客户端写回 |
| login-01 | 少量有界 AuthService | 账号校验、第三方验证、身份映射 |
| online-01 | OnlineService | 用户位置、待确认操作、绑定/进入/离开协调、房间阶段投影 |
| mole-01 | RoomDirectory、每组一个 MoleRoomService、SettlementRecoveryService | 分组与座位、共享地鼠、个人计分、持久化全组局末结果、恢复结算 |
| landlord-01 | RoomDirectory、每房间一个 LandlordRoomService、SettlementRecoveryService | 房间创建、座位/手牌/回合、持久化局末结果、恢复结算 |
| settlement-01 | SettlementService | 积分、结算记录、幂等账本 |

MainService 显式创建并命名 Service，开放 Cluster/Gate。RoomDirectory 只创建/查找房间、为打地鼠分配可加入的等待组、维护本节点生命周期索引、进行有界的超时巡检，不中转命中和出牌。结算恢复 Service 是有独立等待和重试需求的业务运行单元，不是新节点。

现有诊断启动五个进程，现有地鼠状态演示启动 access、login、online、mole 四个进程。后续按第 12 节逐节点确认与修改；完整闭环仍需 settlement，access-02 与 landlord 随对应节点方案另行确认。

## 3. 状态所有权（现有方案，待审）

| owner | 权威状态 | 禁止承担的职责 |
| --- | --- | --- |
| Agent | 本连接已确认会话及待确认操作；持有位置、游戏路由和房间阶段快照 | 积分余额、手牌、命中判定、自行确认位置变化 |
| Login | 账号和第三方身份到 UserID 的映射 | 在线目录、游戏状态 |
| Online | 当前接入/大厅/房间位置、进行中的位置迁移；持有房间阶段的版本投影 | 游戏阶段自行推进、游戏规则、积分修改 |
| RoomService | 该房间全部游戏状态、玩家当前 LoginID/推送地址、绑定令牌、回合和状态版本 | 持久化积分余额修改 |
| SettlementRecoveryService | 待结算任务的提交、退避、状态查询和本地完成标记 | 重新计算结果、修改余额 |
| SettlementService | 持久化积分、已提交结算和账本 | 决定胜负、接受客户端奖励值 |

MySQL 是外部依赖；`*sql.DB` 连接池由进程宿主管理，在 Runtime 停止后关闭。连接池可以并发使用，业务对象和可变游戏状态不能跨 Service 共享。跨消息传递的是独占快照。

Online 保存已确认身份/接入、可选房间绑定、位置版本、GameKind/RoundID/RoomPhase/RoomStateVersion、Connected 和一条可选 PendingOperation，不保存目标、计分板等游戏快照。Room 的完整状态可随位置确认或查询结果转交 Agent；Agent 的游戏缓存与会话记录分开。后台已完成的位置操作恢复时，Agent 在原调用预算内直接向当前 Room 补取缺失或滞后的游戏快照；补取失败保留确认位置，客户端可继续 Snapshot 同步。没有房间路由即大厅，不另存流程状态枚举。字段及版本规则见 [协议规划](PROTOCOL.md)。RoomID 跨重启不复用，RoomName 含 RoomID，旧路由不能命中新建房间。

LoginID 是随机且不可复用的业务登录关联标识，不是 Core Session，也不向 Runtime 引入 epoch/lease。绑定令牌是 RoomService 的业务比较并更新凭据，不是 ServiceID。

用户只分未登录、大厅、房间，由已确认会话/路由推导；Online 不存未登录连接。房间只分等待、进行、结束，两个游戏各自显式迁移；新局创建新 RoomID。认证、RPC 等待、保存和结算步骤不增加状态，也不以重复布尔值重建复杂状态机。客户端始终只连 access，Agent/Online 按同一位置策略准入协议，Room 再验证业务条件。

PendingOperation 存在时只允许本连接查询/核对原操作，已确认位置保留不动；处理中和结果未知使用同一规则，确定结果后才清除记录。断线只更新 Connected，保留房间、未确认操作及结算资格。关闭由 Agent 生命周期处理，登录后换账号需建立新连接。排队请求在 Handler 实际处理时检查当前数据，不为同步 Call 等待增设状态。

Online 是位置唯一 owner，Room 是游戏阶段唯一 owner。access/Online 的阶段只是有版本的观察值，可能滞后；Room 的阶段 Send 不修改 Agent 路由。Online 有界分批核对阶段，Agent 可直接向当前 Room 查询快照；缺少观察时先同步，再开放游戏写操作。Room 再检查实际阶段，结束后仍显示进行中的旧缓存不能让命中/出牌生效。

## 4. 登录、进房与连接切换（现有方案，待审）

### 4.1 登录

1. 客户端始终只连接 access 的 Gate，由 Agent 调用 login 节点验证用户名密码，取得统一 UserID；进房、游戏和结算查询不切换客户端连接目标。
2. Agent 在已经 Running 的协议 Handler 中生成 LoginID，注册连接私有名称，例如 `agent/<随机连接标识>`；注册成功后才发布推送地址。不在 Agent.OnStart 中提前注册。
3. Agent 调用 Online.Bind，携带由服务端产生的 UserID、LoginID、接入地址和 OperationID。
4. Online 串行决定该用户的切换顺序。尚未进房的绑定可直接完成；已有房间的绑定必须先完成房间连接替换，再确认新绑定。
5. Agent 收到已确认结果后进入可操作状态；超时只表示结果未知，使用同一 OperationID 查询或重试，不能自行当成失败并重新创建一次迁移。

“后登录生效”按 Online 接纳并完成绑定的顺序定义，不按客户端时间或随机 LoginID 大小定义。认证成功与绑定成功分开返回；绑定未确认的 Agent 不能操作游戏。

密码使用带随机盐的 Argon2id，参数随哈希保存；根据部署硬件测量成本，限制认证实例数量、输入长度和请求速率，避免哈希并发耗尽内存。参考 [OWASP 密码存储建议](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)。首版用管理命令创建测试账号，不先建设注册/找回密码产品流程。

### 4.2 进房与重连的协调

控制链路为：`Agent → Online → 游戏节点 RoomDirectory/RoomService → Online 确认 → Agent`。这是对原方案调用链的细化：Online 协调低频的位置变化，热点游戏命令仍从 Agent 直达房间。

同一用户最多一条 PendingOperation，记录 OperationID、操作内容、目标位置及比较绑定信息。已确认位置在操作期间保留不动；有记录就拒绝另一位置操作，只允许查询原操作。处理中和结果未知采用同一规则，不另设流程枚举。收到确定成功才安装目标位置，明确拒绝则保留原位置，再清除待确认记录并保留有界确定结果。

RoomService 的 CompareAndBind 只在当前绑定令牌匹配时替换玩家连接，同时产生新令牌。同一操作、相同内容重试不重复应用，改内容返回冲突；具体载荷见协议规划。

初次入座也使用房间分配的令牌；移除玩家后仍保留对应代次信息或操作墓碑，不能简单重置为空而允许迟到请求重新匹配。房间销毁后新房间使用新 RoomID。幂等记录有明确保留范围，过期操作被拒绝，不能重新执行。

例如 A→B 已完成，随后 B→C 已完成，迟到的 A→B 请求因旧令牌不匹配不能把 C 改回 B。不能把“LoginID 不同就覆盖”作为重连规则。

Call 超时后，Online 保留该用户的 PendingOperation，查询目标房间实际绑定/操作结果，或以相同令牌和 OperationID 重试；确定结果后才允许下一次位置变化。一次“尚未找到”不能证明先前请求不会迟到执行。其他用户可以继续绑定，每次 Handler 的外部等待必须有有限预算。

离开也通过 Online 协调：房间移除确认后再清理目录，Agent 收到确定结果才返回大厅并移除游戏路由。首版 filling 允许离开，running 拒绝离开；ended 统一接受离开请求，再由 Room 检查匹配的到账回执，未确认时返回待结算且不移除成员。断线只将 Connected 标记为 false，保留座位、待确认操作和结算资格。位置清理是带 LoginID/操作令牌条件的更新，禁止无条件按 UserID 删除。

### 4.3 旧连接与操作授权

Agent 从自身已认证状态填入 UserID/LoginID，客户端不能选择别人的身份或任意房间路由。RoomService 校验 RoomID、玩家身份、当前 LoginID、局号和相关操作约束。

新绑定成功的含义是房间已替换 LoginID，旧连接之后到达的操作、断线及离开通知均不能改变新绑定。替换之前已经执行的合法旧操作不回滚。踢旧连接的 Send 只是通知，不承担授权正确性。

断线通知在 Agent.Handle 中发起有界清理，然后 Exit；清理失败允许目录暂时滞后，由后续重连/巡检纠正。Agent.OnStop 负责关闭连接，不依赖停机阶段还能发起新的 Cluster Call。

## 5. Online 故障与重启（待审建议）

Online 不可用时拒绝新绑定、进房、跨房间迁移和重连绑定；已确认的 Agent 依靠缓存路由继续操作原房间。Online 不被每次命中/出牌查询。断线清理失败不意味着房间释放座位。

首版建议采用保守且明确的重启策略：Online 重启后不立刻把空目录当作“所有用户已离线”。关闭接入的新登录/进房入口，协调停止旧 access 和游戏进程，确认旧进程退出，再启动 Online、游戏节点及 access。进行中的房间终止，客户端重新认证；游戏节点启动恢复已持久化待结算结果。login、settlement 无需随之重启。

此策略牺牲 Online 重启时的游戏可用性，但避免空目录、新房间和仍活着的旧房间同时拥有同一用户。若某个旧游戏进程无法确认退出，保持新绑定入口关闭，不把网络不可达当作进程已死亡。

普通 access 重启无需此全局流程：RoomService 保留原 LoginID/座位；用户在另一 access 登录，由 Online 通过房间比较并更新完成重连。普通游戏节点崩溃不恢复进行中的房间；其重启确认旧房间已消失后，按条件清理对应目录，再允许相关用户进入新房间。

保留房间的 Online 在线重建是后续可用性改进，需要冻结位置迁移、隔离旧在途命令、枚举权威房间绑定并处理期间变更；首版不把“启动后拉取一次列表”当成完整重建。

## 6. 游戏调用与推送

热点链路：`Client → Agent → Cluster.Call(RoomName, Command) → Agent 回复`。

广播链路：`RoomService → Cluster.Send(access Node, AgentName, Push Command) → Agent.Handle → protocolDispatcher.Push → Client`。RoomService 不逐个 Call Agent；推送请求带目标 LoginID，Agent 拒绝不匹配的旧推送。

Agent 在协议 Handler 中先做状态准入，Room 再校验绑定与游戏条件。Room 阶段变化向 Agent Send 完整可见状态、向 Online Send 低频投影，不在每次命中/出牌更新目录。消息、版本过滤、超时/重复操作和准入表统一见 [协议规划](PROTOCOL.md)。

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

组阶段只保留 `filling → running → ended`。结束后在同一 ended 阶段保存结果、提交结算和查询到账，不为这些步骤增加阶段。同一局只冻结一次全组结果、产生一个 SettlementID；包含所有开局参与者（包括零分及已断线成员），按 UserID 排序记录每人的最终积分增量。

MoleRoomService 持久化一份全组 pending 结果，Recovery 提交同一份冻结结果，Settlement 在一个事务中更新全组积分账户及账本。部分玩家到账、另一部分未到账不能被作为已完成结果；同一组重复提交不重复加分。个人增量为本局合法命中累计奖励，打地鼠奖励总和不要求为零。

保存确认和 Settlement 回执放在冻结结果记录中，客户端从记录得到未保存/待结算/已到账进度，不另建结算状态机。保存或提交尚未确认时不宣称积分到账；获得匹配回执后按每人结果推送，并允许查询同一 SettlementID 下的本人到账结果。持久化、幂等冲突及恢复边界沿用第 9 节，不能因为阶段合并而省略。

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
| external_identities（第三方登录切片） | Login | provider/issuer/subject 唯一，映射到 UserID |
| pending_settlements | 对应游戏节点业务 | SettlementID 主键、(GameKind, RoundID) 唯一、冻结结果、待提交/已提交标记 |
| settlements | Settlement | SettlementID 主键、(GameKind, RoundID) 唯一、规范结果及已提交结果 |
| point_accounts | Settlement | UserID 主键、整数积分 |
| point_ledger | Settlement | (SettlementID, UserID) 唯一、积分增量及记账信息 |

同一个 MySQL 实例可承载这些表，但按 owner 划分写权限；游戏节点不能写 point_accounts/point_ledger/settlements。Room 只插入并核对冻结结果，Recovery 只更新提交标记，重复保存不能把已提交标记重置为待提交。所有游戏节点共用的 pending 表按 GameKind/游戏 Node 范围扫描，首版部署保持每个范围只有一个恢复 owner；不提供多实例并行领取任务，复制游戏节点以后再设计领取机制。误重复提交仍受 Settlement 数据库唯一约束保护。

数据库 schema 用版本化 SQL 明确迁移，普通应用启动不自动建表。初始积分为零，由 Settlement 在首次记账事务中建立积分账户，或通过其管理接口初始化；Login 不直接写余额。

### 9.2 流程与提交边界

1. RoomService 冻结局末结果，生成一次 SettlementID，进入 `ended`；后续保存、结算和到账都保留这一阶段。
2. 使用有限数据库等待写入 pending_settlements，保存局结果与同一 SettlementID；重复保存也验证内容。提交结果不明时查询同一记录，禁止另造 ID。
3. 保存确认写入结果记录，客户端可看到“局已结束、积分待结算”。Room 仍为 ended，此时不宣称积分已到账。
4. SettlementRecoveryService 启动先扫描持久化记录，之后分批周期扫描；Send 唤醒只用于降低延迟，唤醒丢失不影响恢复。
5. 恢复 Service 通过有限 Call 提交冻结结果。SettlementService 校验合法字段、重复用户、整数溢出及游戏对应约束，在一个事务内插入结算、按稳定 UserID 顺序锁定/更新账户、插入账本，全部提交后才返回成功。
6. 相同 ID、相同规范内容返回原始已提交结果；相同 ID、不同内容返回 `settlement.conflict`。同局不同 ID 也拒绝再次记账，不能仅靠调用者承诺只生成一次 ID。
7. Call/Commit 超时或回复丢失时，恢复 Service 查询 SettlementID；尚无记录可能只是原事务仍在途，继续使用原 ID 提交，由唯一约束和事务防止重复记账。
8. 已确认提交后才把 pending 标记为已提交，并向尚存活的 Room 提供匹配的 Settlement 回执。此标记失败可继续重试，同一结算不会重复加分；Room 阶段仍为 ended。回执通知丢失时 Room 可按 SettlementID 核对本节点持久化的完成确认，再允许移除成员。客户端可按 SettlementID 查询完成结果，推送不是唯一完成凭据。

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
- 调用与发送直接使用 GSR，不另包通用 Call/Send。所有跨节点参数/返回值直接使用规范标量、map[string]any 和 []any，由默认 ValueCodec 编码；不为内部命令定义 Protobuf 载荷或扩展 Codec。身份/绑定/操作记录为可复制、可比较的 Go 值，跨节点只组装业务字段。Directory→Room 的 ReserveSeat 用本地 CallName 传普通 Go 值；快照每次新建全部集合，客户端 Protobuf 视图在 access 出口组装。具体签名集中在协议规划。
- 消息方向、编号、载荷及业务错误统一遵循协议规划，不把状态拒绝转换成通用 internal。
- Cluster 入站的 Source 不能证明远端 Agent/UserID 身份。内部 Cluster 仅开放在受信任网络；认证身份由受信任 access 传入，Room 再检查当前业务绑定。客户端提供的 UserID/LoginID 不作为凭据。
- 等待链不能成环：Agent→Online→Room、Agent→Room、Recovery→Settlement；Room 不 Call Online/Agent，Settlement 不 Call Room。业务 Service 不启动 goroutine，不保存 Handler 上下文做异步工作。
- 任一外部 Call/SQL 都有有限预算；配置整个流程的总预算，避免多次子调用各自耗尽一次完整客户端等待。返回超时不等于操作没有执行。
- Mailbox、客户端队列、活跃房间、认证实例、数据库池和恢复批次均配置上限。房间结束后按业务保留期 Exit，避免每一局永久留存一个 Service。

## 11. 第三方登录

第三方登录切片只接一个真实提供商，实施前选定。按提供商协议验证凭据，不能相信客户端直接上报的身份。若采用 OIDC，验证签名、issuer、audience、有效期及相应流程的 nonce 等要求，身份以 issuer/subject 映射。参考 [OIDC ID Token Validation](https://openid.net/specs/openid-connect-core-1_0.html#IDTokenValidation)。

若要求同一人的密码登录和第三方登录返回同一 UserID，必须提供由已认证账号发起的关联流程，并验证新第三方凭据。不能按同名/邮箱猜测并自动合并；第三方身份唯一约束防止并发关联到两个账号。

慢第三方请求只阻塞对应 AuthService，实例数和排队有界；不得因提升并发引入无限 goroutine。

## 12. 逐节点执行计划与验收

后续一次只推进一个节点。当前代码保留为可运行基线，按节点重新核对设计与 GSR 用法；不直接推进 MySQL 或更大的跨节点切片。以下为审查顺序，下一节点由用户决定。

| 顺序 | 节点 | 先展示并确认的方案 | 确认后的最小实施与验证 |
| --- | --- | --- | --- |
| 1 | access | Gate/Agent 的职责与生命周期；连接拥有的数据、最少位置状态、协议准入；GSR 注册、Call/Send、客户端回复与 Push 的正确入口；与其他节点的接口边界 | 只调整 access 已确认部分；验证连接、分发、准入、推送与关闭；依赖节点需要的改动先列出来 |
| 2 | login | 当前 demo 最小认证能力、账号来源、输入/输出、AuthService 和 GSR 调用方式；是否保留现有管理命令和限流等设计 | 只实现已确认的认证方案；验证成功、拒绝和调用边界 |
| 3 | online | 为什么需要该节点、应保留哪些用户数据、位置与路由由谁决定；是否需要现有迁移协调、操作记录、换绑与巡检；先讨论复杂度与失败边界 | 只实现确认后的最小职责；相关旧连接、未知结果和位置一致性用例按最终方案选择 |
| 4 | mole | 多人分组/座位、共享时间线、命中/积分、局末冻结；Directory 与 Room 的必要职责；如何衔接结算、使用哪些 GSR Timer/消息能力 | 逐步实现该节点，验证满组开局、同组竞争、重试/过期、全组结果；外部结算依赖留到接口确认后联调 |
| 5 | settlement | 最小积分账本、全组结算事务、重复/冲突处理；MySQL 与游戏节点的接口、保存和恢复职责归属 | 实现并验证到账、重复/冲突与故障边界，再联调完整地鼠流程 |

每个节点按以下顺序推进：

1. 只读检查当前实现、调用方和已有用例；核对只读 GSR 文档与可运行示例。Cluster 优先看 CLUSTER-CALL-TUTORIAL.md，不凭旧印象封装框架已有能力。
2. 展示简短方案和有必要的取舍，区分用户已确认约束、当前实现和待确认建议；明确本次改哪些文件、依赖哪些接口、如何验证。用户未确认前不写该节点业务代码。
3. 用户确认后完成该节点的最小改动和相关验证，说明 GSR 用法依据、保留/删除的设计及未完成依赖。需要跨节点改动时先说明，再等待明确授权。
4. 展示改动供审查；提交按 AGENTS.md 的明确授权执行，不自动进入下一节点。

common 与 typeclient 按当前节点实际需要配套调整，不提前设计下一节点。协议规划仍集中在 PROTOCOL.md；实际跨节点接口变动要先确认，不能用临时适配层掩盖尚未决定的职责。

当前下一步是 access 的方案审查，本轮仅调整执行计划，不改业务实现。Online 现有方案在该节点审查时重新讨论，不默认保留其协调流程，也不直接删除已实现代码。

完整 demo 的最终验收仍是：用户名密码登录 → 多人同组 → 满员开局 → 共享目标与个人计分 → 局末全组结果（含断线与零分成员）→ 持久化 → 同事务积分到账 → 已保存结果的故障恢复。局末冻结和测试通过不能代替结算到账。节点方案确认后再确定联调顺序及相关故障用例。

后续 access-02、三人斗地主、第三方登录及负载验证分别展示方案，不沿用旧阶段表自动开工。约 10,000 条/秒仍为待测目标，报告区分客户端请求、推送、Cluster 消息与结算事务；压力和跨进程/MySQL 验收按实际节点改动与联调范围显式启用。

## 13. 参考基线

| 来源 | 基线与阅读位置 | 采用范围 |
| --- | --- | --- |
| ../gsr | 本地 1.0，尚未发布；依赖/阅读 revision 286ea6f62b7398c432ba9e42fb70a0e619b6d1d8 | gsr.Run、Service、Gate/Protocol、Cluster 和 SQL 宿主边界；公开 API 以 CONTEXT.md、docs/API-DESIGN.md 为准；Cluster 调用参考 docs/CLUSTER-CALL-TUTORIAL.md |
| ../../skynet_bt/nbgameserver | gameofhlddz/（main、baseattackd、attackd1、roomd1）；gameofshrew/（main、roomd、agent） | 三人斗地主优先 gameofhlddz，不选四人 gameofsrddz；打地鼠仅补充流程问题，不照搬概率/倍率/计费或协议 |
| ../GoServerActorFsm | 阅读 HEAD ecbb971534b429643a704dfa05410687da611d9b，阅读时有用户未提交改动；internal/room、internal/gamelogic/shrew_timeline.go、internal/fsm/player.go、docs/client-sync-contract.md | 分组/座位、共享目标时间线、广播/快照和简单状态迁移；不移植 Actor/PlayerActor/FSM 框架、旧 Envelope 或地图动画 |

本轮核对 GSR 新基线与上一基线的差异仅为 Cluster 参数教程及相关文档/测试，生产 API 未变化。GSR 的 require v0.0.0 是占位版本。各节点 replace ../../../gsr，common replace ../../gsr；本地 go.work 只组装 gsrdemo，关闭 workspace 仍使用 module 的 replace。它不冻结源码，GSR checkout 变化需核对基线；正式发布后再固定发布依赖。Go 编码规范及 .gitignore 初始化来源为旧 revision b2a9f16ae78da0bade541eabbb39a9144d5e04d2，规范仅调整标题和入口，工作约定提取到 AGENTS.md。

GoServerActorFsm 默认容量 10、至少 3 人开始，demo 默认三人满员开始；所读实现的命中回包不构成全组持久化记账/恢复证据。本项目另行实现结算。三个目录均只读，参考实现不是兼容目标，不在其中执行生成、安装、格式化或测试。

当前基线的已实现边界（待逐节点审查）：密码账号由离线 bin/account 写入 configs/accounts.local.json，login 启动读入，不提供注册/账号数据库；mole 每组一个 RoomService，Directory 负责预留和满组分配，房间 Timer 与目录巡检分别推进，Online 低频有界核对阶段。结果在 ended 中冻结全部成员增量、包括断线和零分成员，progress=unpersisted；未保存结果不退出房间，容量到限拒绝新组。MySQL 账号/结果保存、同事务到账、恢复和按确认释放仍未实现，需在相关节点方案确认后逐项推进。用户要求继续保持尽可能少的状态。
