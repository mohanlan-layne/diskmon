# file_catalog 补偿任务设计（size/is_dir 补全与清理）

日期：2026-06-22
分支：split-admin-public-ui

## 背景与问题

客户端用 NTFS USN Journal 实时监听文件变化并写入 `file_catalog`。上传工具是
两步式写文件（先建临时文件 `X.ext.ldtmp`，写完再改名成 `X.ext`），由此产生两类
脏数据：

1. **size 为 NULL**：`enrichSize` 在事件处理时对路径做一次 `os.Stat`，失败就静默
   留空、不重试。处理时刻文件常已被改名/移走/占用，stat 失败 → size 落 NULL。
   注意 NULL 表示“未测到”，不是“0 字节”，磁盘上文件本身是完整的。
2. **残留临时文件行**：rename 旧/新名两条 USN 记录未在 `rename_window_ms`（500ms）
   内配对时，临时文件 `.ldtmp` 的删除事件丢失，孤儿行与最终文件行并存。
3. **is_dir 写错**：客户端把部分通过 rename 创建的目录记成 `is_dir=0`。

后果：服务端打包下载曾用 `size>0` 过滤，导致 NULL-size 的真实文件（如 `.step`）
下不到（已单独修复，移除了 `size>0` 过滤）。本设计从数据层做补偿，根治脏数据。

适用范围：东莞图档库 `dc-it-s-31`、加工程序（CNC partnumber）等**所有**服务器，
按数据中出现的 `server_id` 通用遍历，不写死。

## 架构：XXL-JOB 自动注册 + Go 执行器 SDK

服务端在 K8s 内无文件系统访问，只能经 AList 查看真实文件元数据。

- diskmon-server 内嵌 Go 执行器 SDK `github.com/xxl-job/xxl-job-executor-go`。
- 启动时拉起内嵌执行器（监听 `:9999`），用 accessToken 向 admin 周期上报，
  **自动注册**到执行器组 `diskmon`（与现有 Java 服务一致 `addressType=0`）。
- 注册 BEAN 任务 `backfillCatalog`（Go 函数）。admin 建调度任务：执行器组
  `diskmon`、JobHandler `backfillCatalog`、cron `0 */10 * * * ?`（每 10 分钟）。
- “自动注册”是执行器 SDK 的能力；纯 HTTP+shell-curl 无法自动注册，故采用 SDK。
  逻辑仍全部写在 Go 里，触发方式为 XXL-JOB 调度器直接调用注册的 handler。

### 集群参数（已确认）

- admin 注册地址（集群内）：`http://xxl-job-admin.middleware:8080/xxl-job-admin`
- accessToken：来自 secret `xxl-job-mysql` 的 `XXL_JOB_ACCESS_TOKEN`（值 `default_token`）
- 执行器端口约定：`9999`
- 执行器组 appname：`diskmon`

### 配置与部署改动（fleet）

- `diskmon-deployment.yaml` 增环境变量：`XXL_JOB_ADMIN_ADDR`、`XXL_JOB_ACCESS_TOKEN`
  （从 secret 取）、`XXL_JOB_EXECUTOR_PORT=9999`、`XXL_JOB_APPNAME=diskmon`。
- Service 增 `9999` 端口（规整；admin 实际是主动连 pod IP:9999）。
- 配置增开关 `xxl.enabled`，本地/测试默认关闭，避免本地起服务也连 admin。
- 一次性：admin 建执行器组 `diskmon`（自动注册）+ 调度任务（可用 admin API 建）。

## backfillCatalog 处理逻辑

### AList 新增能力

`internal/alist/client.go` 加 `Stat(ctx, alistPath) (*FileInfo, error)`，调
`POST /api/fs/get`（body `{"path":..., "password":""}`，复用 `c.post`）。
返回 `{IsDir bool, Size int64, Modified time.Time, Exists bool}`；AList 返回
object-not-found 类错误时 `Exists=false`。

### 一次运行流程

1. 查待处理行（每次封顶 1000）：
   ```sql
   SELECT id, server_id, path FROM file_catalog
   WHERE size IS NULL AND updated_at < NOW() - INTERVAL 10 MINUTE
   ORDER BY server_id LIMIT 1000
   ```
   10 分钟时间护栏：刚上传的文件可能仍在写，跳过，避免与上传竞争。
2. 按 `server_id` 分组，每组 `loadAListMounts` 拿挂载（失败跳过该组、计 error）。
3. 逐行 `mounts.resolveURLPath(path)` → `alist.Stat`，按判定树落库：

| AList 探测结果 | 动作 |
|---|---|
| 目录 `is_dir=true` | `UPDATE is_dir=1`（保留，顺带修坏 is_dir） |
| 文件 `size>0` | `UPDATE size=?, is_dir=0` |
| 文件 `size=0` | `DELETE`（0 字节临时/无效文件） |
| 不存在 / 路径解析不到 | `DELETE` |
| AList 调用出错 | 跳过、计 error、保留行下次再试 |

4. 返回摘要 `{scanned, fixed_file, fixed_dir, deleted, not_found, errors}`，
   XXL-JOB 执行日志可见。

### 安全与健壮性

- catalog 新增 `BackfillUpdate` / 复用 `Delete`，全部参数化。
- 每行独立处理，单行报错不影响整批。
- 仅“文件且 size=0”才删，目录绝不删，避免误伤真实文件夹。
- 单次封顶 + 10 分钟护栏，避免打爆 AList、避免与上传竞争。

## 测试

- `alist.Stat`：`httptest` 模拟 AList 各响应（目录 / 有大小文件 / 0 字节 / not-found）。
- handler 判定树：测试库或 sqlmock 验证四个分支各自的 UPDATE/DELETE。
- AList 不可达：整批不崩，计入 errors，行保留待下次。
- 执行器注册：单测覆盖 enabled=false 时不启动、不连 admin。

## 不做（YAGNI）

- 不处理 DB 中 `size=0` 的既有行（本次只扫 NULL；如需可后续扩）。
- 不做分片广播（单实例足够）。
- 不在本任务内改客户端 USN 监听逻辑（数据层补偿即可，监听侧另议）。
