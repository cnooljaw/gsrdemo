# Landlord · 三人斗地主

固定目录负责随机选桌和成员路由，每桌由独立 AttackService 串行裁决。Access 按目录返回的房间名访问牌桌。

- [landlordservice.go](landlordservice.go) 管理房间索引、入桌预留和空桌回收。[attackservice.go](attackservice.go) 持有座位、牌局和当前轮次。
- [attackgame.go](attackgame.go) 和 [cards.go](cards.go) 实现叫抢、出牌与牌型规则。[landlordpulse.go](landlordpulse.go) 驱动到期动作，[landlordevents.go](landlordevents.go) 组织同桌事件。
- [roundsettlement.go](roundsettlement.go) 在局末生成原结算请求并调用结算写池。[landlordjournal.go](landlordjournal.go) 保存待确认请求，供失败重试或节点重启后补交。

[斗地主设计](../../docs/DESIGN.md#6-斗地主牌桌与局末结算) · [运行方式](../../README.md#运行-demo)
