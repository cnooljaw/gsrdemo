# Mole · 打地鼠

地鼠先返回命中和可用银子的即时结果，再后台归批入账。MoleService 串行持有组、成员、目标和内存账，结算服持有权威余额。

- [moleentryservice.go](moleentryservice.go) 校验在线身份并协调进组。已有内存账时直接使用，缺余额时才查询结算读池。
- [moleservice.go](moleservice.go) 处理进组、命中和离开。离开立即退组，已接纳的命中继续结算。[attackgame.go](attackgame.go) 实现目标与命中规则，[molespawn.go](molespawn.go) 推进目标刷新。
- [moleaccounting.go](moleaccounting.go) 维护显示余额、待确认扣返和在途批次。[molesubmissionservice.go](molesubmissionservice.go) 保存原批次、调用结算写池并交回回执，余额有变化时由玩法侧校正。

[地鼠设计](../../docs/DESIGN.md#5-地鼠即时玩法与后台入账) · [运行方式](../../README.md#运行-demo)
