# diskmon-server 界面/API 拆分设计

日期：2026-06-12

## 目标

把 diskmon-server 现有的单页 Web UI（服务器配置 / 文件浏览 / 接口文档三合一）拆成
两部分，避免内部配置、管理功能直接对外暴露：

- **对外**：只保留 `/api/*` 文件数据接口供外部系统调用，diskmon-server 自身不再提供任何浏览界面。
- **对内**：所有可视化界面（服务器配置、客户端管理、文件浏览/预览、接口文档）收拢到
  一个需登录的管理页 `/admin/`。

> 公开的「前端预览」由独立的 AList 服务的 Guest 模式负责（本次不改动 AList 配置）。

## 决策

- 页面边界：对外纯 API 无界面；全部 UI 进内部管理页。
- API 保护面：只保护配置类接口（`/api/servers/*`、`/api/alist/*`、`/api/clients/*`）；
  文件数据接口（`/api/files`、`/api/file/*`、`/api/biz/*`、`/api/drawing/*`、
  下载/ZIP/水印/预览/合并）保持匿名。
- 登录机制：登录页 + 进程内 session cookie。
- 账号密码：放 `server-config.yaml` 的 `admin` 段，生产由 K8s Fleet configmap/secret 提供，
  `ADMIN_PASSWORD` 环境变量可覆盖（同 `ALIST_PASSWORD` 套路）。

## 路由结构

```
公开组（无认证）
  GET  /                        → 302 /admin/
  GET  /admin/login             登录页 HTML
  POST /admin/login             校验凭证 → 下发 session cookie
  POST /admin/logout            清除 session
  GET  /api/files               文件列表
  GET  /api/file/{id}/download|preview|watermark
  GET  /api/files/dl  POST /api/files/zip
  GET  /api/files/preview
  POST /api/files/watermark|merge
  /api/biz/*  /api/drawing/*  /api/pdf-printing/*

管理组（RequireAuth 中间件 = session cookie）
  GET  /admin/                  管理界面 HTML
  /api/servers/*  /api/alist/ping   服务器配置
  /api/clients/*                客户端管理
```

未登录时：页面路由 302 跳 `/admin/login`；`/api/*` 返回 401（前端 `api()` 收到 401 跳登录页）。

## 实现要点

- `internal/config/config.go`：新增 `AdminConfig{User,Password}`，`ADMIN_PASSWORD` env 覆盖，
  `user` 默认 `admin`，`password` 为空则启动报错（防止误开放管理面）。
- `internal/server/handler/admin.go`：`AdminHandler`，含 `Login`/`Logout` + `RequireAuth` 中间件；
  会话为进程内 `map[token]expiry`（mutex 保护，TTL 12h，重启失效）；token 为 32B 随机；
  凭证用 `crypto/subtle` 常量时间比较；cookie `diskmon_session`（HttpOnly, SameSite=Lax）。
- `cmd/server/main.go`：embed `web/admin.html` + `web/login.html`；router 拆公开组 / `r.Group` 管理组。
- `cmd/server/web/`：`index.html` → `admin.html`（加退出登录按钮 + `api()` 401 跳转）；
  新增 `login.html`。
- 现有 handler 正好不跨界：`ServersHandler`、`ClientsHandler` 进管理组，其余进公开组。

## 部署补充（Fleet 仓库，独立）

在 diskmon-server 的 configmap 加 `admin.user`，secret 注入 `ADMIN_PASSWORD`。

## 测试

`internal/server/handler/admin_test.go`：覆盖无 session 时页面 302 / API 401、错误密码拒绝、
正确登录下发 cookie 并解锁两类路由、登出后失效。
