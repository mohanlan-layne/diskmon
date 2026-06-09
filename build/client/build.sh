#!/usr/bin/env bash
# 交叉编译 diskmon-client.exe（Windows amd64）
# 用法: bash build/client/build.sh
set -euo pipefail

cd "$(dirname "$0")/../.."

OUT="dist/diskmon-client"
mkdir -p "$OUT"

echo "-> 编译 diskmon-client.exe ..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o "$OUT/diskmon-client.exe" ./cmd/client

echo "-> 复制配置模板和安装脚本 ..."
cp scripts/config.template.yaml "$OUT/config.yaml"
cp scripts/install-client.ps1   "$OUT/install-client.ps1"

echo ""
echo "==========================================================="
echo "  构建完成: $OUT/"
echo ""
echo "  dist/diskmon-client/"
echo "    diskmon-client.exe   <- 主程序"
echo "    config.yaml          <- 配置文件（需按实际修改）"
echo "    install-client.ps1   <- 安装为 Windows 服务脚本"
echo ""
echo "  部署步骤："
echo "  1. 修改 config.yaml（server_id / name / smb_user / volumes / include_dirs）"
echo "  2. 将整个目录复制到目标 Windows 机器 C:\\diskmon\\"
echo "  3. 以管理员身份运行 PowerShell："
echo "     cd C:\\diskmon"
echo "     .\\install-client.ps1 -Rescan"
echo "  4. 复制启动输出中的「注册信息 JSON」到 diskmon 管理界面"
echo "==========================================================="
