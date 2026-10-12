# online 启动入口

实际启动接线随通信切片加入 main.go，使用 gsr.Run；main 只处理配置、信号及宿主资源，MainService 显式组装本节点 Service。当前没有可运行服务或 readiness，不用空服务冒充业务节点。
