# 共享 Go module

model 当前放节点配置值对象和 readiness 事件；command 放跨节点 Probe/ProbePush 命令；proto 是诊断业务 Protobuf 的单一来源，gen 放生成的 Go 类型和 GSR 消息目录。诊断契约只用于切片 A，不表示认证、玩法或到账成功。

依赖方向为节点 → common；common 不导入节点实现。common 依赖只读本地 GSR 的 runtime/catalog 及 Protobuf，不放 Service、数据库访问、节点启动组装或可变游戏状态。共享代码按实际调用提取。

复杂跨节点载荷显式编码为 Protobuf bytes，不直接发送业务 Go struct。客户端消息编号与 Core CommandID 分开，框架错误编号 9001 不占用。
