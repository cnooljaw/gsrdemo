# AGENTS.md

本文件是进入 gsrdemo 的工作入口，只记录工作规则与文档索引。稳定设计结论写入 docs/，不要只保留在聊天中。

## 项目与修改边界

- gsrdemo 是唯一开发主目录，是一个 Git 仓库管理的多项目工作区。每个节点角色一个独立 Go module；同角色的多个实例共用项目、分别配置。common 是共享 Go module，typeclient 是独立 TypeScript 项目。
- `../gsr`、`../../skynet_bt/nbgameserver` 和 `../GoServerActorFsm` 严格只读；gsr 同时是本地框架依赖，另两个目录提供业务知识参考。不修改源码、文档、配置、依赖或生成结果，不在其中执行会写入文件的生成、格式化、安装或验证命令。
- GSR 1.0 指本地 ../gsr 基线，尚未发布；节点 go.mod 通过 replace 引用只读 GSR。参考 revision 记录在 docs/REFERENCES.md；本地源码变化需核对并更新基线，不当作已经锁定的发布包。
- 游戏业务、协议、配置、生成结果和运行产物都写在 gsrdemo 中。
- 参考实现不构成兼容目标；采用哪些行为由本项目文档、用户要求及实际需求决定。

## 文档入口

- [README.md](README.md)：工程结构、准备与当前进度。
- [docs/DESIGN.md](docs/DESIGN.md)：业务设计与行为契约。
- [docs/USER-SESSION.md](docs/USER-SESSION.md)：用户状态机、位置、协议准入及状态同步。
- [docs/REVIEW-AND-IMPLEMENTATION.md](docs/REVIEW-AND-IMPLEMENTATION.md)：实施顺序及验收。
- [docs/GO-CODE-STYLE.md](docs/GO-CODE-STYLE.md)：Go 编码与评审规则。
- [docs/DEVELOPMENT-WORKFLOW.md](docs/DEVELOPMENT-WORKFLOW.md)：修改、验证、审查与提交。
- [docs/REFERENCES.md](docs/REFERENCES.md)：只读参考来源与差异。

代码、测试、文档不一致时先确认预期行为，同步更新对应契约，不静默选择一份。重要取舍可写 ADR，不把 AGENTS.md 扩成架构百科。

## 基本工作约定

- 先完成最小、可验证的业务纵向切片，不提前搭建没有调用者的通用框架。
- 只修改当前任务范围，不顺手重构；保留用户已有的无关改动。
- Go 代码遵守编码规范并经过 gofmt；自然语言注释尽量中文。每个 Test 函数前说明它保护的可观察行为。
- 优先标准库；引入第三方依赖时说明具体用途和标准库不足之处。
- 共享代码依据实际调用提取，节点不导入彼此的 internal 实现。
- 密钥、私密配置不提交。依赖及生成工具固定版本，锁文件纳入版本管理。

## 验证与交付

- 新行为先建立关键失败用例，再完成最小实现。补测试前先找现有相关用例，不重复验证 GSR 已有底层契约。
- 局部修改运行最小相关验证；切片收尾再检查受影响 package/module。跨 module 切片完成后执行统一验证脚本。
- 纯文档修改只检查 diff、格式和链接，不跑 Go 测试。涉及并发、状态或生命周期时对受影响包运行 race；不机械每轮跑全仓 race/vet。
- 保留测试缓存；仅排查 flaky/缓存问题时使用 -count=1。命令超时先定位原因，不盲目重试或不断增加 timeout。
- 构建可通过不代表业务完成。未运行多进程或 MySQL 时不能宣称真实通信、结算或恢复已验收。
- 汇报修改内容、实际验证、未完成事项；运行脚本只回收自己启动的进程。

## Git

- 默认在当前分支工作，不自动创建、切换、合并分支，不执行 git add/commit/push。
- 完成修改后保留工作树给用户审查；用户要求修正时继续修改并验证。
- 明确说“commit/提交”才授权本地提交；推送需要另行明确授权。提交信息尽量中文。
- 提交前检查 status、diff --check、暂存文件范围及 diff --cached --check；提交后报告 hash 和工作树状态。
- 不使用破坏性命令清理工作区；改动归属不明时先确认。
