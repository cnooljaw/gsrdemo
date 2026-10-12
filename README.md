# GSR Demo

基于 GSR 的多节点游戏业务验证项目。gsrdemo 是唯一开发目录；../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 严格只读。

当前已实现切片 A 的通信接线：五个可执行节点、TS 探测 CLI、共同 Protobuf 协议、跨节点 Call 和 mole→Agent→客户端 Push。当前诊断 Service 不提供登录认证、Online 绑定、房间玩法、数据库或积分结算；通信验收不等于完整业务验收。

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

Go build 输出 bin/access、bin/login、bin/online、bin/mole、bin/settlement，TS build 输出 typeclient/dist。schema 和生成结果纳入 Git，修改 common/proto 后运行 scripts/generate.sh，Go/TS/registry 输出只在本项目；工具版本和消息清单集中在协议规划中。

## 本机通信演示

在根目录前台启动本轮五个进程：

```sh
./scripts/run-local.sh
```

另一个终端运行客户端：

```sh
npm --prefix typeclient run client -- --text "hello demo"
```

CLI 可指定 --url（默认 ws://127.0.0.1:7888/ws）和 --text（默认 hello demo）。它校验 request_id、operation_id/text/node，等待四节点响应与 mole 主动 Push 均到达才输出 complete。空文本返回 invalid，节点不可用返回 unavailable/failed_node，并以非零状态退出。每条探测总 Call 预算 2 秒，CLI 等待预算 5 秒并限制关闭握手时间。

Ctrl-C 停止 run-local.sh 本次启动的子进程，任一节点提前退出也会收回剩余进程；清理只针对自己的 PID。日志按实例在 logs/ 下隔离。也可从根目录单独执行 bin/<角色> -config configs/<角色>-01.json，成功输出 JSON ready，Ctrl-C/SIGTERM 有序退出。

配置是本地开发示例；Gate 为 Plain，监听 127.0.0.1:7888。Cluster 的 GSR Open 实际绑定 0.0.0.0，配置路由中的 127.0.0.1 是拨号地址，不代表其监听限制在回环；本轮用于受信任开发环境。

JSON 配置共用 nodes 路由表，各实例独占日志目录，路径按启动工作目录解释。加载拒绝未知字段、多个对象、缺失监听路由和非法地址，当前 listen 要匹配角色；access-02 在对应切片接入。私密配置用被忽略的 configs/*.local.json/*.local.yaml，数据库字段随持久化切片加入。

真实进程测试显式运行：

```sh
./scripts/process-test.sh
./scripts/process-test.sh --race
```

测试预留临时本机端口、等待 readiness，再用两个同时活跃的 TS 客户端验证响应/推送隔离；还验证空输入拒绝、login 停止后的明确错误和全部进程有序退出。--race 模式连同五个节点一起使用 race 构建。普通 verify.sh 不启动进程或数据库。

通信阶段验收记录（2026-10-12）：process-test.sh --race 已通过，真实用例约 7 秒；verify.sh 的全部模块测试、vet 和 TS 类型检查已通过；run-local.sh 的五节点启动及 Ctrl-C 清理已实际验证。认证、多人玩法、数据库与结算恢复尚未验收。

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
| npm --prefix typeclient run check / build / client | 独立客户端的类型检查、构建和运行 |

## 业务完成标准

先实现多人分别用户名密码登录 → 分配同组及座位 → 满员开局 → 地鼠出现/命中 → 分数更新 → 局结束 → 持久化待结算 → Settlement 事务记账 → 查询积分到账，再验证故障恢复。打地鼠简化玩法，不完整复刻旧游戏；结算不可省略，局结束不能等同于积分到账。

首轮完成是 access、login、online、mole、settlement 五进程完整闭环。同组共享地鼠和局期限、分别记分；同一实例只奖励首个合法命中者。重复/过期命中不得加分，全组结果同事务结算且重复提交不得重复记账，保存过的待结算结果重启后必须补齐。之后加入 access-02 和三人斗地主。

用户始终只连接 access，用户状态仅未登录/大厅/房间，Room 阶段仅等待/进行/结束。Agent/Online 用一条待确认操作记录处理绑定与进出房，结算确认放在结果记录中。协议规划保持一个入口，按业务开始实现时再拆分 .proto。
