# 共享 Go module

model 放已被多个节点使用的业务标识、值对象和独占消息快照；command 放跨节点命令契约；proto 是业务 Protobuf 的单一来源，gen 放生成的 Go 代码。当前只有包骨架，没有提前定义业务类型或消息编号。

依赖方向为节点 → common，common 不导入节点实现。不放 Service、数据库访问、启动组装或可变游戏状态，不为了复用两个游戏而建立通用游戏基类。按真实调用提取共享代码。

复杂跨节点载荷编码为 Protobuf bytes，不直接发送业务 Go struct。客户端消息编号与 Core CommandID 分开；框架错误编号 9001 不占用。
