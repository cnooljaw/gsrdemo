-- 用途：初始化本机 login 示例库及账号表；由管理员执行，不在节点启动时自动迁移。
-- 范围：仅 gsrdemo_login；不修改 GSR 示例库。
CREATE DATABASE IF NOT EXISTS gsrdemo_login CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
USE gsrdemo_login;

CREATE TABLE IF NOT EXISTS accounts (
    user_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    username VARBINARY(64) NOT NULL,
    password_hash BINARY(32) NOT NULL,
    PRIMARY KEY (user_id),
    UNIQUE KEY accounts_username (username)
) ENGINE=InnoDB;

-- 复用 GSR 的本机示例用户；DELETE 仅供显式集成测试清理自己创建的账号。
GRANT SELECT, INSERT, DELETE ON gsrdemo_login.accounts TO 'gsr_demo'@'127.0.0.1';
