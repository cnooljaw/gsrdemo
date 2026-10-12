# TypeScript 客户端

独立 npm 项目，当前提供切片 A 的通信探测 CLI。工具链沿用 GSR WebSocket 示例并由 package-lock.json 锁定：protobuf 用于编解码，Buf/protoc-gen-es 用于生成，TypeScript/Node 类型用于编译检查；原生 WebSocket 不额外引入客户端网络库。

```sh
npm ci
npm run check
npm run build
npm run client -- --url ws://127.0.0.1:7888/ws --text "hello demo"
```

先在根目录执行 scripts/run-local.sh。CLI 使用统一 GSR Envelope 和生成的诊断类型：验证响应 request_id、operation_id、text、节点集合；Push 必须 request_id=0 且来自 mole。两者均到达才打印 complete，不能把 Send admission 当成送达。空文本及节点不可用打印响应并以非零状态退出，5 秒等待预算与关闭预算限制进程寿命。

诊断请求不是登录或游戏请求。后续在同一工程接入多人分组、满员开局、共享地鼠、个人计分与全组结算查询，机器人随对应业务增加。

运行 npm run generate（或根目录 scripts/generate.sh）更新 Go/TS/registry 生成结果，输出只在 gsrdemo。生成代码及锁文件纳入 Git，node_modules/dist 不提交。当前只支持本地 Plain Gate，MAC/WSS 客户端接入在对应切片验证。
