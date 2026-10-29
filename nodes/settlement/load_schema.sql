-- 用途：管理员建立独立的大数据压测库与可恢复造数检查点。
-- 执行：mysql -u root -p < nodes/settlement/load_schema.sql。
-- 范围：只创建 gsrdemo_settlement_load 并授权；不修改原示例库，不清空数据。
CREATE DATABASE IF NOT EXISTS gsrdemo_settlement_load CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
CREATE TABLE IF NOT EXISTS gsrdemo_settlement_load.silver_balances LIKE gsrdemo_settlement.silver_balances;
CREATE TABLE IF NOT EXISTS gsrdemo_settlement_load.settlement_operations LIKE gsrdemo_settlement.settlement_operations;
CREATE TABLE IF NOT EXISTS gsrdemo_settlement_load.settlement_batches LIKE gsrdemo_settlement.settlement_batches;
-- 仅压测工具使用，检查点与每块造数在同一事务提交，不进入业务节点。
CREATE TABLE IF NOT EXISTS gsrdemo_settlement_load.load_seed_progress (
    dataset_id VARBINARY(64) PRIMARY KEY,
    wallet_count BIGINT UNSIGNED NOT NULL,
    initialized_users BIGINT UNSIGNED NOT NULL,
    game_operations BIGINT UNSIGNED NOT NULL,
    game_batches BIGINT UNSIGNED NOT NULL,
    mole_batch_size INT UNSIGNED NOT NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON gsrdemo_settlement_load.* TO 'gsr_demo'@'127.0.0.1';
