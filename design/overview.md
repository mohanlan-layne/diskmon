# diskmon 设计概览

## 目标

监控 Windows 服务器上的 NTFS 文件变更，将文件目录状态同步到 PostgreSQL，对外提供统一的文件检索、预览、下载、水印等能力。

## 两端职责

### client（Windows Agent）

运行在每台 Windows 服务器上，以 Windows Service 方式部署。

- 全量初始化：启动时先清除本服务器在 DB 中的所有数据，再全量扫描写入
- 增量监听：使用 NTFS USN Journal 持续捕获文件变更，增量写入 DB

不对外暴露任何 HTTP 接口，直连 PostgreSQL。

### server（API 服务）

无状态服务，部署在 k8s。直连 PostgreSQL，挂载各服务器 SMB 共享。

- 服务器配置管理：server_id、SMB 地址/账号、系统根路径等
- 文件查询 API：按业务标识（物料+版本）、路径、扩展名等查询
- 文件操作：水印、合并、单文件下载、ZIP 打包下载

## 整体架构

```
[Windows Server A]         [Windows Server B]
  diskmon-client               diskmon-client
  全量扫描 + USN监听            全量扫描 + USN监听
        │                            │
        └──────────┬─────────────────┘
                   │ 直连 PostgreSQL
                   ▼
             PostgreSQL
            file_catalog（按 server_id 分区）
            servers（服务器配置）
                   │
                   ▼
            diskmon-server
          ┌─────────────────────┐
          │  /api/servers       │ 服务器配置 CRUD
          │  /api/files         │ 文件查询
          │  /api/files/preview │ 预览（转发 kkfileview）
          │  /api/files/dl      │ 单文件下载
          │  /api/files/zip     │ ZIP 打包下载
          │  /api/files/wm      │ 打水印
          │  /api/files/merge   │ 合并
          └─────────────────────┘
```

## 数据库设计

### servers — 服务器配置表

| 列 | 类型 | 说明 |
|---|---|---|
| id | SERIAL | 主键 |
| server_id | VARCHAR(100) | 唯一标识，client 启动时配置 |
| name | VARCHAR(200) | 显示名称 |
| smb_host | VARCHAR(200) | SMB 主机地址 |
| smb_user | VARCHAR(100) | SMB 账号 |
| smb_pass | VARCHAR(200) | SMB 密码（加密存储） |
| sys_root | VARCHAR(500) | 系统根路径，如 `D:\drawings\` |
| volumes | JSONB | 卷列表及各卷业务规则标识 |
| created_at | TIMESTAMPTZ | |
| updated_at | TIMESTAMPTZ | |

### file_catalog — 文件目录表（按 server_id 分区）

| 列 | 类型 | 说明 |
|---|---|---|
| id | BIGSERIAL | |
| server_id | VARCHAR(100) | 关联 servers.server_id |
| volume | VARCHAR(10) | 卷标，如 `D:` |
| path | VARCHAR(4096) | 完整系统路径 |
| is_dir | BOOLEAN | 是否目录 |
| size | BIGINT | 文件大小（字节），目录为 NULL |
| ext | VARCHAR(50) | 扩展名，目录为 NULL |
| biz_key | VARCHAR(500) | 业务标识（物料号+版本），可 NULL |
| updated_at | TIMESTAMPTZ | 文件变更时间（来自 USN） |
| synced_at | TIMESTAMPTZ | 同步到 DB 的时间 |

**分区**：PARTITION BY LIST (server_id)，每台服务器一个分区，全量重跑时只 TRUNCATE 对应分区。

**索引**：
- UNIQUE (server_id, path) — 主查询键
- (biz_key) WHERE biz_key IS NOT NULL — 业务标识检索
- (server_id, ext) — 按文件类型筛选
- (server_id, is_dir) — 目录树查询

## 业务标识提取（bizrule）

每个 volume 有独立的提取规则，实现 `VolumeRule` 接口：

```go
type VolumeRule interface {
    // 从文件路径提取业务标识，无法识别返回空字符串
    ExtractBizKey(path string) string
}
```

部署到新磁盘时只需新增对应实现并在 client 配置中注册，其余代码不动。

业务标识格式为 `{物料号}_{版本}`，由各盘规则自行定义如何从路径或文件夹名称中解析。

## 目录结构

```
diskmon/
├── cmd/
│   ├── server/          # server 入口
│   └── client/          # client 入口（Windows Service）
├── internal/
│   ├── catalog/         # file_catalog CRUD
│   ├── scanner/         # 全量扫描
│   ├── watcher/
│   │   └── usn/         # NTFS USN Journal 监听
│   ├── bizrule/         # 业务标识提取规则
│   ├── server/
│   │   └── handler/     # HTTP handler
│   └── config/          # 配置加载
├── db/
│   └── schema.sql       # 建表 SQL
├── design/              # 设计文档
├── go.mod
└── README.md
```

## 技术选型

| 项目 | 选型 |
|---|---|
| 语言 | Go |
| 数据库 | PostgreSQL |
| Windows 监听 | NTFS USN Journal（`golang.org/x/sys/windows`） |
| HTTP 框架 | net/http 标准库 或 chi |
| DB 驱动 | pgx |
| 文件预览 | 转发至 kkfileview（k8s fleet） |
| 文件挂载 | SMB（server 端挂载各 Windows 服务器共享） |

## 非目标

- Linux 平台支持（架构预留接口，以后单独实现）
- 文件内容同步（仅元数据 + 事件）
- 亚秒级实时保证
