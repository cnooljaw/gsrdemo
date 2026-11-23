# TypeScript CLI · 游戏演示客户端

CLI 通过 Access 登录，按 `--game` 运行地鼠、斗地主或定海麻将。客户端消费服务端事件，维护用于演示的本地场景。

- [src/index.ts](src/index.ts) 解析参数、完成认证与登录，并运行地鼠演示或转交牌类流程。
- [src/client.ts](src/client.ts) 处理 WebSocket、请求关联和推送分发。[src/secure_auth.ts](src/secure_auth.ts) 实现传输认证。UserID、银子和序号使用 `bigint`。
- [src/landlord.ts](src/landlord.ts) 与 [src/dhmj.ts](src/dhmj.ts) 提交牌类演示动作。[src/landlord_state.ts](src/landlord_state.ts) 与 [src/dhmj_state.ts](src/dhmj_state.ts) 按序应用事件，缺号时请求快照。结算直接使用本人回执。

[客户端协议](../docs/PROTOCOL.md#1-客户端-cs-协议) · [运行方式](../README.md#6-启动多个客户端) · [构建与测试](../README.md#开发与验证)
