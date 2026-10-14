# GSR Demo

这是一个多项目工作区，从干净骨架按节点逐步设计和实现。当前完成 access 的连接基础，其余节点和 TypeScript 保持骨架。

每个节点角色一个独立 Go module，依赖已发布的 `github.com/cnooljaw/gsr v1.0.1`；入口和源码默认放项目根目录。节点通过 `gsr.Run + MainService` 启动，组装使用 `self.Node()`，跨节点通信使用 `self.Cluster()`；尚未注册业务 Service 或业务命令。版本及资源归属见 [接入知识](docs/DESIGN.md#v101-接入知识)。

| 目录 | 当前内容 |
| --- | --- |
| nodes/access | Cluster、Gate 和每连接一个 Agent，相关源码与测试都在根目录 |
| nodes/login、online、mole、settlement | 仅保留 main.go、main_service.go 和 Go 依赖文件 |
| common/model | 共用的 YAML 宿主配置加载与校验 |
| typeclient | 独立 TypeScript 项目，src/index.ts 仅为入口占位 |
| configs | 一份节点地址表和各实例的宿主配置 |
| scripts | 一个 dev.sh，负责构建、检查和单节点启动 |

旧业务实现、协议定义、生成结果和演示测试已移除。common 根据实际调用提取共享内容，节点不导入彼此实现。业务与协议在当前节点方案确认后再加入。

access 只处理连接生命周期：客户端 → Gate → Agent → GSR 协议 Dispatcher。Gate 固定使用 `ProtocolMAC`：客户端先完成 33 字节 X25519 握手，上行发送 `seq | Envelope | MAC`，下行仍为 Envelope；未握手的 Plain 客户端会被关闭。本机使用 WS，TLS 配置独立于 MAC 模式。每个 Agent 只有 ClientSession 与协议 Dispatcher，业务 Handler 表为空；使用 GSR 通用 Envelope/ErrorResponse，没有本项目 .proto 或生成步骤。收到断线通知后 Agent 退出，停止时关闭当前连接；进程停机由 GSR 管理。

工作规则见 [AGENTS.md](AGENTS.md)，目标和逐节点计划见 [docs/DESIGN.md](docs/DESIGN.md)，Go 规范见 [docs/GO-CODE-STYLE.md](docs/GO-CODE-STYLE.md)。

## 准备与使用

需要 Go 1.24 或更新、Node.js 22.15 或更新及 npm。从根目录执行：

```sh
npm --prefix typeclient ci
./scripts/dev.sh check
./scripts/dev.sh build
./scripts/dev.sh run access
```

check 执行各 Go module 的 test/vet 和 TS 类型检查，其中 access 用例会打开临时本机端口；build 输出 bin/<节点名> 和 typeclient/dist。run 只前台启动指定节点，Ctrl-C/SIGTERM 由该节点有序退出。

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

这些地址用于本机开发。GSR Cluster 的路由地址是拨号地址，不能用它限制监听网卡；节点使用受信任开发网络。

../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 只用于阅读，构建使用发布模块，不依赖兄弟 GSR checkout。access 后续功能和下一节点仍先展示方案，确认后再实现。
