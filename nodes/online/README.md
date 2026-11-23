# Online · 当前登录位置

每个 UserID 只保留一份当前登录位置，包含 LoginID、AccessNode 和 AgentName。OnlineService 独占内存目录，节点重启后由显式登录重建绑定。

- [onlineservice.go](onlineservice.go) 实现 Bind、Lookup 和 Unbind。新绑定返回旧位置，Access 据此通知旧 Agent 退出。
- Unbind 只删除匹配 LoginID 的绑定，旧连接迟到的断线清理不会删掉新位置。
- [main_service.go](main_service.go) 注册固定的 OnlineService，供 Access 和需要核对登录位置的游戏入口调用。

[位置目录设计](../../docs/DESIGN.md#4-online-位置目录) · [运行方式](../../README.md#运行-demo)
