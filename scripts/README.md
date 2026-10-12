# 工程脚本

workspace.sh 创建被忽略的本地 go.work，已有配置保留不覆盖。build.sh 枚举 common、各节点及 tests 下的 module，并输出 bin/<节点> 与 typeclient/dist；verify.sh 运行各 module 默认测试/vet 和 TS 类型检查。

两者支持 --standalone：关闭 workspace，用节点 go.mod 中的本地 GSR/common 替换验证独立 module。GSR 1.0 尚未发布，保持兄弟目录布局；首次获取第三方依赖可能需要网络。Go 检查复用 .cache/go-build，显式 GOCACHE 可覆盖，不清理缓存。

generate.sh 构建固定版本的 Go 生成工具和当前 GSR registry 工具，将 schema 在临时目录组合，然后仅写入 common/gen 与 typeclient/src/gen。生成代码不手改，参考仓库不产生输出。

run-local.sh 在前台启动五节点，等待 stdout readiness；启动失败或任一进程退出时回收本轮子进程。Ctrl-C/SIGTERM 停止，日志按实例隔离。清理先发 TERM，预算内不退出才对自己的 PID 发 KILL；不使用全局 pkill。

process-test.sh 显式构建并运行真实进程验收；--race 使五个节点和测试都启用 race。测试使用临时端口/日志，覆盖两个同时活跃的 TS 连接、Call/Push、空输入、对端停止及清理。默认 verify.sh 跳过真实进程测试，数据库尚未加入。
