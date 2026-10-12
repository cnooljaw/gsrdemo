# 数据库迁移

在结算切片加入版本化 SQL，显式执行迁移，普通节点启动不自动建表。表按写入 owner 划分：Login 的 accounts，游戏的 pending_settlements，Settlement 的 settlements、point_accounts、point_ledger。

局末结果先持久化，再提交幂等结算；余额与账本同事务提交。结算 ID、局号唯一约束、冲突校验与恢复不能因 demo 玩法简化而省略。当前尚无 schema 或数据库验收。
