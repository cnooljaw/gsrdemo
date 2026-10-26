# 目标与逐节点计划

## 1. 目标和当前边界

首个 demo 完成多人同组打地鼠的完整过程：用户名密码登录 → 分组与座位 → 满员开局 → 共享地鼠与个人计分 → 局末全组结果 → 持久化与积分结算 → 已保存结果的故障恢复。玩法保持最小实现，结算不能省略。用户始终只连接 access。

2026-10-24 用户明确：实际同时在线目标约 12,000 人，100,000 人用于系统容量余量，约为实际目标的 8.3 倍。两档分别验证，容量估算不作为已完成验收；Online 的后续测试计划见第 6 节。

三人斗地主后续单独确认，优先参考 nbgameserver 的 gameofhlddz/；多人分组和共享目标可参考 GoServerActorFsm。参考代码提供知识，不作为兼容目标。

当前 access 已具备 MAC Gate/Agent 连接基础，login 已实现内部用户名密码认证与管理命令，online 已实现内部位置目录；mole、settlement 和 TypeScript 保持骨架。尚无客户端登录协议、access 到 Online 的业务接入、游戏状态或游戏演示。旧实现和旧方案已移除，不能当作已确认设计继续扩展。

每个节点角色一个独立 Go module，源码默认放该项目根目录；common 仅包含共享配置代码。没有调用者的通用框架、协议目录和业务包不提前建立。

## 2. 推进顺序

| 顺序 | 节点 | 先审查并确认的范围 |
| --- | --- | --- |
| 1 | access | Gate/Agent 的职责和生命周期、连接拥有的数据、最少状态、客户端协议准入及 GSR 接入方式 |
| 2 | login | 最小认证能力、账号来源、输入输出、认证 Service 的职责与调用边界 |
| 3 | online | 是否需要独立节点，位置数据由谁拥有，哪些协调和状态确有必要 |
| 4 | mole | 多人分组、共享时间线、命中与计分、局末结果，以及与结算的接口边界 |
| 5 | settlement | 最小积分账本、持久化职责、重复与冲突处理、事务和故障恢复 |

每次只推进一个节点：

1. 只读核对现有骨架、实际依赖版本和 GSR 文档/示例。
2. 展示简短方案：职责、数据 owner、最少状态、必要协议与调用链、失败与生命周期边界、修改范围和验证方式。
3. 用户确认后实现当前节点。common、协议和 TypeScript 只配套加入该节点必需的内容；其他节点的依赖问题先列出，不自动扩展实现。
4. 展示改动和实际验证，等待用户决定是否提交及推进下一节点。

access 连接基础和 login 内部认证已提交；online 最小目录已获确认，实现与验证后交用户审查。业务状态尽量少，行为相同的状态优先合并；不为调用、重试和存储步骤增加状态。业务契约见 [PROTOCOL.md](PROTOCOL.md)，按使用范围决定是否需要 .proto。

完成五个节点后再联调打地鼠全流程。三人斗地主、access 多实例和第三方登录分别先确认方案，不提前开工。

## 3. access 第一步（已确认）

- MainService 通过 self.Node().OpenCluster/CreateGate 组装 Cluster 和独立 GateService；每条连接由 Gate 创建一个 Agent。采用 SecurityModeMAC，本机仍使用 WS；先完成 33 字节 X25519 握手，上行使用 seq/Envelope/MAC，下行使用 Envelope，不接受 Plain 降级。监听地址及连接数、帧大小、出站队列上限放在 access 的私有 YAML 配置中，握手等待沿用 GSR 默认 5 秒。
- Gate 拥有连接和 I/O；Agent 只持有 ClientSession 与具名协议 Dispatcher，不接触 socket 或连接表，不持有业务状态，也不增加状态机。
- 调用链为客户端 → Gate → Agent.HandleClientMessage → protocol.Dispatcher。只使用 GSR 通用目录和空业务 Handler 表，错误处理沿用框架规则；没有本项目的协议编号、.proto、生成工具或跨节点业务调用。
- Gate 的断线通知进入 Agent.Handle，Agent 调用 self.Exit 后立即返回；OnStop 调用幂等的 ClientSession.Close。MainService 完成组装后退出，Gate 和 Agent 继续由 Runtime 管理；进程停机由 gsr.Run 收尾。任一步启动失败都向宿主返回错误。
- 修改范围是 access 的入口、MainService、根目录 agent.go 和相关测试/配置；common 仅拆出已有的 YAML 读取和宿主配置解析，角色私有字段留在节点中。其他节点和 TypeScript 保持骨架。
- 验证配置边界、断线清理、真实 MAC 握手与递增序号的消息往返、未握手客户端拒绝、单连接故障隔离、停机关闭连接/监听，以及 Gate 启动失败后的 Cluster 释放；对 access 运行 race。密码学和完整攻击向量沿用 GSR 的验证，不在业务项目复制整套测试。

## 4. GSR 与参考边界

- 节点固定 GSR v1.0.2，tag commit 为 6710a766ca40c06a068e52c52456b39f8b3fe78e；节点仅对本仓库 common 使用本地 replace。
- 本轮只读源码与文档 revision 同该 tag；公开 API 以实际依赖版本为准。升级依据为 [v1.0.2 发布说明](https://github.com/cnooljaw/gsr/releases/tag/v1.0.2)、[ADR 0012](https://github.com/cnooljaw/gsr/blob/v1.0.2/docs/adr/0012-node-capabilities-and-service-logging.md)、[ADR 0013](https://github.com/cnooljaw/gsr/blob/v1.0.2/docs/adr/0013-client-protocol-boundaries.md)和业务 AGENTS 模板。
- 接入参考 [独立业务项目开发指南](https://github.com/cnooljaw/gsr/blob/v1.0.2/docs/APPLICATION-DEVELOPMENT.md)；Cluster 调用优先看 [CLUSTER-CALL-TUTORIAL.md](https://github.com/cnooljaw/gsr/blob/v1.0.2/docs/CLUSTER-CALL-TUTORIAL.md)，直接使用框架已有的参数与调用能力。
- Service、状态所有权、消息快照和资源关闭的工作约束见 [AGENTS.md](../AGENTS.md)，编码规则见 [GO-CODE-STYLE.md](GO-CODE-STYLE.md)。
- ../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 均只读，不在其中生成、安装、格式化或验证。

### v1.0.2 接入知识

- Gate 的 Protocol/ProtocolMAC/ProtocolPlain 及配置字段 Protocol 改名为 SecurityMode/SecurityModeMAC/SecurityModePlain 与 SecurityMode，不提供旧名别名；握手、seq/MAC、TLS、wire 和生命周期不变。
- 安全模式与业务协议独立，Agent 选择具体 Dispatcher。本项目继续使用 Protobuf `protocol` 包和原 Envelope；新增的 `protocol/sproto` 保留自己的协议与每连接会话，不在此次升级中接入，也不增加统一 Codec。
- `self.Node()` 借用当前 Run 的唯一节点对象，仅在当前回调内使用。名称发布用 RegisterService，监听用 OpenCluster/OpenHTTP，Gate 创建用 CreateGate，ClusterListenAddress 只用于观测；Node 不提供 Close、业务通信或内部资源 getter。
- `self.Cluster()` 仅提供来源绑定的 Send/Call。应用配置使用 `gsr.RuntimeConfig` / `gsr.ClusterConfig`；`self.Logger()` 返回只有 Info/Error 的 `ServiceLogger`，共享 Logger 由 Run 关闭。
- MainService 完成组装后可以 Exit；节点监听和已创建的 Service 继续运行。CreateGate 创建独立 GateService，由 Runtime 管理；CreateService/CreateGate 的 ctx 仅用于启动，不是所创建 Service 的长期生命周期。名称归目标 Service，注册者退出不撤销。
- MainService 启动失败由 Run 统一收尾；普通 Service 启动失败只清理自身，不自动停止已经创建的其他 Service。消息、Source、wire、ValueCodec 与关闭顺序没有变化。
- 远程 Handler 使用 `int64` / `uint64` / `float64` / `[]any` / `map[string]any` 等规范参数；Dispatcher 不做隐式类型转换，集合的 nil/空状态不作为远程业务语义。本地 Args 浅拷贝不隔离内部集合，参数和结果仍须独立快照。
- 节点表 host 为访问地址，OpenCluster 取 port 绑定 `0.0.0.0`，访问限制归部署网络；端口零仅用于动态监听测试，不作为出站路由。本项目集中维护 YAML 节点表，不额外拆分监听配置。

框架升级只更新五个节点的依赖、启动/测试接线及文档，不保留旧入口兼容包装；login 的业务增量单独记录在下节。

2026-10-14 验证：`./scripts/dev.sh check`、`./scripts/dev.sh build` 和 access 的 `go test -race ./... -timeout=2m` 通过，包含真实 WebSocket 接入、停机及启动失败清理。其余节点只验证骨架编译/静态检查，未验收跨节点业务或 MySQL。

2026-10-23 升级 v1.0.2：五个节点的 go.mod/go.sum 已更新，access 迁移为 `SecurityMode: gate.SecurityModeMAC`。统一 check/build 与 access race 通过，覆盖真实 MAC WebSocket、连接隔离、启动失败清理及 login 的 TCP Cluster 认证；五个构建产物均确认依赖正式 v1.0.2。此轮未重跑真实 MySQL 验收。

## 5. login 第一步（已确认）

- 单个 LoginService 负责用户名密码验证，MySQL `gsrdemo_login.accounts` 保存稳定 UserID、唯一用户名和密码摘要；没有账号缓存、Token、LoginID、在线状态或额外状态机。
- 按用户确认采用标准库无盐 SHA-256，仅用于示例；不引入 Argon2id。账号由本节点 `-create-user` 管理命令创建，密码从标准输入读取；不增加远程注册接口。
- 宿主通过 GSR mysqlclient.Open 创建原生数据库池，Run 返回后关闭。MainService 创建并命名 LoginService、打开 Cluster 后退出；Service 仅借用数据库池，每次 SQL 最多等待 2 秒。
- Cluster 命令接收两个 string，返回 uint64；命令、名称及错误定义与 LoginService 放在 loginservice.go，文档契约集中在 PROTOCOL.md。当前没有其他节点消费者，不提前抽共享包。MySQL 驱动沿用 GSR 示例的 v1.10.1；sqlmock v1.5.2 仅用于测试，标准库没有对应模拟驱动。
- YAML 的 mysql_dsn 为 login 私有字段，公开模板不含凭据；本机凭据保存在被忽略的 login-01.local.yaml。schema.sql 只初始化本项目专用库，不修改 GSR 示例库，节点启动不自动建表。
- 默认测试使用 SQL mock 和两个真实 Runtime 的 TCP Cluster 链路，覆盖正确/错误凭据、重复认证、非法输入、数据库错误与账号损坏。真实 MySQL 验收显式开启，使用独占测试账号并清理；未运行时不声称数据库验收完成。
- 本轮不修改 access 或 TypeScript 的业务逻辑，不推进 online；下一节点或客户端登录接入另行确认。

2026-10-14：统一 check（Go test/vet、TS 类型检查）、login race 与 login 构建通过。GSR 的本机 MySQL 凭据已只读复用到本项目私密 YAML；管理员已初始化专用库。`GSRDEMO_MYSQL_INTEGRATION=1 go test -run '^TestLoginLiveMySQL$' -timeout=30s -v` 通过：真实 MySQL 创建账号、拒绝重复覆盖，通过 TCP Cluster 两次认证返回同一 UserID，错误密码/不存在账号均拒绝，测试账号清理成功。此验收不包含 access 客户端登录或跨进程业务联调。

## 6. Online 容量估算与待测计划

- 客户端连接、握手与重连由多个 access 承担；Online 保存位置目录，承受节点间 Bind/Lookup/Unbind 等调用压力。游戏操作直接到房间，不逐次查询 Online，也不把客户端心跳转发给 Online。
- 按单 Online 节点、单 OnlineService 起步，是否分片依据真实业务压测决定。以下是估算与测试目标，不直接代表容量已验收，实际测试证据在本节末尾记录。
- 常态估算假设平均在线 30 分钟，每次上线/下线各一次目录调用，另按平均每 10 分钟一次房间位置更新预留：`人数 ÷ 1800 × 2 + 人数 ÷ 600`。额外查询另计；房间更新接口尚未实现。
- 集中重连估算按每人一次 Bind 加一次 Lookup，即 `人数 ÷ 重连秒数 × 2`。这是 Online 的业务 RPC/s，不是客户端连接数；全用 Call 时请求和回复报文数约为其两倍，不等同于 TCP 包数。

| 档位 | 目录记录数 | 常态估算 | 全量用户 60 秒重连 | 全量用户 10 秒重连 |
| --- | ---: | ---: | ---: | ---: |
| 实际在线目标 | 12,000 | 约 33 RPC/s | 400 RPC/s | 2,400 RPC/s |
| 系统容量余量 | 100,000 | 约 278 RPC/s | 约 3,333 RPC/s | 20,000 RPC/s |

Online 实现后，先验证 12,000 条记录下的持续混合调用和上述两档重连峰值；再验证 100,000 条记录、1,000 RPC/s 持续负载，以及约 3,333～5,000 RPC/s 的重连峰值。100,000 人十秒重连的 20,000 RPC/s 作为边界探索，不预先承诺通过。测试使用多个调用方经真实 TCP 汇聚到同一个 OnlineService，覆盖 Bind/Lookup/Unbind 的真实状态变化与 LoginID 竞争；测量前明确持续时长、延迟和拒绝率门槛，记录实际输入/完成速率、P99/最大延迟、MailboxFull、超时、CPU、GC、堆/RSS、排空时间及最终目录正确性。节点调用压测与 access 长连接压测分别报告。

参考证据：[GSR 性能基线](../../gsr/docs/PERFORMANCE-BASELINE.md)、[混合负载](../../gsr/docs/MIXED-LOAD-VALIDATION.md)。历史测试为单热点 Service 本地 Send 约 9,861/s，以及三分钟总量约 9,800/s（含跨节点 Send 约 1,960/s）；均非吞吐上限，也不直接证明当前版本的单热点跨节点 Call 容量。2026-10-24 独立内存估算程序在 Go 1.24.13/arm64 下使用 `map[uint64]location`，location 含 36 字节 LoginID、9 字节 AccessNode、40 字节 AgentName，字符串分别分配；100,000 条记录 GC 后堆增量约 18.7 MiB。此结果不包含完整节点、队列和网络缓冲，不替代业务实现后的内存测量。

2026-10-24 首轮 Online 容量样本通过（GSR v1.0.2、Go 1.24.13、darwin/arm64）；命令与负载构成见第 7 节，非 race 采样，总耗时约 125 秒。

| 预装记录 | 目标 RPC/s | 实际完成 RPC/s | 60 秒完成数 | P50 | P99 | 最大调用延迟 | 发生器最大迟到 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 12,000 | 2,400 | 2,399.92 | 144,000 | 40.334 μs | 219.458 μs | 84.578 ms | 187.764 ms |
| 100,000 | 5,000 | 4,999.83 | 300,000 | 38.291 μs | 163.750 μs | 40.962 ms | 38.626 ms |

两档调用无错误，所有期望结果、最终逐条位置与清理核对通过，未观察到 Call 返回 MailboxFull 或超时。发生器按绝对时间追赶批次，平均速率达标不表示均匀输入；延迟包括调用端结果核对开销。开发机未做 CPU 隔离，首档采样期间还执行了一次局部 check/build；保留原始尾延迟，不据此归因或宣称生产 SLA。此结果支持单节点方案的初步容量选择，不代表多个真实 access 进程、客户端重连、实际业务流量比例或长期稳定性已验收；第 6 节的完整联调计划仍待执行。

## 7. online 第一步（已确认）

- 一个 OnlineService 独占 `map[uint64]onlineLocation`，按 UserID 保存 LoginID、AccessNode、AgentName。仅提供 Bind/Lookup/Unbind，不保存额外在线布尔值、房间状态、历史登录或墓碑，不访问 MySQL/Redis，不启动业务 goroutine。
- AccessNode 与 AgentName 共同定位连接，支持多个 access 使用相同 Agent 名称。未来各 access 使用独立 Cluster 路由名；当前 access 的多实例配置、客户端认证与路由接线仍是后续范围，不在本轮修改 common、access 或 TypeScript。
- 新 LoginID 替换当前绑定，返回旧位置；相同当前 LoginID 只接受完全相同的地址。Unbind 仅匹配 LoginID，不按接入节点批量清理。命令使用规范标量参数，结果为独立快照或 nil，具体错误与超时/重试约束见 [PROTOCOL.md](PROTOCOL.md#online-位置目录)。
- 沿用 GSR v1.0.2 的 Cluster 教程和双节点示例：MainService 创建并命名 OnlineService，OpenCluster 后退出；Runtime 管理 Service，Run 管理节点。Online 不同步调用其他节点；重启清空目录，后续接入重新绑定，不承诺异常时目录实时准确。
- 代码与命令定义放 onlineservice.go；没有真实业务消费者时不提前提取 common 契约。普通测试覆盖跨节点替换、同 ID 冲突、独立快照、非法输入、两接入节点竞争、旧连接迟到清理、真实 TCP 及重启空目录。
- 容量测试使用 `GSRDEMO_ONLINE_LOAD=1 go test -run '^TestOnlineClusterLoad$' -v -timeout=3m` 显式开启；四个测试接入节点在同一测试进程内各运行一个 Runtime/Cluster，经 TCP 汇聚到实际 Online MainService。两档分别预装 12,000/100,000 条记录，定速 60 秒，目标 2,400/5,000 RPC/s。等量混合 Bind 替换、Lookup、旧 LoginID Unbind；计时后逐条核对最终位置、成功解绑与重复解绑。它是有界四路并发的目录压力样本，不是客户端重连或跨进程部署验收。
- 容量样本不增大队列、不重试失败调用；任何调用/结果错误失败，并检查实际完成速率达到目标的 98%。P99、最大延迟和发生器最大迟到如实报告，不预设生产 SLA；堆/GC 是同进程客户端、服务端与夹具聚合采样，后采样还包含最终核对和清理，不等于 Online 独占 RSS 或精确稳态内存。CPU/RSS、长时间稳定性、实际 access 多实例和全量重连恢复仍需后续联调验收。

2026-10-24 实现验证：`./scripts/dev.sh check online`（common/online test 与 vet）、`./scripts/dev.sh build online`、online 的 `go test -race ./... -timeout=2m` 及上述容量样本均通过。真实 TCP 用例运行多个独立 Runtime/Cluster，并未启动多个 OS 进程；重启测试为有序停止节点后创建新的空目录，不等于强杀恢复演练。没有新增依赖或修改其他节点业务。
