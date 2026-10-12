# 业务协议定义

当前 demo/diagnostic/v1/diagnostic.proto 定义通信探测 Request/Response/Push 及内部节点字节载荷。它不表示认证或积分结算。业务协议在此维护，同时生成 common/gen 的 Go 类型及 typeclient/src/gen 的 TypeScript 类型。

后续按 auth、session、mole、settlement 拆分当前切片实际使用的协议；session 承载用户可见状态、位置/迁移查询和状态同步。用户只向 access 发送业务意图，access/Online 按连接和位置状态准入协议，Room 再验证业务阶段，详见 [用户状态契约](../../docs/USER-SESSION.md)。Registry 的消息定义不等于任意用户状态均可调用。

GSR Envelope、ErrorResponse 和消息目录选项复用只读本地框架定义。scripts/generate.sh 将本项目 schema 与只读 GSR schema 组合到临时输入目录，Go 插件只生成本项目协议，TS 同时生成依赖定义。所有输出只落到 gsrdemo，不运行参考仓库的 generate。

工具版本：protoc-gen-go v1.36.1，Buf v1.73.0，protoc-gen-es v2.15.0；Go/registry 插件从锁定依赖及当前本地 GSR 基线构建到 bin/tools，npm 工具由 typeclient/package-lock.json 锁定。生成文件纳入 Git，不手改；registry 与协议一起生成。
