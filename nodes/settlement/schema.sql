-- 用途：由管理员初始化通用结算示例库；应用启动不自动迁移。
-- 范围：仅 gsrdemo_settlement，不修改 login、mole 或 GSR 的数据库。
CREATE DATABASE IF NOT EXISTS gsrdemo_settlement CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
USE gsrdemo_settlement;
CREATE TABLE IF NOT EXISTS silver_balances (
    user_id BIGINT UNSIGNED NOT NULL,
    amount BIGINT NOT NULL,
    version BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS settlement_batches (
    batch_id VARBINARY(128) NOT NULL,
    request_hash BINARY(32) NOT NULL,
    request_json JSON NOT NULL,
    receipt_json JSON NOT NULL,
    PRIMARY KEY (batch_id)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS settlement_operations (
    operation_id VARBINARY(128) NOT NULL,
    request_hash BINARY(32) NOT NULL,
    origin_batch VARBINARY(128) NOT NULL,
    operation_position INT UNSIGNED NOT NULL,
    business VARBINARY(128) NOT NULL,
    status VARBINARY(16) NOT NULL,
    debit BIGINT NOT NULL,
    credit BIGINT NOT NULL,
    PRIMARY KEY (operation_id),
    KEY operations_business_status (business, status)
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON gsrdemo_settlement.* TO 'gsr_demo'@'127.0.0.1';
