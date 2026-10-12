# access 节点项目

职责：接入、每连接 Agent、协议分发与客户端写回。

客户端始终只连接 access。Agent 管理本连接状态机，持有 Online 已确认位置和 Room 阶段快照，按当前状态准入协议并选择内部路由；具体契约见 [用户状态设计](../../docs/USER-SESSION.md)。这些业务行为在后续切片实现，当前仍仅有诊断通信。

独立 Go module；同角色不同实例使用同一构建产物和不同配置，不复制项目。实现放 internal/access，启动组装放 cmd/access。只通过命令契约与其他节点通信，不导入其他节点实现。

当前实现通信切片的可执行宿主与诊断 Service/Agent，通过 gsr.Run 启动；实际 access 业务尚未实现。GSR 由本 module 的 replace 引用只读 ../../../gsr，common 引用本仓库 ../../common；关闭 workspace 也可独立构建，基线见根目录 docs/REFERENCES.md。

从根目录执行 bin/access -config configs/access-01.json。启动成功后 stdout 输出一条 JSON ready 事件，业务/基础设施日志写入本实例目录，Ctrl-C/SIGTERM 有序退出。探测协议不能当作业务完成状态。
