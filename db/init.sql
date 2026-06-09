-- diskmon 数据库初始化脚本
-- 执行方式: mysql -h <host> -u <user> -p < db/init.sql

CREATE DATABASE IF NOT EXISTS diskmon
    CHARACTER SET utf8mb4
    COLLATE utf8mb4_unicode_ci;

USE diskmon;

-- 服务器配置表
CREATE TABLE IF NOT EXISTS servers (
    id         INT UNSIGNED    NOT NULL AUTO_INCREMENT,
    server_id  VARCHAR(100)    NOT NULL,
    name       VARCHAR(200),
    smb_host   VARCHAR(200),
    smb_user   VARCHAR(100),
    smb_pass   VARCHAR(200),
    sys_root   VARCHAR(500),
    volumes    JSON,
    api_addr   VARCHAR(200),
    api_token  VARCHAR(200),
    alist_urls JSON,
    created_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_server_id (server_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 文件目录主表（LIST 分区，每台 Windows 服务器一个分区）
CREATE TABLE IF NOT EXISTS file_catalog (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    server_id  VARCHAR(100)    NOT NULL,
    volume     VARCHAR(10)     NOT NULL,
    path       VARCHAR(2048)   NOT NULL,
    is_dir     TINYINT(1)      NOT NULL DEFAULT 0,
    size       BIGINT,
    ext        VARCHAR(50),
    biz_key    VARCHAR(500),
    updated_at DATETIME(6)     NOT NULL,
    synced_at  DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id, server_id),
    UNIQUE KEY uk_server_path (server_id, path(650))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  PARTITION BY LIST COLUMNS(server_id) (
    PARTITION p_placeholder VALUES IN ('__placeholder__')
  );

CREATE INDEX idx_biz_key ON file_catalog (biz_key);
CREATE INDEX idx_ext     ON file_catalog (server_id, ext);
CREATE INDEX idx_dir     ON file_catalog (server_id, is_dir);

-- 专用账号（可选，生产建议用独立账号而非 root）
-- CREATE USER IF NOT EXISTS 'diskmon'@'%' IDENTIFIED BY 'CHANGE_ME';
-- GRANT SELECT, INSERT, UPDATE, DELETE, ALTER ON diskmon.* TO 'diskmon'@'%';
-- FLUSH PRIVILEGES;

SELECT 'diskmon 初始化完成' AS status;
