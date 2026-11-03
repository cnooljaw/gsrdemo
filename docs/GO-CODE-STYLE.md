# GSR Demo Go 编码规范

本规范补充 Go 官方约定，目标是让代码简单、直白、统一、可读。它不追求代码越短越好，而追求熟悉业务但不是原作者的开发者能够快速理解行为、边界和失败方式。

来源：从只读 `../gsr/docs/GO-CODE-STYLE.md` 复制，参考 revision `b2a9f16ae78da0bade541eabbb39a9144d5e04d2`；保留编码规则，文档入口随本项目依赖版本更新。

## 1. 规则层级

规范分为两类：

- **强制规则**：可以在评审或工具中明确判断，新增和修改代码必须遵守。
- **评审启发**：用于发现复杂度，不以固定行数或层数机械裁决。

本项目业务架构以 [DESIGN.md](DESIGN.md) 为准；GSR 公开 API 查阅实际依赖 v1.0.2 的 `docs/API-DESIGN.md` 和源码，旁边只读仓库的指南作为补充。版本与阅读基线见设计文档末尾。本规范不得被用来绕过 Service 状态所有权、Mailbox 或生命周期边界。

## 2. 强制规则

### 2.1 格式与布局

- 所有 Go 代码必须经过 `gofmt`。
- 源文件按具体类型或职责命名，例如 `loginservice.go`、`agent.go`、`agentsession.go`、`account_mysql.go`，避免含义过宽的 `service.go`。
- 同一类型确需拆到多个文件时保留类型前缀，再加职责后缀，例如 `agent_protocol.go`、`agent_login.go`；这只是文件命名规则。具有独立职责的代码优先通过具名字段组合，不因文件带同一前缀就把全部方法挂在一个类型上。
- 具名函数和方法使用正常多行函数体，不把赋值、清理和返回压缩在一行。
- 两个函数或方法之间保留一个空行。
- 长签名、调用和复合字面量按语义换行；约 120 字符可作为软提示，不是硬门禁。
- 每行表达一个主要动作，不使用分号压缩控制流程。
- Dispatcher 注册表一行一个“命令/消息 ID → 处理方法”，不将多项挤在一行；键值使用 `gofmt` 对齐。生产代码与测试中的注册表遵循同一写法。

```go
func (service *PlayerService) Close() error {
	service.context = nil
	service.phase = phaseClosed
	return nil
}
```

### 2.2 命名

- 名称表达领域含义和所有权，避免 `data`、`obj`、`tmp`、`ret` 等空泛名称。
- 不同职责使用不同语义名称，不只依靠大小写或包名区分。例如 access 的 Agent 是连接 Service，内部游戏组件使用 Component，不能都叫 Agent。
- 缩写按 Go 习惯统一为 `ID`、`URL`、`HTTP`、`RPC`、`GSR`。
- 方法接收者必须命名；同一类型保持一致，不使用 `this` 或 `self`。
- 使用中的参数必须表达用途；为满足接口而未使用的参数显式命名为 `_`。
- `gsr.ServiceContext` 参数统一命名为 `self`，表示当前 Service 的身份与能力，不使用 `scoped` 或 `ctx`；普通 `context.Context` 仍命名为 `ctx`。未使用的 ServiceContext 参数保持 `_`。此规则针对上下文参数，不把业务 Service 的方法接收者改名为 self。
- 沿用项目稳定术语，不为相同概念创造近义词。
- 消息处理方法按入口命名：GSR 内部命令入口使用 `handle...`（如 `mole.handleSnapshotPush`、`handleSettleGroup`），客户端协议入口使用 `on...`（如 `onLogin`、`landlord.onCall`）。组件归属由类型或具名字段表达，不重复把游戏名塞入组件的每个方法名。普通业务方法、值对象方法和生命周期回调不套用这些前缀；GSR 要求的 `Handle`、`HandleClientMessage`、`OnStart`、`OnStop` 保留原名。

注册规则与示例见 [消息注册与分发](#212-消息注册与分发)。

### 2.3 控制流

- 错误、无效输入和终止分支优先提前返回，让主路径保持左对齐。
- 已经 `return`、`continue` 或 `break` 的分支后不写多余 `else`。
- 允许只完成一个动作的 `if` initializer：

```go
if err := config.Validate(); err != nil {
	return err
}
```

- 当初始化值还参与后续业务判断，或条件混合错误、nil 和业务状态时，拆成具名局部变量。

```go
player, err := repository.LoadPlayer(ctx, playerID)
if err != nil {
	return err
}
if player.Disabled {
	return ErrPlayerDisabled
}
```

### 2.4 Error

- 错误必须处理，不使用 panic 表达预期的业务失败或外部失败。
- 使用 `%w` 保留错误链；包装必须增加操作、关键身份或边界信息。
- 不在多层重复添加 `failed`、`service failed` 等没有定位价值的文字。
- `errors.Is`、`errors.As` 和稳定的 sentinel/type error 用于程序判断，不依赖错误字符串分支。
- 多阶段清理需要同时保留多个错误时使用明确的聚合策略，例如 `errors.Join`。
- 日志通常只在能够决定处置方式的边界记录；不要每层既记录又返回同一个错误。

```go
player, err := repository.LoadPlayer(ctx, playerID)
if err != nil {
	return fmt.Errorf("load player %d: %w", playerID, err)
}
```

### 2.5 Context

- `context.Context` 只承载 deadline、cancellation 和请求范围 metadata，不作为依赖容器。
- `context.Context` 通常作为第一个参数，不存入长期对象；只有明确生命周期 owner 可以持有用于关闭的根 context/cancel。
- `ServiceContext` 是 Runtime 授予当前 Service 的能力集合，不等同于 `context.Context`。
- 不在 Handler 外或非 Runtime 管理的 goroutine 中保存、复用 `ServiceContext` 或 `CommandContext`。
- 数据库、配置、日志器等长期依赖通过明确字段或窄接口注入。

### 2.6 Goroutine 与 Channel

- Service 和业务层禁止直接使用 `go`，异步状态推进使用 Command、Timer 或独立 Service。
- 不提供公共 `Runtime.Go`、`Fork` 或相似通用入口。
- 允许启动 goroutine 的基础设施必须有明确 owner，并负责取消、等待、并发上限、panic 和错误归属。
- 新增并发 owner 必须覆盖正常关闭、取消、超时和泄漏测试。
- Channel 只用于真实的 goroutine 通信、队列或事件循环，不作为默认解耦工具。
- 能用普通调用、mutex 或 atomic 清楚表达的同步，不强行改成 channel。

### 2.7 接口、Struct 与构造

- 接口由使用者定义，并保持为完成该用途所需的最小能力集合。
- 只有多实现、测试隔离、模块边界、插件化或明确依赖倒置需求时才引入接口。
- Struct 显式声明依赖，默认优先组合而非继承式 embedding；只有 embedding 不泄漏内部生命周期、方法集合和所有权时才作为局部实现细节使用。
- Service 内的组合组件按实际职责持有自身数据和依赖，不持有完整父 Service 指针来任意访问其他组件。组件仍由所属 Service 串行调用，不增加独立生命周期、Mailbox 或 goroutine；身份等共享事实只保留一个来源，不在各组件重复维护。读取当前共享状态时通过注入函数取得值快照，不将构造时的副本长期当作当前值；只注入实际需要的能力。
- 固定组件采用显式组装，不按每条命令创建组件，也不为固定接线引入动态插件或自动发现框架。注册清单与分发规则见 [消息注册与分发](#212-消息注册与分发)。
- Service 具体类型显式声明 `Handle` 消息入口；若使用 Dispatcher，应以具名字段持有并由 `Handle` 转发，不依靠匿名嵌入的方法提升满足接口。这是编码与评审规则，Go 接口本身不能强制检查。
- 明确承担框架、业务组件或基础设施接口职责的类型，在类型定义后紧跟编译期接口断言，例如 `var _ Service = (*echoService)(nil)`。同一类型实现多个重要接口时统一放在一个 `var` 块中；Agent 同时实现 `Service` 与 `ClientMessageHandler` 时，两者都显式声明。按实际方法集选择指针或值形式，不为断言修改 receiver，不集中到文件末尾或独立的 `interfaces.go`。Service、上下文、Gate 连接、游戏接入组件、Redis Hook 等明确实现的接口都应声明，测试替身同样遵守；普通值对象不为此人为增加接口。Go 仍通过方法集隐式实现接口，断言是本项目的编码要求，用于显式表达意图并由编译器校验。
- 配置项较多时默认使用 Config Struct；Functional Option 只在可选项多、组合频繁且确实更清楚时使用。
- 构造函数完成默认值填充和不变量建立；可能失败的构造返回 error。
- 框架内部组件之间必须成立的不变量由构造器保证，不暴露给业务调用方组装后再运行时校验。例如，协议 Dispatcher 内部的解码与写回必须共用同一 Registry，由 Dispatcher 构造器建立这种关联，而不是要求调用方重复传入并匹配。
- `MustXxx` 只用于静态、由程序员定义且失败代表编程错误的构造，名称必须明确 panic 语义。

### 2.8 配置

- 配置使用明确 Struct，并在进入业务代码前完成默认值填充和完整校验。
- 文件、环境变量和命令行只在配置加载边界读取。
- 业务模块只接收最终配置对象，不在运行路径中重复解析配置源。
- 稳定的业务规则和内部实现参数保留在代码中，不为了“灵活”过度配置化。
- 敏感信息不提交到仓库，也不写进普通配置文件。

### 2.9 注释和文档

- 注释解释契约、原因、并发保证和非显而易见的限制，不复述代码。
- 导出符号提供 Go doc；文档重点说明调用者需要知道的行为和失败语义。
- TODO 必须说明未完成事项；需要跨版本追踪时附任务编号或设计文档链接。
- 可执行示例使用带语言标识的代码块（如 shell、go、typescript、sql、yaml），写明执行目录、终端或数据库控制台及必要前置步骤；Demo 步骤先启动服务节点，再启动多个客户端。重复操作集中到已有入口或运行说明。

### 2.10 包文档文件

- `doc.go` 是集中放置 package comment 的惯例文件名，不是 Go 的特殊入口，也不会因为文件名自动执行逻辑。
- 包级说明放在 `doc.go`，让 `go doc`、IDE 和 pkgsite 能展示统一的包契约；不要在其中放业务初始化或隐藏副作用。

### 2.11 测试可读性

- 每个 `Test...` 函数前写一句注释，说明它保护的可观察行为；注释不只重复函数名。

### 2.12 消息注册与分发

- **不同业务消息使用独立 CommandID 注册，避免在通用 Handler 中用字符串子指令再次分发。** 在 `CommandHandlers` 中将每个命令直接绑定到具体 `handle...` 方法，不使用 `kind/type` 字符串重建第二套命令路由；普通玩法规则和数据处理内部的分支不受此限制。
- 内部命令处理方法用 `handle...`，客户端协议处理方法用 `on...`；每项注册独占一行，由 `gofmt` 对齐，明确展示“ID → 方法”。
- CommandID 在同一 Service 的命令表内唯一，各组件合并后也必须唯一；不同 Service 可以复用编号。客户端 MsgID 与内部 CommandID 属于不同编号空间。重复注册和占用框架保留命令在组装时拒绝，不允许按顺序覆盖。
- Service 名称、CommandID 等契约默认与所属实现就近定义；有真实共享调用需求时才提取共享契约，不为了少量常量新建泛化 common 层，节点也不能导入彼此实现。
- 通用协议与各游戏分别提供注册表；由连接入口从同一份显式组件清单汇总。新增游戏不再分别修改注册、进入互斥和断线清理名单；新增协议仍须维护实际协议定义、生成产物及客户端。

```go
// 各组件在自己的类型内列出注册表，连接入口统一合并。
func (mole *Mole) CommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		moleSnapshotPushCommand:          mole.handleSnapshotPush,
		moleBalanceCorrectionPushCommand: mole.handleBalanceCorrectionPush,
	}
}

func (mole *Mole) ProtocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_ENTER): mole.onEnter,
		uint32(demopb.MsgId_C2S_HIT):   mole.onHit,
	}
}
```

## 3. 评审启发

以下指标提示需要重新审视代码，但不单独构成拒绝理由：

- 匿名函数已经表达独立业务概念、需要单独测试，或跨越多个抽象层级；此时提取成具名函数。
- 函数同时处理策略、I/O、持久化、日志和状态修改；按不同变化原因拆分。
- 控制流出现连续深层嵌套；优先使用提前返回、提取决策或拆分阶段。
- 调用者需要了解目标模块的内部表、锁、dirty bit、session 或清理顺序；将意图操作收回目标模块。
- 一个慢调用、热点用户或执行故障会阻塞无关连接、房间或通道；检查等待发生在哪个 owner、容量是否有界以及清理是否继续，优先把错误和积压限制在局部。组件组合本身不提供独立故障隔离。
- 三五行相似业务代码不必急于抽象；已经稳定重复的机械流程可以封装。
- 封装 `StartAll`、`StopAll`、`CloseAll` 等流程时，必须保留执行顺序、失败阶段、对象身份、多错误和回滚语义。
- 一个抽象需要大量泛型参数、回调或 option 才能使用；先检查是否把几个不同概念错误合并了。

## 4. Runtime 与业务错误边界

Runtime 可以统一提供：

- panic recovery；
- Handler 和 RPC 错误转换边界；
- tracing/metrics 的基础挂接；
- deadline/cancellation 传播；
- 生命周期阶段和失败对象的诊断信息。

Runtime 不应隐式决定：

- 哪些业务操作可以 retry；
- retry 次数与退避；
- 幂等键和重复执行语义；
- 业务 timeout；
- 部分成功后的补偿或回滚。

这些策略由拥有操作语义的模块决定，Runtime 只提供实现机制和可观测边界。

## 5. 验证门禁

验证按变更范围和风险分层；不要求每个小编辑都逐层跑完。选择能覆盖本轮行为的最小检查，形成连贯切片后再升级验证。纯文档修改不运行 Go 测试。下列 Shell 示例在待验证的 Go module 目录执行，包路径、测试名等占位符先替换；跨 module 的统一检查在 gsrdemo 根目录执行 `./scripts/dev.sh check`。

### 5.1 L0：格式与快速反馈

Go 文件修改后运行 `gofmt`。如果有相关测试，直接用定向测试获得编译和行为反馈，不必再额外运行一次仅编译命令：

```shell
gofmt -w <changed-go-files>
go test ./path/to/package -run '^TestSpecificBehavior$'
```

仅在没有适合的测试或需要单独确认编译时，才使用 `go test ./path/to/package -run '^$'`。文档修改只检查 diff、格式和改动涉及的链接，不运行无关 Go 测试。

### 5.2 L1：相关行为

开发循环优先只运行本轮行为相关的测试，一般运行一次：

```shell
go test ./path/to/package -run '^TestSpecificBehavior$'
```

测试名替换为实际名称。只有排查 flaky、随机性或并发时序问题时，才重复运行或使用 `-count=1`。相关测试默认在 2 分钟内完成。

### 5.3 L2：受影响 package

一个局部功能切片完成、共享测试辅助代码变更或同包多个行为彼此相关时，运行受影响 package 全测；只有静态分析对本次生产代码变更有价值时，再运行该 package 的 `go vet`：

```shell
go test ./path/to/package -timeout=5m
go vet ./path/to/package
```

默认预算为 5 分钟。`go test -timeout` 限制的是各测试二进制的执行时间；总体命令仍需观察实际进度。单个独立测试或小改动不因惯例追加全包验证。

### 5.4 L3：跨 package 或阶段验收

跨 package 功能切片完成、重要集成节点或发布前，根据改动范围执行全仓测试和静态分析：

```shell
go test ./...
go vet ./...
```

全仓验证不是每轮小改动的门禁；对同一份未变化代码，已由更完整的验证覆盖后不重复执行。

### 5.5 L4：高风险验证

涉及并发、生命周期或共享状态时，优先在切片收尾对受影响 package 运行 race 测试。只有改动影响 Runtime、Scheduler、Mailbox、Cluster 等跨 package 共享并发边界，或处于重要集成/发布验收时，才运行全仓 race：

```shell
go test -race ./path/to/affected/package
```

压力、泄漏、跨进程和外部依赖测试可以独立为 slow/integration 测试集，由 CI、阶段收尾或明确需求触发；不得为了缩短日常验证而把普通正确性测试移出默认测试集。

### 5.6 耗时与重试规则

- 命令超过预算或长时间无输出时，先定位具体 package 和测试，不盲目等待。
- 同一命令超时后不得未经分析连续重试，也不得只通过增大 timeout 掩盖死锁或泄漏。
- 日常验证保留 Go cache；仅在排查缓存、随机性或 flaky test 时使用 `-count=1`。
- 不习惯性执行 `go clean -testcache`。
- CPU、内存和 I/O 互不争抢的检查可以并行；全仓测试与全仓 race 默认串行。
- 建立 package 正常耗时基线；明显变慢应作为潜在性能或资源泄漏回归处理。
- 如果预算内无法完成必要验证，交付时列出已通过、未完成、耗时位置和剩余风险。

工具通过不代表代码自动符合本规范；职责、命名、依赖方向和 API 阅读成本仍需要评审。
