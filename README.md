# GSR Demo

基于 **GSR v1.0.2** 的多节点打地鼠示例，使用 TypeScript 命令行客户端演示登录、多人分组、命中反馈和银子结算。

调用链：客户端 → access → login / online / mole → settlement → MySQL。Redis 保存 mole 的待结算批次和统计检查点。

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
| `configs/mole-01.local.yaml` | `redis_url` 默认 `redis://127.0.0.1:6379/0`；`redis_key` 默认 `gsrdemo:mole:accounting` |

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

### 4. 初始化地鼠检查点

MySQL、Redis 可用后，在 mole 尚未启动时执行：

```shell
./bin/mole -config configs/mole-01.local.yaml -init-accounting
```

该命令为新演示创建零统计检查点，不重置已有统计。以后重启沿用同一 Redis 数据和 key，无需重复初始化。

### 5. 按顺序启动服务节点

MySQL、Redis 先保持运行。**每个节点各用一个终端**，按下表顺序启动；确认当前节点没有启动报错，再启动下一个。

| 顺序 | 节点与职责 | 启动命令 | 默认地址 |
| --- | --- | --- | --- |
| 1 | login：验证账号密码 | `./scripts/dev.sh run login` | Cluster `127.0.0.1:19002` |
| 2 | online：保存在线位置 | `./scripts/dev.sh run online` | Cluster `127.0.0.1:19003` |
| 3 | settlement：余额校验和持久记账 | `./scripts/dev.sh run settlement` | Cluster `127.0.0.1:19005` |
| 4 | mole：分组、目标和命中计算 | `./scripts/dev.sh run mole` | Cluster `127.0.0.1:19004` |
| 5 | access：客户端接入和业务路由 | `./scripts/dev.sh run access` | Cluster `127.0.0.1:19001`；WS `127.0.0.1:7888` |

`run` 前台运行节点，优先读取 `configs/<节点>-01.local.yaml`，没有时读取对应 `.yaml`。mole 监听启动后会恢复 Redis 检查点，恢复成功才接纳玩家；全部服务正常运行后再启动客户端。日志在 `logs/<实例名>/`。

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

输出顺序为 `login → enter → action → hit → settlement`，完成指定锤数后输出 `leave` 并退出。`hit` 是即时内存结果，`settlement` 才是最终入账；期间还会收到 `snapshot` 和有差异时的 `balance_correction` 推送。

同组玩家共享目标，同一目标只有一个玩家能命中。目标被抢走或过期时，CLI 输出 `miss`，刷新快照后继续寻找目标；`--hits` 只统计成功命中。每次等待/争抢预算默认 15 秒（`--timeout-ms` 更大时按该值，各次 RPC 另有超时）。超时、断线或其他错误仍停止，不自动重发结果未知的命中；保留输出中的 ActionID / HitID 供复查。

### 7. 停止演示

先让客户端完成并看到 `settlement`，再在各服务终端按 Ctrl-C，顺序为 **access → mole → settlement → online → login**，最后按需停止 Redis 和 MySQL。保留 Redis 持久化数据供下次恢复；尚未保存到 Redis 的内存命中不保证崩溃恢复。

## 开发与验证

| 操作 | 命令 |
| --- | --- |
| 全部 Go test/vet 与 TS 编译/单测 | `./scripts/dev.sh check` |
| 仅检查或构建一个节点 | `./scripts/dev.sh check access` / `./scripts/dev.sh build access` |
| 重新生成 Go / TS 业务协议 | `./scripts/dev.sh generate access` |
| TS 客户端单测 | `npm --prefix typeclient test` |

协议定义在 [game.proto](nodes/access/proto/demo/v1/game.proto)，生成产物入库。需要编辑器 workspace 时执行 `cp go.work.example go.work`，编辑器的 Go SDK 同样须为 1.24+。

五个独立节点、单/三客户端、真实 MySQL 和临时 Redis 的自动闭环验收（先完成依赖安装、建表、本机配置和构建）：

```shell
GOWORK=off GSRDEMO_ACCESS_PROCESSES=1 go -C nodes/access test \
  -run '^TestAccess(ClientProcessesLiveMySQL|ThreeClientsLiveMySQL|CLITargetRefusalAndUnknownResult)$' -v -timeout=1m
```

用例创建并清理自己的随机测试账号和钱包，不使用 `demo1～3`；只启动和回收自己的节点与 Redis 进程。

## 源码与文档

每个节点是 `nodes/<角色>` 下的独立 Go module；共享配置在 `common/model`，结算路由在 `common/settlement`，客户端在 `typeclient`。构建依赖发布的 GSR v1.0.2。

- 节点间 Call：见 [Agent 登录](nodes/access/agent_login.go)、[Mole 查询 Online](nodes/mole/moleentryservice.go)、[Mole 调用结算](nodes/mole/molesubmissionservice.go)。
- 节点间 Send 与客户端推送：见 [AttackService](nodes/mole/attackservice.go) 和 [Agent 推送接收](nodes/access/agent_mole.go)。
- [业务协议](docs/PROTOCOL.md)、[设计与恢复边界](docs/DESIGN.md)、[Go 代码规范](docs/GO-CODE-STYLE.md)、[工作规则](AGENTS.md)。
- 容量测试另见 [Online](docs/DESIGN.md#6-online-容量估算与待测计划)、[结算负载](docs/DESIGN.md#95-结算层负载基线)、[大数据历史压测](docs/DESIGN.md#97-大数据历史压测)，不属于运行 Demo 的必需步骤。
