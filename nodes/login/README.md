# login 节点项目

职责：账号认证与身份映射。

独立 Go module；同角色不同实例使用同一构建产物和不同配置，不复制项目。实现放 internal/login，启动组装放 cmd/login。只通过命令契约与其他节点通信，不导入其他节点实现。

当前实现通信切片的可执行宿主与诊断 Service/Agent，通过 gsr.Run 启动；实际 login 业务尚未实现。GSR 由本 module 的 replace 引用只读 ../../../gsr，common 引用本仓库 ../../common；关闭 workspace 也可独立构建，基线见根目录 docs/REFERENCES.md。

从根目录执行 bin/login -config configs/login-01.json。启动成功后 stdout 输出一条 JSON ready 事件，业务/基础设施日志写入本实例目录，Ctrl-C/SIGTERM 有序退出。探测协议不能当作业务完成状态。
