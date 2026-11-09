# GSR Demo

基于 **GSR v1.0.3** 的多节点打地鼠与三人斗地主示例，使用 TypeScript 命令行客户端演示登录、地鼠命中反馈、斗地主叫抢出牌和银子结算。

调用链：客户端 → access → login / online / mole 或 landlord → settlement → MySQL。Redis 保存临时认证 Token、地鼠批次/统计检查点及斗地主待结算局末结果。

## 运行 Demo

打开系统终端（macOS Terminal、iTerm 或编辑器集成终端均可），使用 **zsh**，先用 `cd` 进入 **gsrdemo 根目录**。下面标为 **Shell** 的命令都在该终端执行，包括 `go`、`node`、`npm` 命令；标为 **SQL** 的语句在登录 MySQL 后的 `mysql>` 控制台执行。

首次完成第 1～4 步；以后确认 MySQL、Redis 已运行，再从第 5 步启动。

### 1. 准备环境

需要 Go 1.24+、Node.js 22.15+、npm、MySQL 8.0+ 和 Redis。先启动本机 MySQL，监听 `127.0.0.1:3306`。

在一个单独终端启动演示用 Redis；已有符合配置的 Redis 时跳过：

```shell
mkdir -p .cache/redis
LC_ALL=C redis-server --bind 127.0.0.1 --port 6379 \
  --dir "$PWD/.cache/redis" --appendonly yes --appendfsync always \
  --maxmemory-policy noeviction
```

保持该终端运行。另开终端安装客户端依赖并构建全部节点：

```shell
npm --prefix typeclient ci
./scripts/dev.sh build
```

构建产物为 `bin/<节点名>` 和 `typeclient/dist`；普通构建无需重新生成协议。

### 2. 初始化数据库与本机配置

建表脚本使用业务账号 `gsr_demo@127.0.0.1`。该账号不存在时，先执行 `mysql -u root -p` 登录 MySQL 创建，并自行设置密码：

```sql
CREATE USER IF NOT EXISTS 'gsr_demo'@'127.0.0.1' IDENTIFIED BY '替换为本机密码';
```

执行 `exit` 退出 MySQL，再在终端执行建库建表脚本，脚本同时授予业务账号所需权限：

```shell
mysql -u root -p < nodes/login/schema.sql
mysql -u root -p < nodes/settlement/schema.sql
cp -n configs/login-01.yaml configs/login-01.local.yaml
cp -n configs/settlement-01.yaml configs/settlement-01.local.yaml
cp -n configs/mole-01.yaml configs/mole-01.local.yaml
chmod 600 configs/login-01.local.yaml configs/settlement-01.local.yaml configs/mole-01.local.yaml
```

编辑这三份 `.local.yaml`（已有文件不会被复制命令覆盖）：

| 文件 | 需要填写或核对的字段 |
| --- | --- |
| `configs/login-01.local.yaml` | `mysql_dsn: "gsr_demo:本机密码@tcp(127.0.0.1:3306)/gsrdemo_login"` |
| `configs/settlement-01.local.yaml` | `mysql_dsn: "gsr_demo:本机密码@tcp(127.0.0.1:3306)/gsrdemo_settlement"` |
| `configs/mole-01.local.yaml` | `redis_url` 默认 `redis://127.0.0.1:6379/0`；`redis_key` 默认 `gsrdemo:mole:accounting:v2` |

`.local.yaml` 不提交到 Git。节点地址统一在 [configs/nodes.yaml](configs/nodes.yaml)，相对文件路径以配置文件所在目录为基准。

### 3. 创建三个演示账号并初始化银子

**执行位置：终端中的 zsh，当前目录为 gsrdemo 根目录。** 完成前两步后，将下面整块命令保留换行粘贴到终端执行；看到密码提示后输入密码并按回车，输入内容不会显示。

这段 Shell 会依次调用已经构建的 `bin/login` 和 `bin/settlement`，创建 `demo1`、`demo2`、`demo3`，设置同一个演示密码，并使用返回的真实 UserID 初始化各自的钱包。

```shell
# Shell（zsh），在 gsrdemo 根目录的终端执行
read -rs 'demo_password?设置三个账号的演示密码: '
for demo_user in demo1 demo2 demo3; do
  account_result=$(printf '%s\n' "$demo_password" | ./bin/login \
    -config configs/login-01.local.yaml -create-user "$demo_user") || break
  demo_user_id=${account_result#UserID=}
  printf '%s: UserID=%s\n' "$demo_user" "$demo_user_id"
  ./bin/settlement -config configs/settlement-01.local.yaml \
    -init-wallet "$demo_user_id" -silver 10000 || break
done
unset demo_password
```

此步骤只做一次；账号已存在会报错，不会覆盖密码。已有账号可用其 UserID 单独执行 `-init-wallet`：相同初始金额重复执行不会重复发放，改金额会拒绝。服务启动不自动注册账号或创建钱包。

### 4. 初始化地鼠检查点（仅地鼠需要）

MySQL、Redis 可用后，在 mole 尚未启动时执行。已有本地配置请将 `redis_key` 更新为 `gsrdemo:mole:accounting:v2`；本版使用单提交链路，不迁移旧多通道 Redis 演示数据。

```shell
./bin/mole -config configs/mole-01.local.yaml -init-accounting
```

该命令创建版本 2 的零统计检查点，重复执行不重置同一 key 中已有的新统计。以后重启沿用同一 Redis 数据和 key，无需重复初始化。斗地主跳过此步骤，使用 `configs/landlord-01.yaml` 中的独立 `redis_key`，不需要新增 SQL 表。

### 5. 按顺序启动服务节点

MySQL、Redis 先保持运行。**每个节点各用一个终端**，按下表顺序启动；确认当前节点没有启动报错，再启动下一个。

| 顺序 | 节点与职责 | 启动命令 | 默认地址 |
| --- | --- | --- | --- |
| 1 | login：密码认证 / Token 签发与校验 | `./scripts/dev.sh run login` | Cluster `127.0.0.1:19002` |
| 2 | online：保存在线位置 | `./scripts/dev.sh run online` | Cluster `127.0.0.1:19003` |
| 3 | settlement：余额校验和持久记账 | `./scripts/dev.sh run settlement` | Cluster `127.0.0.1:19005` |
| 4（地鼠） | mole：分组、目标和命中计算 | `./scripts/dev.sh run mole` | Cluster `127.0.0.1:19004` |
| 4（斗地主） | landlord：随机凑桌、叫抢与出牌 | `./scripts/dev.sh run landlord` | Cluster `127.0.0.1:19007` |
| 5 | access-01：客户端接入和业务路由 | `./scripts/dev.sh run access` | Cluster `127.0.0.1:19001`；WS `127.0.0.1:7888` |
| 6（可选） | access-02：第二个接入实例 | `./bin/access -config configs/access-02.yaml` | Cluster `127.0.0.1:19006`；WS `127.0.0.1:7889` |

演示地鼠启动 mole，演示斗地主启动 landlord；同时演示两种游戏时均启动。

每个 settlement 节点提供 `settlement-write` Call 服务池、`settlement-read` 查询和当前游戏使用的 `settlement-submit` 异步入口。通过 `worker_count/mysql_connections/mailbox_capacity` 调整容量；当前两条写入路径各有默认 8 个执行者，共享 10 个 MySQL 连接，各邮箱容量 64。地鼠仍 100ms/阈值归批、单个在途批次，斗地主每局一笔多人事务；游戏暂用 Send 提交，Command 20 回执直接返回原游戏 Service，结果未知按原 ID 限频重试。调用边界见 [Settlement 设计](docs/DESIGN.md#7-settlement并行执行与权威账务)，调用方迁移列于[后续工作](docs/DESIGN.md#10-后续工作)。`settlement_node` 默认 `settlement`，其他实例在 `nodes.yaml` 配置并显式选择，共用权威 MySQL，不自动切换。

需要复测本机并行与批次取舍时运行 `./scripts/dev.sh load settlement`；三轮 64 来源确认运行 `GSRDEMO_SETTLEMENT_ASYNC_CONFIRM=1 ./scripts/dev.sh load settlement`。测试使用真实本机专用 MySQL、双向 TCP，只清理自建样本，逐档核对余额/版本/唯一账务；结果保存在 `.cache/settlement-async-*.json`。当前默认依据短程对比选择，不代表生产在线人数上限。

`run` 前台运行节点，优先读取 `configs/<节点>-01.local.yaml`，没有时读取对应 `.yaml`。mole 监听启动后会恢复 Redis 检查点，恢复成功才接纳玩家；全部服务正常运行后再启动客户端。日志在 `logs/<实例名>/`。

两个 access 共用同一个二进制，第二个实例另开终端执行表中命令。所有节点的地址表仍只维护 `configs/nodes.yaml`；各实例的配置文件、`instance` 和 `listen` 统一使用 `access-01`、`access-02`、`access-03` 等编号名称，不保留 `access` 路由别名。每个实例使用独立的 Cluster、WS 端口和日志目录；修改地址表后，重启所有节点以加载新路由。

### 6. 启动多个客户端

另开三个客户端终端，分别运行以下命令。每次输入第 3 步设置的密码；**不要让多个客户端使用同一个账号**，新登录会替换旧连接。

终端 A：

```shell
read -rs 'demo_password?demo1 密码: '
printf '%s\n' "$demo_password" | node typeclient/dist/index.js --username demo1 --hits 10
unset demo_password
```

终端 B：

```shell
read -rs 'demo_password?demo2 密码: '
printf '%s\n' "$demo_password" | node typeclient/dist/index.js --username demo2 --hits 10
unset demo_password
```

终端 C：

```shell
read -rs 'demo_password?demo3 密码: '
printf '%s\n' "$demo_password" | node typeclient/dist/index.js --username demo3 --hits 10
unset demo_password
```

客户端默认连接 `ws://127.0.0.1:7888/ws`，可用 `--url` 修改；`--hits` 为 1～100。每组最多 3 人、9 洞，每秒产生一个目标、有效 2 秒。有效命中每锤扣 100 银子，返 0 或 198，各 50%。

已启动第二接入时，可在终端 B 运行以下命令，代替上面的终端 B 命令，让三个账号跨接入进入同一个游戏组：

```shell
read -rs 'demo_password?demo2 密码: '
printf '%s\n' "$demo_password" | node typeclient/dist/index.js \
  --url ws://127.0.0.1:7889/ws --username demo2 --hits 10
unset demo_password
```

同账号可以从另一接入重新登录，服务端会替换旧连接；随后重新 Enter 获取快照和余额。CLI 不自动重连，跨接入替换、断线后重新登录及原 HitID 唯一入账由下方自动验收覆盖。

输出顺序为 `authenticated → login → enter → action → hit`，完成指定锤数后输出 `leave` 并退出。`hit` 是即时内存结果；CLI 不逐锤查询或等待财务入账，不再输出地鼠 `settlement` 事件。结算回执由服务端处理，有差异时主动推送 `balance_correction`，期间仍会收到 `snapshot`。CLI 退出不代表待确认操作已持久化。

密码认证仅获取平台 AccessToken；随后 CLI 使用 Token 登录并绑定 Online。Token 默认有效期 24 小时，Redis 保存摘要和到期时间；普通断线不撤销，校验不续期。Token/密码认证失败会返回错误并关闭当前连接。Login 配置支持 redis_url、redis_key、token_ttl_seconds；既有私密配置缺省使用本机 6379、gsrdemo:auth:token 和 86400 秒。

需要复用 Token 时，首次密码运行可在 node 命令末尾加 `--save-token-file .cache/demo1-token.json`。文件以 0600 权限新建，不覆盖已有文件，日志不输出 Token。以后无需 stdin 密码：

```shell
node typeclient/dist/index.js --token-file .cache/demo1-token.json --hits 3
```

Token 过期或 Redis 记录丢失后重新密码认证；不要提交凭据文件。认证/Token 变化需要重建并重启 login、access 和 TS 客户端，旧版密码 Login 协议已停用。普通客户端仍只连 access；生产接入使用 WSS，MAC 本身不加密 Token。

同组玩家共享目标，同一目标只有一个玩家能命中。目标被抢走或过期时，CLI 输出 `miss`，刷新快照后继续寻找目标；`--hits` 只统计成功命中。每次等待/争抢预算默认 15 秒（`--timeout-ms` 更大时按该值，各次 RPC 另有超时）。超时、断线或其他错误仍停止，不自动重发结果未知的命中；保留输出中的 ActionID / HitID 供受控验收或管理核查；客户端不提供逐锤账务查询。

斗地主同样使用三个不同账号，在终端 A/B/C 分别将下面命令中的 `demo1` 改成 `demo1`、`demo2`、`demo3`。先启动 landlord 节点，再启动三个客户端：

```shell
read -rs 'demo_password?账号密码: '
printf '%s\n' "$demo_password" | node typeclient/dist/index.js --game landlord --username demo1
unset demo_password
```

默认仍连接 access-01；需要跨接入时，在终端 B 的 node 命令后添加 `--url ws://127.0.0.1:7889/ws`。客户端自动随机入桌、准备、叫抢地主、出牌/不出，等待整局结算后离开。叫抢各等待 10 秒，出牌 15 秒；三人都不叫最多重发两次。演示机器人使用简单单张策略，服务端仍校验全部普通牌型。

斗地主底分 10，每次抢地主、炸弹/王炸各翻倍。输出包含 `authenticated → login → landlord_enter → landlord_bid → landlord_play → landlord_settlement → landlord_leave`；`landlord_settlement` 携带本人的扣返、最终余额、版本和共同 OperationID，客户端直接使用该回执，不再追加查询账务。局末不先广播 pending；每桌先保存待结算请求到 Redis，再异步 Send 给结算服，收到有效回执后向三人各推送一次最终结果，随后删除待记录，清理失败单独重试。Redis 只保留待处理请求，不积累成功流水；结算服恢复或地主节点重启后，已保存请求按原 ID 补交，避免重复入账。不恢复进行中的手牌。

### 7. 停止演示

地鼠 CLI 的 `leave` 只表示玩法退出；停服前由受控验收或管理员集中确认本次操作已入账，斗地主等待 `landlord_settlement`。然后在各服务终端按 Ctrl-C，顺序为 **两个 access → mole / landlord → settlement → online → login**，最后按需停止 Redis 和 MySQL。保留 Redis 持久化数据供下次恢复；尚未保存到 Redis 的内存命中或局末结果不保证崩溃恢复。

## 开发与验证

| 操作 | 命令 |
| --- | --- |
| 全部 Go test/vet 与 TS 编译/单测 | `./scripts/dev.sh check` |
| 仅检查或构建一个节点 | `./scripts/dev.sh check access` / `./scripts/dev.sh build access` |
| 重新生成 Go / TS 业务协议 | `./scripts/dev.sh generate access` |
| TS 客户端单测 | `npm --prefix typeclient test` |

测试按行为归属组织：纯规则用例就近放在被测文件对应的 `*_test.go`，`*_cluster_test.go` 验证节点调用契约，`*_live_test.go` / `*_process_test.go` 验证真实存储或进程协作，`*_load_test.go` 提供显式启用的容量样本。共享启动与后端替身放在 `*_fixture_test.go`。不同层只保留各自特有的失败边界，避免重复验证同一成功流程。

协议定义在 [game.proto](nodes/access/proto/demo/v1/game.proto)，生成产物入库。需要编辑器 workspace 时执行 `cp go.work.example go.work`，编辑器的 Go SDK 同样须为 1.24+。

单/双接入的独立节点进程、三客户端、真实 MySQL 和临时 Redis 的自动闭环验收（先完成依赖安装、建表、本机配置和构建；需 redis-server 和 redis-cli）：

```shell
GOWORK=off GSRDEMO_ACCESS_PROCESSES=1 go -C nodes/access test \
  -run '^TestAccess(ThreeClientsLiveMySQL|TwoNodesReconnectLiveMySQL|LandlordThreeClientsLiveMySQL|CLITargetRefusalAndUnknownResult|CLITokenFileAuthentication)$' -v -timeout=1m
```

用例创建并清理自己的随机测试账号和钱包，不使用 `demo1～3`；只启动和回收自己的节点与 Redis 进程。双接入用例还覆盖跨接入故障恢复、原 HitID 唯一入账、Redis 批次恢复、余额校正与旧客户端查账入口拒绝，以及 Online 中断隔离和重启后显式重登恢复。斗地主用例另验证三客户端叫抢出牌、终态后不重复查账、三人唯一结算、结算服中断重试及已保存结果的地主节点重启恢复。地鼠验收程序在客户端结束后按本次操作 ID 集中核对 SQL 账务与钱包，不经玩家协议轮询。测试覆盖入口与恢复边界见 [设计文档](docs/DESIGN.md#9-验证依据与设计取舍)。

## 源码与文档

每个节点是 `nodes/<角色>` 下的独立 Go module；共享配置在 `common/model`，客户端在 `typeclient`；节点间直接使用 GSR Call/Send。构建依赖发布的 GSR v1.0.3。

- 节点间 Call：见 [Agent 登录](nodes/access/agent_login.go)、[Mole 查询 Online](nodes/mole/moleentryservice.go)。
- 异步结算与推送：见 [结算入口及执行者](nodes/settlement/settlementsubmitservice.go)、[地鼠批次提交](nodes/mole/molesubmissionservice.go)、[斗地主局末结算](nodes/landlord/roundsettlement.go)和[地鼠接入组件](nodes/access/gameagent/mole.go)。
- [业务协议](docs/PROTOCOL.md)、[设计与恢复边界](docs/DESIGN.md)、[Go 代码规范](docs/GO-CODE-STYLE.md)、[工作规则](AGENTS.md)。
- [GSR 游戏业务教程（21 页 HTML）](docs/gsr-game-business.html)：在浏览器中离线打开，支持分步讲解、讲者备注、打印 PDF 和新游戏开发清单。
- 容量测试入口及适用边界见 [并行与容量取舍](docs/DESIGN.md#92-并行与容量取舍)，不属于运行 Demo 的必需步骤。
