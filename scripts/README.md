# 工程脚本

从根目录运行 scripts/workspace.sh 创建被忽略的本地 go.work；已有文件保留不覆盖。scripts/build.sh 编译所有业务 module 并构建 typeclient，scripts/verify.sh 运行各 module 测试、vet 及客户端类型检查。

两者支持 --standalone：关闭 workspace，用各节点 go.mod 声明的本地 GSR/common 替换验证独立 module。GSR 1.0 尚未发布，当前依赖只读 ../gsr，不下载假定的发布版本；保留兄弟目录布局即可使用。第三方依赖首次获取仍可能需要网络。

两个脚本默认复用 .cache/go-build，避免沙箱无法写入系统缓存；显式 GOCACHE 可覆盖，不清理缓存。

当前只有包骨架，Go build 不产生节点二进制。加入实际 cmd/<节点>/main.go 后，构建脚本应升级为输出 bin/<节点>。协议生成及启动/停止脚本随实际 schema、配置及 readiness 加入并验收，当前不提供假启动脚本。

后续启动失败需回收本次启动的子进程，日志按实例隔离；停止脚本只操作自己记录且核对身份的进程，不按名称全局 pkill。生成仅输出 common/gen 和 typeclient/src/gen，迁移显式执行。
