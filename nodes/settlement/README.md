# Settlement · 权威账务

游戏节点提交已算好的银子变动，Settlement 在 MySQL 中保存钱包、流水和回执，按操作身份保证重试幂等。

- [main_service.go](main_service.go) 组装读写两个 GSR Service 池。多个成员并行服务，请求方普通 Call 逻辑池名。
- [settlementwriteservice.go](settlementwriteservice.go) 校验并提交写请求。[settlementstore.go](settlementstore.go) 在事务内核对原操作、锁定钱包并保存流水和回执。
- [settlementgroup.go](settlementgroup.go) 执行多人扣返、余额封顶和手续费规则。[settlementreadservice.go](settlementreadservice.go) 提供余额、操作和批次查询。

[账务设计](../../docs/DESIGN.md#7-settlement并行执行与权威账务) · [协议](../../docs/PROTOCOL.md#26-通用银子结算接口) · [运行方式](../../README.md#运行-demo)
