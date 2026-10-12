# GSR Demo

基于 GSR 的多节点游戏业务验证项目。gsrdemo 是唯一开发目录；../gsr、../../skynet_bt/nbgameserver 和 ../GoServerActorFsm 严格只读。

当前完成工程初始化：common 与五个节点独立 Go module、TypeScript 工具链、规范和统一检查脚本。尚未实现业务通信、可执行节点、客户端 CLI、数据库或结算。初始化不等于业务验收。

## 结构

| 项目 | 职责 |
| --- | --- |
| common | 共享值对象、命令契约、业务协议及 Go 生成结果 |
| nodes/access | 接入与 Agent |
| nodes/login | 认证与身份映射 |
| nodes/online | 在线目录与位置协调 |
| nodes/mole | 最小多人同组打地鼠完整流程、局末结果与结算恢复 |
| nodes/settlement | 幂等积分结算及账本 |
| typeclient | 独立 TypeScript CLI/机器人项目 |
| configs、migrations、scripts、tests | 配置、迁移、工程操作和跨进程验收 |

每个节点角色一个项目；access-01/access-02 共用 access。nodes/landlord 在斗地主切片再建立，优先参考 gameofhlddz/。根目录没有 go.mod，一个 Git 仓库管理全部项目。

节点依赖 common 和 gsr，节点之间只通过命令契约通信，不导入对方实现。共享代码按真实需求提取，不建立通用 GameService 基类。

## 准备与验证

Go 1.24 或更新、Node.js 22.15 或更新、npm；当前工具版本沿用只读 GSR 示例并锁定。从根目录执行：

```sh
./scripts/workspace.sh
npm --prefix typeclient ci
./scripts/verify.sh
./scripts/build.sh
```

本地 go.work 被忽略，示例纳入 Git，只组装本仓库 module。五个节点通过自身 go.mod 的 replace 引用只读 ../gsr，以当前本地 1.0 为基线，尚无发布版本；占位版本 v0.0.0 不表示框架的完成程度。两个业务参考仓库只用于阅读；Go 模块缓存与 npm 缓存保留。

独立 module 验证使用 scripts/verify.sh --standalone 和 scripts/build.sh --standalone，关闭 workspace，仍使用节点 go.mod 的本地 GSR/common 替换。需要保留当前兄弟目录布局；第三方依赖首次获取可能需要网络，GSR 本身无需远端下载。GSR 基线见 [参考来源](docs/REFERENCES.md)。

当前 Go build 只编译包骨架，不生成服务二进制；TS build 输出 typeclient/dist。实际启动、readiness、协议生成与数据库迁移随对应切片加入。

## 业务完成标准

先实现多人分别用户名密码登录 → 分配同组及座位 → 满员开局 → 地鼠出现/命中 → 分数更新 → 局结束 → 持久化待结算 → Settlement 事务记账 → 查询积分到账，再验证故障恢复。打地鼠简化玩法，不完整复刻旧游戏；结算不可省略，局结束不能等同于积分到账。

首轮完成是 access、login、online、mole、settlement 五进程完整闭环。同组共享地鼠和局期限、分别记分；同一实例只奖励首个合法命中者。重复/过期命中不得加分，全组结果同事务结算且重复提交不得重复记账，保存过的待结算结果重启后必须补齐。之后加入 access-02 和三人斗地主。

工作规则见 [AGENTS.md](AGENTS.md)，架构见 [DESIGN](docs/DESIGN.md)，实施及验收见 [实施计划](docs/REVIEW-AND-IMPLEMENTATION.md)。默认留工作树供审查，不自动提交或推送。

分组、服务端时间线、组内广播和快照部分参考 GoServerActorFsm；一组由一个 MoleRoomService 拥有，默认三人一组。不移植它的自研 Actor、PlayerActor、FSM 框架或旧 Envelope。
