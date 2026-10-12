# mole 节点项目

职责：最小多人同组打地鼠房间、局末冻结、待结算保存及恢复。

独立 Go module；同角色不同实例使用同一构建产物和不同配置，不复制项目。实现放 internal/mole，启动组装放 cmd/mole。只通过命令契约与其他节点通信，不导入其他节点实现。

当前是可编译包骨架，尚无业务 Handler 或可执行入口。GSR 使用本地 1.0 基线，通过本 module 的 replace 指向只读 ../../../gsr；go.mod 中的 v0.0.0 是未发布模块的占位版本。根目录 go.work 只组装本仓库 module；GSR 和 common 均在自身 go.mod 声明本地替换，关闭 workspace 也能在当前目录布局中构建。基线 revision 见根目录 docs/REFERENCES.md。

一组对应一个 RoomID、一个 MoleRoomService。默认三人一组，满员开局，共享地鼠与局期限，分别记分；同一实例只奖励首个合法命中者，局末冻结全组结果并一次事务结算。只借鉴 GoServerActorFsm 的分组、时间线、广播和快照业务思路。
