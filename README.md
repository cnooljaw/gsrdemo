# GSR Demo

这是一个多项目工作区，当前已收回为工程骨架，后续按节点逐步设计和实现。

每个节点角色一个独立 Go module，依赖已发布的 `github.com/cnooljaw/gsr v1.0.0`；入口和源码默认放项目根目录。当前五个节点都只有 `main.go`、`main_service.go`、`go.mod` 和 `go.sum`：通过 `gsr.Run + MainService` 打开 Cluster 并等待退出，尚未注册业务 Service 或命令。

| 目录 | 当前内容 |
| --- | --- |
| nodes/access、login、online、mole、settlement | 各节点的启动骨架 |
| common/model | 共用的 YAML 宿主配置加载与校验 |
| typeclient | 独立 TypeScript 项目，src/index.ts 仅为入口占位 |
| configs | 一份节点地址表和各实例的宿主配置 |
| scripts | 一个 dev.sh，负责构建、检查和单节点启动 |

业务实现、协议定义、生成结果和旧演示测试已移除。common 根据实际调用提取共享内容，节点不导入彼此实现。业务与协议在当前节点方案确认后再加入。

工作规则见 [AGENTS.md](AGENTS.md)，目标和逐节点计划见 [docs/DESIGN.md](docs/DESIGN.md)，Go 规范见 [docs/GO-CODE-STYLE.md](docs/GO-CODE-STYLE.md)。

## 准备与使用

需要 Go 1.24 或更新、Node.js 22.15 或更新及 npm。从根目录执行：

```sh
npm --prefix typeclient ci
./scripts/dev.sh check
./scripts/dev.sh build
./scripts/dev.sh run access
```

check 执行各 Go module 的 test/vet 和 TS 类型检查；build 输出 bin/<节点名> 和 typeclient/dist。run 只前台启动指定节点，Ctrl-C/SIGTERM 由该节点有序退出。

按节点工作时可执行 `./scripts/dev.sh check access` 和 `./scripts/dev.sh build access`，仅处理 common 与 access。TypeScript 当前只做类型检查与编译，没有连接或业务功能。Go 命令使用 GOWORK=off 和 .cache/go-build，保留缓存；编辑器需要时执行 `cp go.work.example go.work`。

节点地址只维护 [configs/nodes.yaml](configs/nodes.yaml)。各实例通过 nodes_file 引用该表，配置中的文件路径以该配置文件所在目录为基准。例如：

```yaml
instance: access-01
listen: access
nodes_file: nodes.yaml
log_directory: ../logs/access-01
```

配置只包含实例、Cluster 路由和日志路径，拒绝未知字段、重复键、多文档和非法开发地址。Go 标准库没有 YAML 解析器，因此 common 固定依赖 go.yaml.in/yaml/v3 v3.0.5。私密配置使用被忽略的 configs/*.local.yaml。

这些地址用于本机开发。GSR Cluster 的路由地址是拨号地址，不能用它限制监听网卡；节点使用受信任开发网络。

../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 只用于阅读，构建使用发布模块，不依赖兄弟 GSR checkout。下一步先展示 access 的方案，确认后再实现。
