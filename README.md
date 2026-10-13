# GSR Demo

基于 GSR 的多节点游戏业务验证项目。gsrdemo 是唯一开发目录；../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 严格只读。

当前代码已实现密码登录、Online 位置协调、access 协议准入和三人同组打地鼠：满员开局、共享地鼠、竞争命中、重连快照、局末全组冻结。账号暂由离线管理命令写入本地哈希文件；局末结果标明 unpersisted，尚未接入 MySQL/积分到账，不能称为完整 demo。五节点诊断保留为显式开关。

后续改为一个节点一个节点推进：access → login → online → mole → settlement。每个节点先展示方案及 GSR 用法依据，用户确认后再实现；当前 Online 协调、换绑和巡检等设计仍待审查，已有代码与测试不代表设计已定案。执行顺序见 [逐节点计划](docs/DESIGN.md#12-逐节点执行计划与验收)。

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

每个节点角色一个项目；Go 入口 main.go、业务源码和测试默认直接放该项目根目录，统一为 package main，实际需要独立包时再分目录。access-01/access-02 共用 access。nodes/landlord 在斗地主切片再建立，优先参考 gameofhlddz/。根目录没有 go.mod，一个 Git 仓库管理全部项目。

节点依赖 common 和 gsr，节点之间只通过命令契约通信，不导入对方实现。共享代码按真实需求提取，不建立通用 GameService 基类。

阅读 access 时从 [agent.go](nodes/access/agent.go) 看连接和固定分发入口，再看 login.go（认证与连接替换）、position.go（已确认位置与进出房）、mole.go（地鼠协议）、probe.go（诊断）。这些都是同一个 Agent 的方法，共用 Mailbox；会话记录只保存身份、绑定和房间阶段，游戏快照单独缓存。common/command 只定义命令编号；内部调用直接用 GSR 的原生参数/map/list，Protobuf 只用于客户端协议。

## 准备与验证

Go 1.24 或更新、Node.js 22.15 或更新、npm；当前工具版本沿用只读 GSR 示例并锁定。从根目录执行：

```sh
npm --prefix typeclient ci
./scripts/dev.sh check
./scripts/dev.sh build
```

本地 go.work 被忽略，示例纳入 Git，只组装本仓库 module；编辑器需要时执行 `cp go.work.example go.work`，已有文件保留。common 和五个节点固定依赖已发布的 `github.com/cnooljaw/gsr v1.0.0`；节点通过 replace 引用本仓库 common。../gsr 只用于阅读框架文档，两个业务参考仓库只用于阅读；Go 模块缓存与 npm 缓存保留。

构建和检查统一使用 scripts/dev.sh，逐个 module 执行并固定 GOWORK=off，依赖选择以各自 go.mod 为准。首次获取 GSR 和其他第三方依赖需要网络，构建不依赖兄弟目录中的 GSR checkout。GSR 基线记录在业务设计末尾，接入约定参考其独立业务项目开发指南并提取到 AGENTS.md。

Go build 输出 bin/access、bin/login、bin/online、bin/mole、bin/settlement；账号管理使用 bin/login account，TS build 输出 typeclient/dist。schema 和生成结果纳入 Git，修改 common/proto 后运行 scripts/generate.sh；GSR 协议依赖与 registry 工具均从 common/go.mod 选定的模块读取，Go/TS/registry 输出只在本项目。工具版本和消息清单集中在协议规划中。

## 本机演示

先建立四个测试账号，密码从标准输入读取，哈希保存到忽略提交的本地文件。以下命令会交互读取密码；每个账号重复一次，账号文件写完后再启动 login：

```sh
read -s 'demo_password?测试密码（至少 8 字节）：'; echo
printf '%s\n' "$demo_password" | bin/login account -username player1
unset demo_password
```

依次创建 player1、player2、player3、player4；管理入口只用于本地离线测试，不提供线上注册。再建立 configs/clients.local.yaml，内容为四条 YAML 账号记录，密码需与账号创建时一致：

```yaml
- username: player1
  password: "对应测试密码"
- username: player2
  password: "对应测试密码"
- username: player3
  password: "对应测试密码"
- username: player4
  password: "对应测试密码"
```

该文件只供本机机器人读取，不提交。

从根目录启动五个进程，另一个终端运行四个独立 WebSocket 客户端的演示：

```sh
./scripts/run-local.sh
npm --prefix typeclient run mole
```

演示验证未登录拒绝、错误密码、三人同组与第四人隔离、竞争命中与重试、等待组离开、同局重连、断线零分成员仍进入局末结果；最后输出 complete/progress=unpersisted。所有客户端始终只连接 access。默认一局 15 秒，mole 配置可指定 group_size/round_ms/window_ms/gap_ms 四项，范围由加载器校验。

两个 CLI 均可指定 --url（默认 ws://127.0.0.1:7888/ws）；mole 另有 --accounts（默认 configs/clients.local.yaml）。如需原五节点探测，显式将 access 配置 enable_probe 设为 true，重启 access，再运行 `npm --prefix typeclient run client -- --text "hello demo"`。默认关闭该诊断协议，返回 disabled。探测校验关联并等待四节点响应和 mole Push；总 Call 预算 2 秒、CLI 等待 5 秒。

Ctrl-C 停止 run-local.sh 本次启动的子进程，任一节点提前退出也会收回剩余进程；清理只针对自己的 PID。日志按实例在 logs/ 下隔离。也可从根目录单独执行 bin/<角色> -config configs/<角色>-01.yaml，成功输出 JSON ready，Ctrl-C/SIGTERM 有序退出。

配置是本地开发示例；Gate 为 Plain，监听 127.0.0.1:7888。Cluster 的 GSR Open 实际绑定 0.0.0.0，配置路由中的 127.0.0.1 是拨号地址，不代表其监听限制在回环；本轮用于受信任开发环境。

节点配置使用 YAML，地址只在 [configs/nodes.yaml](configs/nodes.yaml) 配置一次。各实例通过 `nodes_file: nodes.yaml` 引用该表，例如 [access-01.yaml](configs/access-01.yaml)：

```yaml
instance: access-01
listen: access
nodes_file: nodes.yaml
log_directory: ../logs/access-01
gate_address: 127.0.0.1:7888
enable_probe: false
```

nodes_file、log_directory 和 accounts_file 的相对路径以节点配置文件所在目录为基准，绝对路径原样使用。加载拒绝未知字段、重复键、多文档、缺失监听路由和非法地址；listen 要匹配角色。账号哈希文件为 configs/accounts.local.yaml，机器人账号为 configs/clients.local.yaml；私密配置使用被忽略的 configs/*.local.yaml，数据库字段随对应节点方案加入。

Go 和 Node 标准库不提供 YAML 解析，分别固定 [go.yaml.in/yaml/v3](https://pkg.go.dev/go.yaml.in/yaml/v3) v3.0.5 和 [yaml](https://eemeli.org/yaml/) 2.8.3，锁文件纳入版本管理；解析只在配置加载边界进行。

真实进程测试显式运行：

```sh
./scripts/dev.sh test
./scripts/dev.sh test --race
```

测试预留临时本机端口、创建临时测试账号并等待 readiness。通信用例验证五节点探测和故障；业务用例只启动 access/login/online/mole 四节点，运行上述完整多人状态切片。--race 模式同时检查所有子进程和测试。dev.sh check 不启动进程或数据库。

当前验证记录（2026-10-13）：使用发布的 GSR v1.0.0，协议重新生成无差异；各 module test/vet、TS 类型检查与构建通过；YAML 共享文件、相对路径、错误配置和五份示例配置的用例通过；dev.sh test --race 的五节点探测与多人用例通过，合计约 26 秒。此前验证过 run-local.sh 的启动及 Ctrl-C 清理。MySQL 保存、积分到账与结算恢复尚未验收。

## 工程命令

以下均从根目录执行，Go 脚本复用 .cache/go-build，显式 GOCACHE 可覆盖，不清理测试缓存。

| 命令 | 用途 |
| --- | --- |
| ./scripts/dev.sh build [--race] | 构建节点二进制与 TS；race 版放 bin/race |
| ./scripts/dev.sh check | 各 module 测试/vet 和 TS 类型检查 |
| ./scripts/dev.sh test [--race] | 构建并执行真实多进程测试，只回收测试自己的进程 |
| ./scripts/generate.sh | 生成 Go/TS/registry，并整理生成文件；npm --prefix typeclient run generate 等价 |
| ./scripts/run-local.sh | 前台启动五节点，等待 readiness，回收本次启动的进程 |
| npm --prefix typeclient run check / build / client / mole | 独立客户端的类型检查、构建、探测与多人演示 |

## 业务完成标准

先实现多人分别用户名密码登录 → 分配同组及座位 → 满员开局 → 地鼠出现/命中 → 分数更新 → 局结束 → 持久化待结算 → Settlement 事务记账 → 查询积分到账，再验证故障恢复。打地鼠简化玩法，不完整复刻旧游戏；结算不可省略，局结束不能等同于积分到账。

首轮完成是 access、login、online、mole、settlement 五进程完整闭环。同组共享地鼠和局期限、分别记分；同一实例只奖励首个合法命中者。重复/过期命中不得加分，全组结果同事务结算且重复提交不得重复记账，保存过的待结算结果重启后必须补齐。access-02 和三人斗地主分别先确认方案再加入。

用户始终只连接 access，用户状态仅未登录/大厅/房间，Room 阶段仅等待/进行/结束。Agent/Online 用一条待确认操作记录处理绑定与进出房，结算确认放在结果记录中。协议规划保持一个入口，按业务开始实现时再拆分 .proto。

当前内存容量有界：最多 512 个 Online 用户、每用户 128 条位置结果、64 个房间、目录 1024 条分配记录、每房间 512 条控制记录和 512 条命中记录。位置记录不淘汰，达到上限返回 capacity；未保存结果保留在 ended 房间，不清空后另开局。MySQL 切片完成后再按持久化确认和保留期释放。密码使用固定 golang.org/x/crypto v0.26.0 的 Argon2id（标准库未提供），单认证 Service 串行哈希，并限制输入和速率。
