-- diskmon MySQL 8.0 schema
-- charset: utf8mb4, engine: InnoDB

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
    api_addr   VARCHAR(200),   -- client 管理 API，如 192.168.1.10:19090
    api_token  VARCHAR(200),
    alist_urls JSON,           -- volume → AList /d/xxx URL，注册时写入
    created_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uk_server_id (server_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 文件目录主表（按 server_id LIST 分区）
-- 分区方式：每台服务器一个分区，新增服务器时 ALTER TABLE ADD PARTITION（在线 DDL，无需重启）
-- 全量重扫时用 ALTER TABLE TRUNCATE PARTITION p_xxx（O(1) 极快）
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
    -- 唯一路径：prefix(700) 覆盖 99.9% 的实际路径长度（Windows MAX_PATH 通常 < 260）
    UNIQUE KEY uk_server_path (server_id, path(650))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
  PARTITION BY LIST COLUMNS(server_id) (
    -- 占位分区，实际 server 分区由 diskmon 在注册时动态添加
    PARTITION p_placeholder VALUES IN ('__placeholder__')
  );

-- 业务标识检索（物料+版本）
CREATE INDEX idx_biz_key ON file_catalog (biz_key);

-- 扩展名筛选
CREATE INDEX idx_ext ON file_catalog (server_id, ext);

-- 目录树查询
CREATE INDEX idx_dir ON file_catalog (server_id, is_dir);

-- 新增服务器示例（由 diskmon-server 自动执行，无需手动操作）:
-- ALTER TABLE file_catalog ADD PARTITION (
--     PARTITION p_server_a VALUES IN ('server-a')
-- );

-- 全量重扫清空示例（O(1)，不锁表）:
-- ALTER TABLE file_catalog TRUNCATE PARTITION p_server_a;
