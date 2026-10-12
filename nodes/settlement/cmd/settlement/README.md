# settlement 启动入口

main.go 只加载配置、处理信号并调用 gsr.Run；MainService 显式创建命名诊断 Service（access 打开 Gate 并按连接创建 Agent）及 Cluster。启动成功输出 readiness，不依赖任意 sleep。

当前运行的是通信切片，不提供 settlement 的真实业务。根目录 build.sh 输出 bin/settlement，示例配置为 configs/settlement-01.json。
