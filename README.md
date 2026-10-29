# GSR Demo

这是一个多项目工作区，按节点逐步设计和实现。当前完成 access 的连接基础、login 的内部密码认证、online 的内部位置目录和 mole 的节点内首版业务；settlement 已实现通用银子结算及 MySQL 持久化，TypeScript 保持骨架，客户端游戏仍未接入。

每个节点角色一个独立 Go module，依赖已发布的 `github.com/cnooljaw/gsr v1.0.2`；入口和源码默认放项目根目录。节点通过 `gsr.Run + MainService` 启动，组装使用 `self.Node()`，跨节点通信使用 `self.Cluster()`。版本及资源归属见 [接入知识](docs/DESIGN.md#v102-接入知识)。

| 目录 | 当前内容 |
| --- | --- |
| nodes/access | Cluster、Gate 和每连接一个 Agent，相关源码与测试都在根目录 |
| nodes/login | 密码认证、MySQL 账号表、账号创建管理命令及测试 |
| nodes/online | 内存位置目录、跨接入节点绑定替换、条件解绑及测试 |
| nodes/mole | 分组、持续地鼠、同步内存扣返、统计池、跨组批量提交及测试 |
| nodes/settlement | 单人、批量单人、多人原子结算，MySQL 幂等及明细归档 |
| common/model | YAML 宿主配置加载与校验 |
| typeclient | 独立 TypeScript 项目，src/index.ts 仅为入口占位 |
| configs | 一份节点地址表和各实例的宿主配置 |
| scripts | 一个 dev.sh，负责构建、检查和单节点启动 |

旧演示实现已移除，现有业务仅按确认的节点范围加入。common 根据实际调用提取共享内容，节点不导入彼此实现。内部契约见 [PROTOCOL.md](docs/PROTOCOL.md)。

access 只处理连接生命周期：客户端 → Gate → Agent → GSR 协议 Dispatcher。Gate 固定使用 `SecurityModeMAC`：客户端先完成 33 字节 X25519 握手，上行发送 `seq | Envelope | MAC`，下行仍为 Envelope；未握手的 Plain 客户端会被关闭。本机使用 WS，TLS 配置独立于 MAC 模式。每个 Agent 只有 ClientSession 与协议 Dispatcher，业务 Handler 表为空；使用 GSR 通用 Envelope/ErrorResponse，没有本项目 .proto 或生成步骤。收到断线通知后 Agent 退出，停止时关闭当前连接；进程停机由 GSR 管理。

工作规则见 [AGENTS.md](AGENTS.md)，目标和逐节点计划见 [docs/DESIGN.md](docs/DESIGN.md)，Go 规范见 [docs/GO-CODE-STYLE.md](docs/GO-CODE-STYLE.md)。

mole 包含 MoleService、每组一个 AttackService、唯一 MoleAccountingService 和 MoleSubmissionService。命中同步返回内存扣返结果，提交 Service 独立等待 settlement 批量入账；mole 不使用 SQL，游戏持续运行，无固定局长。节点内实现与边界见 [mole 方案](docs/DESIGN.md#8-mole-方案与首版实现)，独立结算见 [settlement 实现](docs/DESIGN.md#9-settlement-节点与结算实现)。

## 准备与使用

需要 Go 1.24 或更新、Node.js 22.15 或更新及 npm。从根目录执行：

```sh
npm --prefix typeclient ci
./scripts/dev.sh check
./scripts/dev.sh build
./scripts/dev.sh run access
```

check 执行各 Go module 的 test/vet 和 TS 类型检查，其中 access/login/online/mole/settlement 用例会打开临时本机端口；build 输出 bin/<节点名> 和 typeclient/dist。run 只前台启动指定节点，优先读取 configs/<节点>-01.local.yaml，Ctrl-C/SIGTERM 由该节点有序退出。

按节点工作时可执行 `./scripts/dev.sh check access` 和 `./scripts/dev.sh build access`，仅处理 common 与 access。TypeScript 当前只做类型检查与编译，没有连接或业务功能。Go 命令使用 GOWORK=off 和 .cache/go-build，保留缓存；编辑器需要时执行 `cp go.work.example go.work`。

若编辑器出现 `go.work requires go >= 1.24.0 (running go 1.23.3)`，先检查 `go version` 和 `go env GOROOT`。终端自动选择的新工具链可能与 gopls 实际执行的 `go` 不同；在本机 `.vscode/settings.json` 中将 `go.goroot` 设为 Go 1.24 或更新 SDK 的目录，并通过 `go.toolsEnvVars.PATH` 把该目录的 `bin` 放在 `${env:PATH}` 前，`GOTOOLCHAIN` 设为 `auto`。配置后执行 `Go: Restart Language Server`；仍沿用旧环境时执行 `Developer: Reload Window`。此文件含本机路径，已被 Git 忽略，不降低 go.work/go.mod 的版本要求。工具链选择规则见 [Go 官方文档](https://go.dev/doc/toolchain)。

节点地址只维护 [configs/nodes.yaml](configs/nodes.yaml)。各实例通过 nodes_file 引用该表，配置中的文件路径以该配置文件所在目录为基准。例如：

```yaml
instance: access-01
listen: access
nodes_file: nodes.yaml
log_directory: ../logs/access-01
gate:
  address: 127.0.0.1:7888
  max_connections: 128
  max_frame_bytes: 4096
  max_outbound_queue_bytes: 65536
```

宿主配置包含实例、Cluster 路由和日志路径，拒绝未知字段、重复键、多文档和非法开发地址。Gate 参数只在 access 的私有配置类型中声明，由 GSR 校验其资源边界；其他节点不接收这些字段。Go 标准库没有 YAML 解析器，因此 common 固定依赖 go.yaml.in/yaml/v3 v3.0.5。私密配置使用被忽略的 configs/*.local.yaml。

access 的 WebSocket 测试使用与 GSR 相同的 gorilla/websocket v1.5.3，因为 Go 标准库不提供 WebSocket 客户端；protobuf v1.36.1 只用来构造和检查框架已生成的通用测试帧，不生成业务协议。

## login 使用

login 只接受内部 Cluster 认证请求，access 和 TypeScript 尚未接入客户端登录。账号密码按确认使用无盐 SHA-256，仅用于示例。先由管理员初始化专用库，脚本为已有的本机 gsr_demo 用户授予账号表权限：

```sh
mysql -u root -p < nodes/login/schema.sql
./scripts/dev.sh build login
```

复制 configs/login-01.yaml 为被忽略的 configs/login-01.local.yaml，将 mysql_dsn 填为该用户在 gsrdemo_login 库的连接串，并将文件权限设为 0600。不在公开模板中填写真实密码。

管理命令从标准输入读取一行密码，创建成功输出 UserID，重复用户名拒绝且不覆盖旧账号。以下为 zsh 用法：

```sh
read -rs 'login_password?示例密码: '
printf '%s\n' "$login_password" | ./bin/login -config configs/login-01.local.yaml -create-user demo
unset login_password
./scripts/dev.sh run login
```

默认测试不连接 MySQL；显式启用专用本机数据库测试时执行：

```sh
cd nodes/login
GOWORK=off GSRDEMO_MYSQL_INTEGRATION=1 go test -run '^TestLoginLiveMySQL$' -timeout=30s
```

该用例检查库名和地址，创建独占测试账号，通过真实 TCP Cluster 认证并核对稳定 UserID，测试后删除自己创建的账号。schema.sql 不在应用启动或测试中自动执行。

## online 使用

online 提供内部 Bind/Lookup/Unbind，按 UserID 保存 LoginID、AccessNode、AgentName。位置保留接入节点名，允许不同 access 使用同名 Agent；旧 LoginID 的断线通知不能删除新连接。当前 access 尚未接入该目录，命令与异常语义见 [内部协议](docs/PROTOCOL.md#online-位置目录)。

```sh
./scripts/dev.sh check online
./scripts/dev.sh build online
./scripts/dev.sh run online
```

目录仅在内存中，重启后为空；异常退出的接入节点可能留下旧位置，后续新登录覆盖。Online 不处理客户端连接、心跳、游戏状态或持久化。

显式运行约两分钟的多调用节点容量样本（默认 check 跳过）：

```sh
cd nodes/online
GOWORK=off GSRDEMO_ONLINE_LOAD=1 go test -run '^TestOnlineClusterLoad$' -v -timeout=3m
```

测试包含预装记录与最终逐条清理，实际总耗时长于两分钟。目标与结果口径见 [设计文档](docs/DESIGN.md#6-online-容量估算与待测计划)。它不代表实际 access 多进程或 12,000/100,000 条客户端连接已验收。

这些地址用于本机开发。GSR Cluster 的路由地址是拨号地址，不能用它限制监听网卡；节点使用受信任开发网络。

## mole 使用

演示规则固定为每组最多 3 人、9 洞，每秒产生一个目标、有效 2 秒；一锤扣 100，返 0 或 198 各 50%。AttackService 本地 Call 记账 Service，立即返回扣返和内存余额供 UI 显示；独立 settlement 最终裁决并持久化，不一致时只校正余额。本轮未修改 access/TypeScript，尚无真实客户端闭环。

```sh
./scripts/dev.sh check mole
./scripts/dev.sh build mole
./scripts/dev.sh run mole
```

mole 只读取 configs/mole-01.yaml 的宿主配置，没有数据库连接、建表脚本或源端持久化。所有余额、幂等凭据和明细的 SQL 归 settlement。此前初始化的 gsrdemo_mole 库不再使用，代码不会自动删除该库。

mole 在结算汇总初始化和用户余额加载完成前拒绝计费进入。默认跨节点测试使用测试 Online、结算和 Agent Service；真实结算数据库和独立进程测试按下节显式开启。access/TypeScript 仍未接入游戏。

未获确认的命中和批次只存在内存中；mole 崩溃可能丢失尚未提交的操作。重启后重新加载 settlement 已保存的余额和汇总，并可查询原操作，不承诺补交已丢失的内存批次。客户端即时显示不代表持久化入账。

## settlement 使用

三种写入口分别是 SettleSingle、SettleBatch（独立单人项）、SettleGroup（多人整体）。写协议为 version=2：单人用 UserID + Items，多人用 Changes，均保留 Reason 分类，允许同用户多条明细并汇总校验；同一业务操作跨批次/入口仍按 OperationID 去重；详情见 [协议](docs/PROTOCOL.md#通用银子结算接口)。

管理员先执行建表脚本；复制 configs/settlement-01.yaml 为权限 0600 的 settlement-01.local.yaml，填写专用库的 mysql_dsn：

```sh
mysql -u root -p < nodes/settlement/schema.sql
./scripts/dev.sh check settlement
./scripts/dev.sh build settlement
```

钱包由用户中心决定创建和初始银子，Settlement 只执行；Demo 用独立管理命令模拟用户中心，普通结算不创建钱包。以下 UserID=1 为示例，替换为 login 创建账号时返回的真实 UserID；初始化银子也保留明细，同用户同金额重试不重复发放，改金额拒绝：

```sh
./bin/settlement -config configs/settlement-01.local.yaml -init-wallet 1 -silver 10000
./scripts/dev.sh run settlement
```

节点启动不迁移数据库、不自动创建钱包。MySQL 失败不报告入账成功；调用超时按原身份重试。默认测试使用 go-sqlmock v1.5.2 注入 SQL 失败（标准库不提供对应模拟驱动），不访问外部数据库。

显式真实 MySQL 验证：

```sh
cd nodes/settlement
GOWORK=off GSRDEMO_SETTLEMENT_MYSQL=1 go test -run '^TestSettlementLiveMySQL$' -v -timeout=1m
```

独立 online/mole/settlement 进程加真实 MySQL 的闭环测试，先从根目录构建三个节点，再运行：

```sh
./scripts/dev.sh build online
./scripts/dev.sh build mole
./scripts/dev.sh build settlement
cd nodes/settlement
GOWORK=off GSRDEMO_SETTLEMENT_MYSQL=1 GSRDEMO_SETTLEMENT_PROCESSES=1 go test -run '^TestSettlementMoleProcessesLiveMySQL$' -v -timeout=1m
```

这些用例只接受专用本机 gsrdemo_settlement 库，创建随机独占测试钱包，并清理自己的操作、批次和余额；进程用例只回收自己启动的进程。access/TS 的真实客户端链路、故障负载和入账延迟目标仍需后续验收。

../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 只用于阅读，构建使用发布模块，不依赖兄弟 GSR checkout。access 后续功能和下一节点仍先展示方案，确认后再实现。

结算层负载测试（不执行地鼠目标判定或棋牌局内逻辑）：

```sh
cd nodes/settlement
GOWORK=off GSRDEMO_SETTLEMENT_MYSQL=1 GSRDEMO_SETTLEMENT_LOAD=1 go test -run '^TestSettlementLoad$' -v -timeout=5m
```

默认依次执行 A～D，每组 30 秒；可用 `GSRDEMO_SETTLEMENT_LOAD_DURATION=60s` 调整，较长测试同步调整 Go 的 timeout。A 为 600 钱包、3000 笔地鼠操作/秒，每 100ms 提交 300 笔；B 为 2500 桌的 7500 钱包、100 笔三人整体操作/秒；C 叠加；D 把约 20% 的两类操作集中到同一钱包。每笔保留 Reason 分类，结束后核对余额、版本、唯一成功操作及扣返总额，仅清理本次样本。详情见 [设计文档](docs/DESIGN.md#95-结算层负载基线)。结果写入被忽略的 `.cache/settlement-load.json`（项目根目录），不会提交原始运行数据。

5000 桌四人配置及独立进程 CPU 采样：

```sh
./scripts/dev.sh build settlement
cd nodes/settlement
GOWORK=off GSRDEMO_SETTLEMENT_MYSQL=1 GSRDEMO_SETTLEMENT_LOAD=1 \
  GSRDEMO_SETTLEMENT_LOAD_PROFILE=four GSRDEMO_SETTLEMENT_LOAD_DURATION=60s \
  go test -run '^TestSettlementLoad$' -v -timeout=10m
```

该配置依次测试 A（600 地鼠钱包/3000 hit/s）、B（5000 桌四人/200 桌结算/s）、C（两者合计 20,600 钱包）、D（C 加热点钱包）。独立启动并回收本次 settlement 子进程，MySQL 仅观察，不停止；每秒通过 `ps` 累计 CPU 时间之差计算进程负载，100% 表示一个逻辑核。需要本机仅有一个 mysqld 进程，CPU 采样仅在显式负载测试中启用。结果及逐秒样本写入 `.cache/settlement-load-four.json`。正式容量测试勿启用 race；race 用短程测试单独检查。

大数据历史压测使用独立库，数据会保留用于后续档位，不能指向日常示例库：

```sh
# 首次由管理员执行；不清空已有库。
mysql -u root -p < nodes/settlement/load_schema.sql
# 准备 configs/settlement-load.local.yaml：节点配置结构沿用 settlement，
# mysql_dsn 的数据库必须是 gsrdemo_settlement_load，文件权限 0600。
./scripts/dev.sh history settlement
```

参数集中在 `configs/settlement-history.yaml`：100 万钱包，按约 102 万/1000 万/5000 万/1 亿游戏操作分档；钱包初始化另产生 100 万操作及批次。默认每 3200 笔游戏操作生成 210 个批次，保留真实分类、请求摘要和完整回执。工具采用有界事务块，检查点与账务一起提交；再次运行从已提交进度继续，不清空或改写已造历史。造数 ID 按 1024 个前缀交错生成，金额为确定性的演示扣返，不用于验证随机数质量。

每档测试固定活跃钱包、每 25 秒轮换且分散到百万钱包空间的用户，再测试历史批次/操作查询、同批重试、跨批重提、内容冲突及全历史 Summary。读取基线会预热部分数据；首遍/重复访问不等同于严格冷启动。正常结束仅删除本次压力流量并恢复借用钱包；历史数据不会清理。测试被强杀时保留 `.cache/history-wallet-backup-*.json` 和现场，工具发现残留 `load-` 账务会拒绝继续造数，不能绕过检查点或直接删历史。报告为 `.cache/settlement-history-<游戏操作数>.json`，造数和压测可能持续数小时。

自然档位之后，工具会在同库追加每锤单批的真实新操作，将批次表推进到约 1000 万和 3000 万条，不改旧账；因此实际操作总数约为 1.034 亿和 1.234 亿，报告按真实数量记录。已完成档位不会伪装成较小规模重测。外部数据库进度不在 Go 缓存依赖中，`history` 入口显式使用 `-count=1`，普通测试继续保留缓存。
