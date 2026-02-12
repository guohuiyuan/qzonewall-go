#!/usr/bin/env bash
set -euo pipefail

echo "========================================"
echo "QzoneWall-Go - 启动脚本 (Linux/macOS)"
echo "========================================"

echo "Go 版本:"
go version

echo "整理依赖..."
go mod tidy

echo "生成资源文件 (winres / embed 等)..."
go generate ./...

echo "构建项目..."
go build -trimpath -ldflags="-s -w" -o wall ./cmd/wall

echo "运行程序..."
./wall
