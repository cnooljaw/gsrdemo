# GSR Demo

基于 GSR 的多节点游戏业务验证项目。gsrdemo 是唯一开发目录；../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 严格只读。

当前已实现密码登录、Online 位置协调、access 协议准入和三人同组打地鼠：满员开局、共享地鼠、竞争命中、重连快照、局末全组冻结。账号暂由离线管理命令写入本地哈希文件；局末结果标明 unpersisted，尚未接入 MySQL/积分到账，不能称为完整 demo。五节点诊断保留为显式开关。

文档入口：[业务设计与实施验收](docs/DESIGN.md)、[全部协议规划](docs/PROTOCOL.md)、[Go 编码规范](docs/GO-CODE-STYLE.md)。工作流程与提交规则集中在 [AGENTS.md](AGENTS.md)。

## 结构

| 项目 | 职责 |
| --- | --- |
| common | 共享值对象、命令契约、业务协议及 Go 生成结果 |
| nodes/access | 接入与 Agent |
| nodes/login | 认证与身份映射 |
| nodes/online | 在线目录与位置协调 |
| nodes/mole | 最小多人同组打地鼠完整流程、局末结果与结算恢复 |
| nodes/settlement | 幂等积分结算及账本 |
| typeclient | 独立 TypeScript CLI/机器人项目 |
| configs、scripts、tests | 配置、工程操作和跨进程验收；数据库切片按需建立 migrations |

每个节点角色一个项目；access-01/access-02 共用 access。nodes/landlord 在斗地主切片再建立，优先参考 gameofhlddz/。根目录没有 go.mod，一个 Git 仓库管理全部项目。

节点依赖 common 和 gsr，节点之间只通过命令契约通信，不导入对方实现。共享代码按真实需求提取，不建立通用 GameService 基类。

阅读 access 时从 [agent.go](nodes/access/internal/access/agent.go) 看连接和固定分发入口，再看 login.go（认证与连接替换）、position.go（已确认位置与进出房）、mole.go（地鼠协议）、probe.go（诊断）。这些都是同一个 Agent 的方法，共用 Mailbox；会话记录只保存身份、绑定和房间阶段，游戏快照单独缓存。common/command 只定义命令编号；内部调用直接用 GSR 的原生参数/map/list，Protobuf 只用于客户端协议。

## 准备与验证

Go 1.24 或更新、Node.js 22.15 或更新、npm；当前工具版本沿用只读 GSR 示例并锁定。从根目录执行：

```sh
./scripts/workspace.sh
npm --prefix typeclient ci
./scripts/verify.sh
./scripts/build.sh
```

本地 go.work 被忽略，示例纳入 Git，只组装本仓库 module。五个节点通过自身 go.mod 的 replace 引用只读 ../gsr，以当前本地 1.0 为基线，尚无发布版本；占位版本 v0.0.0 不表示框架的完成程度。两个业务参考仓库只用于阅读；Go 模块缓存与 npm 缓存保留。

独立 module 验证使用 scripts/verify.sh --standalone 和 scripts/build.sh --standalone，关闭 workspace，仍使用节点 go.mod 的本地 GSR/common 替换。需要保留当前兄弟目录布局；第三方依赖首次获取可能需要网络，GSR 本身无需远端下载。GSR 基线记录在业务设计末尾。

Go build 输出 bin/access、bin/login、bin/online、bin/mole、bin/settlement 和 bin/account，TS build 输出 typeclient/dist。schema 和生成结果纳入 Git，修改 common/proto 后运行 scripts/generate.sh，Go/TS/registry 输出只在本项目；工具版本和消息清单集中在协议规划中。

## 本机演示

先建立四个测试账号，密码从标准输入读取，哈希保存到忽略提交的本地文件。以下命令会交互读取密码；每个账号重复一次，账号文件写完后再启动 login：

```sh
read -s 'demo_password?测试密码（至少 8 字节）：'; echo
printf '%s\n' "$demo_password" | bin/account -username player1
unset demo_password
```

依次创建 player1、player2、player3、player4；管理入口只用于本地离线测试，不提供线上注册。再建立 configs/clients.local.json，内容是四个 `{ "username": "player1", "password": "对应测试密码" }` 对象组成的数组，密码需与账号创建时一致。该文件只供本机机器人读取，不提交。

从根目录启动五个进程，另一个终端运行四个独立 WebSocket 客户端的演示：

```sh
./scripts/run-local.sh
npm --prefix typeclient run mole
```

演示验证未登录拒绝、错误密码、三人同组与第四人隔离、竞争命中与重试、等待组离开、同局重连、断线零分成员仍进入局末结果；最后输出 complete/progress=unpersisted。所有客户端始终只连接 access。默认一局 15 秒，mole 配置可指定 group_size/round_ms/window_ms/gap_ms 四项，范围由加载器校验。

两个 CLI 均可指定 --url（默认 ws://127.0.0.1:7888/ws）；mole 另有 --accounts（默认 configs/clients.local.json）。如需原五节点探测，显式将 access 配置 enable_probe 设为 true，重启 access，再运行 `npm --prefix typeclient run client -- --text "hello demo"`。默认关闭该诊断协议，返回 disabled。探测校验关联并等待四节点响应和 mole Push；总 Call 预算 2 秒、CLI 等待 5 秒。

Ctrl-C 停止 run-local.sh 本次启动的子进程，任一节点提前退出也会收回剩余进程；清理只针对自己的 PID。日志按实例在 logs/ 下隔离。也可从根目录单独执行 bin/<角色> -config configs/<角色>-01.json，成功输出 JSON ready，Ctrl-C/SIGTERM 有序退出。

配置是本地开发示例；Gate 为 Plain，监听 127.0.0.1:7888。Cluster 的 GSR Open 实际绑定 0.0.0.0，配置路由中的 127.0.0.1 是拨号地址，不代表其监听限制在回环；本轮用于受信任开发环境。

JSON 配置共用 nodes 路由表，各实例独占日志目录，路径按启动工作目录解释。加载拒绝未知字段、多个对象、缺失监听路由和非法地址，当前 listen 要匹配角色；access-02 在对应切片接入。私密配置用被忽略的 configs/*.local.json/*.local.yaml，数据库字段随持久化切片加入。

真实进程测试显式运行：

```sh
./scripts/process-test.sh
./scripts/process-test.sh --race
```

测试预留临时本机端口、创建临时测试账号并等待 readiness。通信用例验证五节点探测和故障；业务用例只启动 access/login/online/mole 四节点，运行上述完整多人状态切片。--race 模式同时检查所有子进程和测试。普通 verify.sh 不启动进程或数据库。

验收记录（2026-10-12）：统一脚本的 module test/vet 和 TS 类型检查通过；受影响状态包的 race 通过；process-test.sh --race 的五节点探测与四节点多人用例全部通过，合计约 26 秒。通信阶段曾验证 run-local.sh 的启动及 Ctrl-C 清理。MySQL 保存、积分到账与结算恢复尚未验收。

## 工程命令

以下均从根目录执行，Go 脚本复用 .cache/go-build，显式 GOCACHE 可覆盖，不清理测试缓存。

| 命令 | 用途 |
| --- | --- |
| ./scripts/workspace.sh | 创建本地 go.work，已有配置保留 |
| ./scripts/build.sh [--standalone] | 构建各 module、节点二进制和 TS；standalone 关闭 workspace |
| ./scripts/verify.sh [--standalone] | 各 module 测试/vet 和 TS 类型检查，默认跳过真实进程 |
| ./scripts/generate.sh | 生成 Go/TS/registry；npm --prefix typeclient run generate 等价 |
| ./scripts/run-local.sh | 前台启动五节点，等待 readiness，并负责自己进程的清理 |
| ./scripts/process-test.sh [--race] | 构建并显式执行真实进程验收，race 同时检测节点与测试 |
| npm --prefix typeclient run check / build / client / mole | 独立客户端的类型检查、构建、探测与多人演示 |

## 业务完成标准

先实现多人分别用户名密码登录 → 分配同组及座位 → 满员开局 → 地鼠出现/命中 → 分数更新 → 局结束 → 持久化待结算 → Settlement 事务记账 → 查询积分到账，再验证故障恢复。打地鼠简化玩法，不完整复刻旧游戏；结算不可省略，局结束不能等同于积分到账。

首轮完成是 access、login、online、mole、settlement 五进程完整闭环。同组共享地鼠和局期限、分别记分；同一实例只奖励首个合法命中者。重复/过期命中不得加分，全组结果同事务结算且重复提交不得重复记账，保存过的待结算结果重启后必须补齐。之后加入 access-02 和三人斗地主。

用户始终只连接 access，用户状态仅未登录/大厅/房间，Room 阶段仅等待/进行/结束。Agent/Online 用一条待确认操作记录处理绑定与进出房，结算确认放在结果记录中。协议规划保持一个入口，按业务开始实现时再拆分 .proto。

当前内存容量有界：最多 512 个 Online 用户、每用户 128 条位置结果、64 个房间、目录 1024 条分配记录、每房间 512 条控制记录和 512 条命中记录。位置记录不淘汰，达到上限返回 capacity；未保存结果保留在 ended 房间，不清空后另开局。MySQL 切片完成后再按持久化确认和保留期释放。密码使用固定 golang.org/x/crypto v0.26.0 的 Argon2id（标准库未提供），单认证 Service 串行哈希，并限制输入和速率。
