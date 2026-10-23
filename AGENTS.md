# AGENTS.md

本文件只记录工作规则与文档入口；稳定设计结论写入现有设计文档。

## 项目与修改边界

- gsrdemo 是唯一开发主目录，一个 Git 仓库管理多个项目。每个节点角色一个独立 Go module，同角色多实例共用项目、分别配置；common 为共享 Go module，typeclient 为独立 TypeScript 项目。
- 节点源码、入口和测试默认放项目根目录，不预设 cmd/ 或 internal/；实际需要独立包时才建目录。
- 所有使用 GSR 的 go.mod 固定 github.com/cnooljaw/gsr v1.0.2，不提交指向本地 GSR 的 replace；节点仅对本仓库 common 使用本地 replace。
- ../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 严格只读，只提供框架文档和业务参考。不修改源码、文档、配置、依赖或生成结果，不在其中执行会写文件的生成、格式化、安装或验证；依赖缓存和 vendor 同样不手工修改。
- 游戏业务、配置、协议和运行产物都写在 gsrdemo；参考实现不是兼容目标。依赖与阅读基线见 docs/DESIGN.md。

## 文档入口

- [README.md](README.md)：结构、准备与工程命令。
- [docs/DESIGN.md](docs/DESIGN.md)：目标、逐节点计划与参考基线。
- [docs/GO-CODE-STYLE.md](docs/GO-CODE-STYLE.md)：Go 编码与评审规则。

代码、测试和文档不一致时先确认预期行为，同步更新契约，不静默选择一份。不按每次讨论新增文档，不创建空目录说明或重复 README。协议有实际需求后在 docs/PROTOCOL.md 集中维护，具体 .proto 用到时再拆分。

## 工作约定

- 从工程骨架按节点推进，只实现已确认范围；一次只审查或实现一个节点，不能依据旧实现或未确认方案直接扩展业务。
- 修改节点业务代码前先展示简短方案：职责、数据归属、最少状态、协议与调用链、GSR 文档/示例依据、失败与生命周期边界。用户确认后才实施；未确认时只做只读核对和方案修订。
- 一个节点完成后展示改动与实际验证，等待用户决定是否推进下一节点。common、协议和 TypeScript 只修改当前节点已确认方案必需的部分；其他节点的依赖问题先列出，不自动扩大实现范围。
- 优先简单、可验证的方案，不提前搭建没有调用者的通用框架；共享代码依据真实调用提取，节点不导入彼此实现。
- 业务状态尽量少，优先合并行为相同的状态；操作和结果记录只在必要时增加，不为 RPC、重试和存储步骤增加状态，也不以重复布尔值重建复杂状态机。
- 只修改当前任务范围，保留用户无关改动。Go 代码经过 gofmt，自然语言注释尽量中文，每个 Test 函数前说明它保护的可观察行为。
- 优先标准库；引入第三方依赖时说明具体用途和标准库不足之处。依赖和生成工具固定版本，锁文件纳入版本管理，密钥和私密配置不提交。
- 配置统一用 YAML；节点地址表只维护 configs/nodes.yaml，各实例通过 nodes_file 引用。相对文件路径以配置文件所在目录为基准。
- 脚本头部写明用途、参数和执行范围；重复操作合并到现有入口，不为少量操作新增独立脚本。

## GSR 接入约定

依据 v1.0.2 发布说明、开发指南与 ADR 0012/0013；版本基线和入口见 [DESIGN.md](docs/DESIGN.md#4-gsr-与参考边界)。

- 使用 gsr.Run + MainService；节点组装用 self.Node().RegisterService/OpenCluster/CreateGate/OpenHTTP，通信用 self.Cluster().Send/Call，不手工组装 Runtime/ClusterClient。
- Gate 用 SecurityModeMAC 指定传输认证，业务协议由 Agent 选择；当前沿用 Protobuf protocol 包与 Envelope。
- Service 显式实现 Handle，接口断言紧跟类型。有业务命令时具名持有 Dispatcher；没有命令的 MainService 直接返回 ErrUnknownCommand。客户端 Agent 另显式实现 HandleClientMessage。
- 每份可变状态只有一个 Service owner，跨 Service 通过消息访问；纯规则和 owner 私有值对象按需保留。
- self、Node 和 Handler 上下文仅在当前回调使用，不保存或借给异步任务，不在业务中启动 goroutine；引用参数和结果传独立快照。
- 直接使用 GSR Call/Send；Call 有限等待，远程 Handler 使用规范参数类型，不另做内部 Protobuf 编解码。Send 成功只表示接纳，超时、重复和迟到结果的幂等或补偿由业务负责。
- 配置用 gsr.RuntimeConfig/gsr.ClusterConfig；日志用只提供 Info/Error 的 ServiceLogger。gsr.Run 管理节点资源，Gate 是独立 Service；业务只关闭自有资源，self.Exit() 后立即返回，创建者退出不停止已创建的 Service。
- 生成工具与 GSR schema 同版本，不混用旁边 checkout；其余接入边界见 DESIGN.md。

## 验证与交付

- 新行为先建立关键失败用例，再完成最小实现；先找已有相关用例，不重复验证 GSR 底层契约。
- 局部修改运行最小相关验证，跨 module 收尾执行统一检查。并发、状态或生命周期变化对受影响包运行 race；不机械每轮全仓 race/vet。
- 纯文档只检查 diff、格式和链接。保留测试缓存，仅排查 flaky/缓存问题时用 -count=1；超时先定位原因，不盲目重试或增加 timeout。
- 构建通过不代表业务完成；未运行真实通信或 MySQL 时不宣称相应能力已验收。汇报实际改动、验证和未完成事项；运行只回收自己启动的进程。

## Git

- 默认在当前分支工作，不自动创建、切换、合并分支，不执行 git add/commit/push。
- 完成修改后保留工作树给用户审查；明确说“commit/提交”才授权本地提交，推送需另行明确授权。提交信息尽量中文。
- 提交前检查 status、diff --check、暂存范围和 diff --cached --check；提交后报告 hash 与工作树状态。
- 不使用破坏性命令清理工作区；改动归属不明时先确认。
