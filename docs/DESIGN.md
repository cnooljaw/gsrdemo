# 目标与逐节点计划

## 1. 目标和当前边界

首个 demo 完成多人同组打地鼠的完整过程：用户名密码登录 → 分组与座位 → 满员开局 → 共享地鼠与个人计分 → 局末全组结果 → 持久化与积分结算 → 已保存结果的故障恢复。玩法保持最小实现，结算不能省略。用户始终只连接 access。

三人斗地主后续单独确认，优先参考 nbgameserver 的 gameofhlddz/；多人分组和共享目标可参考 GoServerActorFsm。参考代码提供知识，不作为兼容目标。

当前 access 已具备 Gate/Agent 连接基础，其余四个节点和 TypeScript 保持骨架。没有业务状态、业务命令、本项目客户端协议、生成结果、账号管理或游戏演示。旧实现和旧方案已移除，不能当作已确认设计继续扩展。

每个节点角色一个独立 Go module，源码默认放该项目根目录；common 目前只包含实际共用的配置代码。没有调用者的通用框架、协议目录和业务包不提前建立。

## 2. 推进顺序

| 顺序 | 节点 | 先审查并确认的范围 |
| --- | --- | --- |
| 1 | access | Gate/Agent 的职责和生命周期、连接拥有的数据、最少状态、客户端协议准入及 GSR 接入方式 |
| 2 | login | 最小认证能力、账号来源、输入输出、认证 Service 的职责与调用边界 |
| 3 | online | 是否需要独立节点，位置数据由谁拥有，哪些协调和状态确有必要 |
| 4 | mole | 多人分组、共享时间线、命中与计分、局末结果，以及与结算的接口边界 |
| 5 | settlement | 最小积分账本、持久化职责、重复与冲突处理、事务和故障恢复 |

每次只推进一个节点：

1. 只读核对现有骨架、实际依赖版本和 GSR 文档/示例。
2. 展示简短方案：职责、数据 owner、最少状态、必要协议与调用链、失败与生命周期边界、修改范围和验证方式。
3. 用户确认后实现当前节点。common、协议和 TypeScript 只配套加入该节点必需的内容；其他节点的依赖问题先列出，不自动扩展实现。
4. 展示改动和实际验证，等待用户决定是否提交及推进下一节点。

access 连接基础已实现，当前交用户审查，再决定后续范围。业务状态尽量少，行为相同的状态优先合并；不为调用、重试和存储步骤增加状态。协议有实际需求后集中维护，再按使用范围拆分 .proto。

完成五个节点后再联调打地鼠全流程。三人斗地主、access 多实例和第三方登录分别先确认方案，不提前开工。

## 3. access 第一步（已确认）

- MainService 打开 Cluster 和 Gate；每条连接由 Gate 创建一个 Agent。采用本机 Plain WebSocket，监听地址及连接数、帧大小、出站队列上限放在 access 的私有 YAML 配置中。
- Gate 拥有连接和 I/O；Agent 只持有 ClientSession 与具名协议 Dispatcher，不接触 socket 或连接表，不持有业务状态，也不增加状态机。
- 调用链为客户端 → Gate → Agent.HandleClientMessage → protocol.Dispatcher。只使用 GSR 通用目录和空业务 Handler 表，错误处理沿用框架规则；没有本项目的协议编号、.proto、生成工具或跨节点业务调用。
- Gate 的断线通知进入 Agent.Handle，Agent 调用 self.Exit 后立即返回；OnStop 调用幂等的 ClientSession.Close。MainService 完成组装后退出，Gate 和 Agent 继续由 Runtime 管理；进程停机由 gsr.Run 收尾。任一步启动失败都向宿主返回错误。
- 修改范围是 access 的入口、MainService、根目录 agent.go 和相关测试/配置；common 仅拆出已有的 YAML 读取和宿主配置解析，角色私有字段留在节点中。其他节点和 TypeScript 保持骨架。
- 验证配置边界、断线清理、真实 WebSocket 的错误路径与单连接故障隔离、停机关闭连接/监听，以及 Gate 启动失败后的 Cluster 释放；对 access 运行 race。

## 4. GSR 与参考边界

- 节点固定 GSR v1.0.0，tag commit 为 d5531a6ba413415f3ab65b8100f894750c44dcd4；节点仅对本仓库 common 使用本地 replace。
- 只读文档 revision 为 30a870f282f6fd2b6fc58b6e3d3e962a291a14b8，生产源码与该发布 tag 一致；公开 API 以实际依赖版本为准。
- 接入参考 [独立业务项目开发指南](../../gsr/docs/APPLICATION-DEVELOPMENT.md)；Cluster 调用优先看 [CLUSTER-CALL-TUTORIAL.md](../../gsr/docs/CLUSTER-CALL-TUTORIAL.md)，直接使用框架已有的参数与调用能力。
- Service、状态所有权、消息快照和资源关闭的工作约束见 [AGENTS.md](../AGENTS.md)，编码规则见 [GO-CODE-STYLE.md](GO-CODE-STYLE.md)。
- ../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 均只读，不在其中生成、安装、格式化或验证。
