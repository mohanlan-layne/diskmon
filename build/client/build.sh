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

echo "-> 复制安装脚本 ..."
cp scripts/install-client.ps1 "$OUT/install-client.ps1"

echo ""
echo "==========================================================="
echo "  构建完成: $OUT/"
echo ""
echo "  dist/diskmon-client/"
echo "    diskmon-client.exe   <- 主程序"
echo "    install-client.ps1   <- 安装为 Windows 服务脚本"
echo ""
echo "  config 文件在 build/ 目录："
echo "    build/config-template.yaml     <- 新机器模板"
echo "    build/config-dc-it-s-31.yaml   <- 东莞成品代码图档库"
echo "    build/config-partnumber.yaml   <- CNC 10号厂房"
echo ""
echo "  部署步骤："
echo "  1. 从 build/ 取对应 config（或从 config-template.yaml 新建）"
echo "  2. 将 exe + config + install-client.ps1 复制到目标机器 C:\\diskmon\\"
echo "  3. 以管理员身份运行 PowerShell："
echo "     cd C:\\diskmon"
echo "     .\\diskmon-client.exe --config C:\\diskmon\\<config>.yaml --install --rescan"
echo "==========================================================="
