# Access · 客户端接入

每条连接由一个 Agent 持有登录身份和当前游戏路由。Gate 负责传输，游戏节点持有玩法状态，Settlement 持有权威余额。

- [main_service.go](main_service.go) 创建 Gate 并组装 Agent。[agent.go](agent.go) 和 [agent_handlers.go](agent_handlers.go) 分别接入客户端 Protobuf 消息和节点内部命令。
- [agent_login.go](agent_login.go) 处理密码认证与 Token 登录，登录成功后向 Online 绑定连接。替换旧连接和断线清理都按 LoginID 校验。
- [commole.go](commole.go)、[comlandlord.go](comlandlord.go)、[comdhmj.go](comdhmj.go) 定义各游戏的请求、推送与离开逻辑。Agent 用同一份组件清单完成注册、进入互斥和断线清理。
- [agent_failure.go](agent_failure.go) 处理认证、登录和进入失败时的回包与关闭边界。

[接入设计](../../docs/DESIGN.md#3-接入认证与登录) · [协议](../../docs/PROTOCOL.md) · [运行方式](../../README.md#运行-demo)
