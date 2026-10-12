# 只读参考来源

## GSR

依赖基线：只读本地 `../gsr` 的 1.0 完成版本，用户已明确尚未发布。当前参考 revision 为 `d5531a6ba413415f3ab65b8100f894750c44dcd4`，本轮确认其工作树干净。

五个节点通过自己的 go.mod 中 `replace github.com/cnooljaw/gsr => ../../../gsr` 使用本地源码；require 的 v0.0.0 仅为未发布模块的占位版本，不是框架里程碑版本。common 同样由本地 replace 引用，默认 workspace 只组装 gsrdemo 的 module；关闭 workspace 也可沿用本地替换。

本地 replace 不冻结源码内容，兄弟 GSR 仓库变化时应核对并更新基线；本 demo 不修改或切换 GSR 的工作树。正式发布后再移除本地替换、固定实际版本并验证可复现构建。初始化复制的编码规范和工具链来自旧 revision `b2a9f16ae78da0bade541eabbb39a9144d5e04d2`，保留此来源记录。

| 问题 | 参考文件 | 采用方式 |
| --- | --- | --- |
| Go 风格 | docs/GO-CODE-STYLE.md | 复制规则，仅调整标题与文档入口 |
| Git 忽略 | .gitignore | 原样作为基础，再追加 demo 专属规则 |
| 工作习惯 | AGENTS.md、docs/DEVELOPMENT-WORKFLOW.md | 提取最小切片、分层验证、用户审查及提交门禁 |
| 宿主启动与 Service | examples/counter/、examples/twonode/ | 参考 gsr.Run 与显式 MainService 组装 |
| Gate 与协议 | examples/websocket-gate/、protocol/README.md | 参考双消息入口、Envelope、Push 及 Go/TS 工具链 |
| SQL 集成 | examples/mysql-product/ | 参考数据库资源边界；业务事务在 demo 实现 |

公开 API 以只读 docs/API-DESIGN.md、CONTEXT.md 及当前实现为准。本项目不复制 Runtime 内部架构裁决到 AGENTS.md，不因示例只有一个 module 而沿用其工程边界。

## Skynet 业务

路径：`../../skynet_bt/nbgameserver`。

| 业务 | 参考文件 | demo 的差异 |
| --- | --- | --- |
| 打地鼠 | gameofshrew/main.lua、roomd.lua、agent.lua | 仅借鉴启动、进入/离开、命中等业务问题；不照搬概率池、倍率、计费和旧协议。最小多人同组玩法仍必须完整结算到账 |
| 三人斗地主 | gameofhlddz/README、main.lua、baseattackd.lua、attackd1.lua、roomd1.lua | 优先参考普通三人流程；首版规则按 DESIGN 明确裁剪，牌型变体与积分公式在该切片前固定 |

`gameofhlddz/attackd1.lua` 明确出现“三人斗地主结算”；`gameofsrddz/attackd.lua` 有“四人斗地主结算”，后者不作为三人实现的首选。

参考目录严格只读。旧实现的经济规则、协议、历史扩展和客户端信任方式均不自动成为 demo 契约。具体算法参考前检查是否依赖外部 Lua/C 模块，不假定只复制一个文件便可复用。

## GoServerActorFsm

路径：`../GoServerActorFsm`，严格只读。参考 HEAD 为 `ecbb971534b429643a704dfa05410687da611d9b`；阅读时工作树存在用户未提交改动（包含配置与 README 等），以下是当前工作树证据，不假定全部属于该提交。未修改或运行其测试/生成命令。

| 部分 | 已核对文件 | 在 GSR Demo 中的采用方式 |
| --- | --- | --- |
| 容量分组与座位 | internal/room/room_actor.go、room_actor_test.go | RoomDirectory 为等待组分配成员，每组一个 MoleRoomService；组满转入下一组 |
| 人数阈值与共享局 | internal/room/attack_actor.go | 满员后启动共享时间线；组内玩家共用目标和局状态 |
| 实例与服务端时间窗口 | internal/gamelogic/shrew_timeline.go、shrew_timeline_test.go | RoundID 加实例序号识别目标；服务端验证点击窗口，旧实例无效 |
| 同组广播与快照 | internal/room/attack_actor.go、docs/client-sync-contract.md | 向本组 Agent 推送共享状态，重连读取权威快照，版本过滤乱序消息 |
| 简单命中奖励 | internal/gamelogic/kick.go | 首版固定每次合法命中 10 积分；只接受服务端已验证的单个目标 |
| 用户状态迁移 | internal/fsm/player.go | 借鉴业务枚举与显式迁移表；demo 按 USER-SESSION 分开连接、位置和房间阶段，补充认证/迁移未知及协议准入 |

参考当前默认配置为容量 10、至少 3 人开始；demo 为缩小流程默认三人一组且满员开始，组人数可配置。参考的 RoomActor → AttackActor → PlayerActor 链不直接移植：GSR 中组状态和个人本局分数由同一个 MoleRoomService 拥有，连接及认证由已有 Agent 承担。旧 Envelope、ActorRef、reply channel、自研 FSM 和地图动画不引入本项目。

参考中的“命中结算”是规则计算及回包；当前所读文件没有全组局末结果持久化、积分账本同事务提交或结算恢复的实现证据。本项目必须另行实现这些流程，不能据其演示已能命中就省略完整结算。

当前切片已实际运行 GSR 节点宿主和诊断通信，GSR 仍从本地只读目录构建，参考源码未修改。真实五节点 Call/Push 及退出已通过 race 进程测试；认证、多人玩法、数据库结算和恢复未实现，不能据此宣称完整业务验收。
