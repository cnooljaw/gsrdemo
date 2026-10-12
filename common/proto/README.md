# 业务协议定义

在真实通信切片加入 .proto，业务协议只在此定义一次，同时生成 common/gen 的 Go 类型和 typeclient/src/gen 的 TypeScript 类型。当前尚无业务 schema。

GSR Envelope、ErrorResponse 和协议选项复用 只读本地 GSR 1.0 基线的框架定义，不手写第二套。生成脚本必须从 gsrdemo 调用，限制输出到本项目；不直接执行 GSR 示例的 generate 命令，因为它会写入参考仓库。

生成工具沿用 typeclient 锁定的 Buf/protoc-gen-es；protoc-gen-go 固定为 v1.36.1 并安装到本项目忽略的 bin/tools。接入实际 schema 时再建立并验证生成配置，必要生成代码纳入 Git。
