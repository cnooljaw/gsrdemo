# 开发、审查与提交工作流

来源：提取只读 GSR 的 AGENTS.md 和 docs/DEVELOPMENT-WORKFLOW.md 中的基本工作习惯；本项目采用多 module 验证，不复制框架专属设计流程。

## 标准循环

修改 → 最小相关验证 → 汇报并保留工作树 → 用户审查 → 按反馈修正 → 用户明确要求提交后才创建本地提交。推送另行授权。

开始工作先检查 `git status --short`，不覆盖用户改动。默认不创建或切换分支，不自动暂存、提交或推送。

## 验证范围

| 变更 | 验证 |
| --- | --- |
| 文档 | diff --check、格式及链接 |
| 局部 Go 行为 | gofmt 和所在 module 的定向测试 |
| 连贯功能切片 | 受影响 package/module 全测；有需要时 vet |
| 并发或生命周期 | 受影响 package race 测试 |
| 跨 module 切片或阶段验收 | scripts/verify.sh；真实进程/数据库演练显式开启 |
| TypeScript | typeclient 内 npm run check；有业务测试后补对应测试 |

根目录没有 go.mod，`go test ./...` 不能代表全部节点。脚本只枚举 common、nodes 下的项目及将来 tests 下的测试 module，不验证只读 gsr 的源码。日常优先在受影响 module 运行最小测试，不每轮执行统一脚本。

相关测试默认控制在 2 分钟内，包级验证默认 5 分钟内；超时先定位慢点。保留缓存，不习惯性加 -count=1，不重复已覆盖的检查。未具备数据库或网络条件时列出未验证项，不假报成功。

## 提交门禁

用户授权本地提交后，检查 `git status --short`、`git diff --stat`、`git diff --check`，逐个暂存本次任务所属文件，再检查 `git diff --cached --name-only` 和 `git diff --cached --check`。归属不明先确认；不丢弃用户已有暂存内容。

提交信息尽量中文，描述可验证的变化。提交后汇报 hash、内容及工作树状态。未经明确授权不推送，不使用 reset --hard、checkout --、clean 等命令清理用户改动。
