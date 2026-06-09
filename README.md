# diskmon

Windows 文件监控 + 统一文件服务平台。

- **client**：运行在 Windows 服务器，通过 NTFS USN Journal 监听文件变更，同步到 PostgreSQL
- **server**：API 服务，提供文件查询、水印、下载、ZIP 打包等能力

详见 `design/overview.md`。
