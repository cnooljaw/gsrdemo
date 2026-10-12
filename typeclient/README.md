# TypeScript 客户端

独立 npm 项目，用于 CLI 和机器人，不是 Go module。工具链版本与 GSR WebSocket 示例一致，package-lock.json 已锁定；无需在只读参考目录安装依赖。

```sh
npm ci
npm run check
npm run build
```

当前只建立工具链和 source 目录，没有可运行的游戏客户端。后续从 common/proto 生成 src/gen 类型，接入 GSR Envelope 与 Gate，再实现多客户端登录、分组入座、满员开局、竞争命中、共享快照与计分板、局末状态及查询每人结算到账；同一 CLI/机器人将用于三个玩家的斗地主验收。

node_modules 与 dist 不提交。当前依赖用途：protobuf 用于消息编解码；Buf/protoc-gen-es 用于协议生成；TypeScript 和 Node 类型用于编译检查。protoc-gen-go 版本见 common/proto/README.md。
