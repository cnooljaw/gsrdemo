# Dhmj · 四人定海麻将

固定目录负责随机凑桌、准备和重登。开局后每桌由独立 AttackService 串行持有手牌、响应窗口和托管状态。

- [directory.go](directory.go) 管理座位与登录路由，向同桌玩家转发事件。开局时将等待区序号交给牌桌接着递增。
- [attackservice.go](attackservice.go) 处理牌桌操作和服务端计时。[turn.go](turn.go) 裁决弃牌响应，[selfaction.go](selfaction.go) 处理自摸与杠，[roundevents.go](roundevents.go) 按接收座位生成事件。
- [scoring.go](scoring.go) 计算理论付款。[roundsettlement.go](roundsettlement.go) 先保存原请求再调用结算写池，确认后由牌桌通知目录转发本人回执。[settlementjournal.go](settlementjournal.go) 实现 Redis 待结算记录。

[麻将设计](../../docs/DESIGN.md#10-定海麻将随机房与局末结算) · [运行方式](../../README.md#定海麻将四人演示)
