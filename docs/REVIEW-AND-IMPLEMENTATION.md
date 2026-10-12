# 修订方案评审与实施建议

日期：2026-10-12。评审对象为 [修订方案](DESIGN.md)，核对了只读 GSR 的 `CONTEXT.md`、`docs/API-DESIGN.md`、`service.go`、`cluster.go`、Gate/Protocol 示例、双节点和 MySQL 示例。

## 评审结论

方案可以进入首个业务切片开发。六种节点角色足够；第一轮完整闭环只需要五个进程。工程骨架现已初始化，业务代码、真实节点进程及数据库尚未实施或启动；以下结论仍是方案评审，不是业务运行验收结果。

补充并再次修订后，未发现要求增加框架 API 或新节点角色的阻断项。方案已经覆盖关键安全边界；实现是否满足这些边界仍须通过时序和崩溃测试验证。

## 原建议再次评审发现

下列发现针对补充前仍存在的方案缺口，已落实到 DESIGN.md；不作为当前代码缺陷。

| 级别 | 触发与影响 | 修订后的最小方案 | 验证 |
| --- | --- | --- | --- |
| P1 | 新连接 C 已接管，旧 A→B 的绑定 Call 迟到；只比较目标 LoginID 会被改回 B | 房间比较 ExpectedBindingToken；同用户未确认迁移不并发推进；移除也改变令牌避免空值复用 | 控制消息时序，C 接管后投递旧绑定及旧离开，C 不变 |
| P1 | Online 重启空目录，旧房间仍持有玩家；允许新进房会出现同用户双房间 | 首版协调重启 access/游戏节点，确认旧进程退出才开放；不承诺保留进行中的房间 | 旧节点不可确认退出时拒绝新绑定；已保存结算仍恢复 |
| P1 | 游戏调用结束、MySQL 提交或回复丢失；另造 ID 或直接重试可能重复加分 | 冻结 ID、持久化 pending、数据库唯一约束、事务和规范内容比较 | 真 MySQL 并发重复、冲突、提交后丢回复、pending 完成标记前强杀 |
| P1 | Timer 到期时房间 Mailbox 满；GSR 本次触发结束，后续无人操作，局可能不再结束 | 每命令按时间推进＋独立目录巡检补推进；超时/巡检失败可观测 | 模拟 Timer admission 失败，无客户端新请求时最终冻结结果 |
| P2 | Online/Room/Agent 互相 Call，或房间逐个等待推送/结算，等待期间同 Service 不处理消息 | 有向等待链、推送 Send、独立结算恢复 Service | 慢结算/慢客户端下其他房间和玩家继续操作 |
| P2 | 两种登录方式被当作同一账号，但第三方身份首次登录并未关联 | 明确认证后的账号关联；issuer/subject 唯一映射 | 并发关联拒绝冲突，凭邮箱/同名不能合并 |
| P2 | 跨节点直接传 Go struct，或以 Source 当远端用户身份；与现有 GSR wire 语义不符 | 复杂载荷显式 Protobuf bytes；信任内部 access 并检查房间业务绑定 | 真实进程编解码和错误类型路径；旧 LoginID 拒绝 |

证据入口：GSR `cluster.go` 的 Call 明确等待期间不处理下一条消息；`CONTEXT.md` 的 Source 不进入 wire；`examples/twonode/README.md` 说明规范值类型及不恢复 struct；`docs/API-DESIGN.md` 的 Timer admission 失败结束本次触发、名称清理及 Cluster Quiesce 约束。

## 首版取舍及剩余风险

1. Online 位置变化由一个 Service 协调，有限房间 Call 会短暂拖慢其他用户的目录操作。先测登录/进房延迟，确有瓶颈再按 UserID 分片；不要先加入多个争抢同一目录的实例。
2. Online 重启需要协调终止接入及进行中的游戏，代价明显，但规则可验证。保留活房间的重建作为后续独立切片，不在首轮隐含承诺。
3. 只保证已持久化的局末结果恢复。MySQL 不可用且 Room panic/进程崩溃时，未保存结果可能丢失；未保存状态不能呈现“积分已到账”。
4. 单 SettlementService 串行事务可能成为吞吐瓶颈，但 10,000 条游戏消息/秒不等于 10,000 次结算/秒。先测真实局末频率和积压，再决定结算并发，数据库唯一约束始终保留。
5. 房间巡检只能在有限负载和正常运行的目录 Service 下提供补推进。持续过载要拒绝新局；目录自身失败必须被发现和恢复，不能声称 Timer 永不丢失。
6. 第三方提供商、斗地主牌型变体和积分公式在相应切片前确定；它们不阻挡密码登录与打地鼠闭环。

## 实施顺序

### 切片 A：工程与真实通信

初始化目录采用一个 Git 仓库、多独立 module：

```text
common/                # 独立 Go module：model、command、proto、gen
nodes/access/          # 独立 Go module：cmd/access、internal/access
nodes/login/           # 独立 Go module：cmd/login、internal/login
nodes/online/          # 独立 Go module：cmd/online、internal/online
nodes/mole/            # 独立 Go module：cmd/mole、internal/mole
nodes/settlement/      # 独立 Go module：cmd/settlement、internal/settlement
typeclient/            # 独立 npm 项目：CLI、机器人及 TS 生成类型
configs/ + migrations/
scripts/ + tests/      # 工程操作及真实进程/数据库验收
```

根目录 go.work 只组装本仓库 module，示例纳入 Git；GSR 使用只读 ../gsr 的本地 1.0 基线，节点 go.mod 自身声明 replace，不依赖未发布的远端版本。关闭 workspace 验证独立 module 的本地依赖接线。节点之间不导入彼此实现；SQL 留在拥有数据的节点内，不先抽通用 persistence 或 Repository 框架。业务协议从 common/proto 同时生成 Go 与 TypeScript 类型，输出只在 gsrdemo 内。

工程初始化只交付可编译包骨架和工具链，不代表切片 A 的真实通信验收完成。各节点实际 main、配置和 readiness 在通信接线时实现，不启动空服务冒充业务。Go 风格和 .gitignore 从 GSR 复制，AGENTS.md 仅提取工作习惯，三个参考仓库均严格只读。

斗地主目录等到对应切片再建。宿主组装具体 Service，注入配置/连接池等明确资源；不搭依赖注入容器。启动脚本为每节点使用独立日志目录，等待 readiness，失败时收回自己启动的子进程。

先跑 access→login、access→online、access→room 的真实 Cluster Call，以及 room→Agent 的 Push，确认业务 Protobuf bytes 和 GSR Envelope 接线。复杂 payload 不直接传 Go struct。只使用回环或可信内网开发配置。

### 切片 B：登录与打地鼠状态

打地鼠实现最小多人同组完整玩法；借鉴 GoServerActorFsm 的分组、共享目标时间线、组内广播和快照，不移植它的 Actor/FSM 框架，也不完整复刻 gameofshrew 的概率、倍率和计费。此切片为中间验收，必须继续完成切片 C 的持久化结算才能交付首轮完整 demo。

先写纯业务验证：人数阈值、座位与满组分配、组间隔离、多人竞争同一实例只奖励一人、目标窗口/重复操作、全组局末只冻结一次、旧 Timer 无效。再接 Agent、Online、每组一个 MoleRoomService 和四进程、多 TS 客户端；满员后才启动共同时间线。

同一轮建立 LoginID 和比较绑定的最小协议，不能先做无条件覆盖登录以后再补。测试正常路径和超时路径；对尚未加入的恢复能力，返回明确不支持或待确认状态，不能假成功。

完成纯函数边界与真实接线后再扩展整个受影响 package 测试，不重复验证 GSR 的底层 Mailbox/来源机制。应用测试保护玩家行为和分数。

### 切片 C：结算及崩溃恢复

先写迁移 SQL 和数据库约束，再实现保存 pending、Settlement 事务、恢复 Service。用专用 MySQL 测试库验证全组积分与账本同事务提交、唯一约束竞争和真实事务；SQL mock 只能辅助检查查询/错误分支，不能代替数据库幂等验收。

故障注入点至少包括：pending 提交后、结算事务提交后回复前、结算确认后 pending 标记前。分别强杀测试自己启动的游戏/结算进程；重启后断言最终积分和每用户账本只变化一次。另测同 ID 改内容、同局换 ID、MySQL 不可用和 Commit 结果未知。

本切片完成就是第一轮里程碑：五进程、多客户端同组真实闭环、全组可查询的到账结果、可补齐的待结算记录；包括零分及断线成员，不能遗漏个人结算。不要在此之前同时展开斗地主和第三方登录。

### 切片 D：第二接入与目录故障

增加 access-02，先验证跨接入重连、两次登录竞争、旧推送/断线/游戏操作/绑定迟到。测试必须控制发送和释放时序，不依赖随机 sleep 碰运气。

将 Online 的 uncertain 查询/条件重试、业务幂等保留窗口、协调重启脚本做成可运行验收。Online 不可用时已有房间继续，新绑定拒绝；恢复时严格执行文档定义的重启边界。

把该切片放在斗地主之前，先在多人打地鼠组内验证单个玩家连接迁移不改变其他玩家、共同目标或局期限，再把已经验证的模式用于三人斗地主。

### 切片 E：斗地主

优先核对只读 nbgameserver/gameofhlddz/ 的三人流程，再固定本 demo 的规则表和积分公式，用表驱动测试覆盖牌型比较与手牌数量。接入三玩家 RoomService，三 TS 客户端/机器人跑完一局。

验收重点是非法操作不变更状态、私人手牌隔离、超时和手动动作只推进一次、重连不重置期限、三人同事务积分结算。复用业务协议/结算语义，保留两个游戏独立规则，不抽 GameService 基类。

### 切片 F：第三方登录、故障及负载

选定一家真实提供商，完成凭据验证与认证后的身份关联，再做真实回调/凭据错误验收。测试凭据和密钥不提交。

运行单房间 panic、access 强杀、慢客户端、Online 故障、数据库不可用、Timer 投递失败等演练，然后逐级提高客户端并发和消息速率。报告成功与拒绝、端到端延迟及积压；10,000 条/秒仅在目标硬件和明确业务混合下判定。

## 开发与完成门槛

- 默认在 gsrdemo 当前工作区开发，不修改 gsr、nbgameserver 或 GoServerActorFsm，不自动提交或推送；提交消息尽量中文。
- 每个切片先建立关键失败用例，再实现；测试复杂度以新增业务契约为边界，不复制整套框架测试。
- 开发期间跑最小相关验证；跨 package/module 切片完成后通过 scripts/verify.sh 枚举业务 module 全测与 vet；根目录 go test ./... 不能代替多 module 验证。涉及状态、资源生命周期的包在切片收尾跑 race，真进程/真 MySQL 测试显式开启并设总超时。
- 交付说明包括启动方法、通过的命令、故障注入断言和未覆盖范围。没有实际启动 MySQL/多进程，就不能写“恢复与幂等已验证”。
