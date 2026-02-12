#!/bin/bash

# QzoneWall-Go Docker 部署脚本
# 用于在新电脑上快速部署 qzonewall-go

set -e

echo "🚀 开始部署 QzoneWall-Go..."

# 检查 Docker 是否安装
if ! command -v docker &> /dev/null; then
    echo "❌ Docker 未安装，请先安装 Docker"
    exit 1
fi

# 创建工作目录
WORK_DIR="qzonewall-deploy"
if [ -d "$WORK_DIR" ]; then
    echo "⚠️  目录 $WORK_DIR 已存在，使用现有目录"
else
    mkdir "$WORK_DIR"
    echo "📁 创建工作目录: $WORK_DIR"
fi

cd "$WORK_DIR"

# 拉取最新镜像
echo "📦 拉取 Docker 镜像..."
docker pull guohuiyuan/qzonewall-go:latest

# 创建示例配置文件
if [ ! -f "config.yaml" ]; then
    echo "📝 创建示例配置文件..."
    cat > config.yaml << 'EOF'
# QzoneWall-Go 配置文件
# 请根据需要修改以下配置

# QQ空间配置
qzone:
  keep_alive: 10s
  max_retry: 2
  timeout: 30s

# QQ 机器人配置
bot:
  zero:
    nickname:
      - "表白墙"
    command_prefix: "/"
    super_users:
      - 123456789  # 替换为你的 QQ 号
    ring_len: 4096
    latency: 1000000
    max_process_time: 240000000000
  ws:
    - url: "ws://localhost:3001"  # 替换为你的 NapCat 地址
      access_token: "your_token"   # 替换为你的 token

# 表白墙配置
wall:
  show_author: false
  anon_default: false
  max_images: 9
  max_text_len: 2000
  publish_delay: 0s

# 数据库
database:
  path: "data.db"

# Web 管理后台
web:
  enable: true
  addr: ":8081"
  admin_user: "admin"
  admin_pass: "change_this_password"  # 务必修改默认密码！
EOF
    echo "✅ 配置文件已创建: config.yaml"
    echo "⚠️  请编辑 config.yaml 文件，配置你的 QQ 号、NapCat 地址和密码"
else
    echo "ℹ️  配置文件已存在，跳过创建"
fi

# 停止可能存在的旧容器
if docker ps -a --format 'table {{.Names}}' | grep -q "^qzonewall$"; then
    echo "🛑 停止旧容器..."
    docker stop qzonewall || true
    docker rm qzonewall || true
fi

# 运行容器
echo "🏃 启动容器..."
docker run -d \
  --name qzonewall \
  --restart unless-stopped \
  -p 8081:8081 \
  -v "$(pwd)/config.yaml:/home/appuser/config.yaml" \
  -v "$(pwd)/data.db:/home/appuser/data.db" \
  guohuiyuan/qzonewall-go:latest

# 等待容器启动
echo "⏳ 等待服务启动..."
sleep 3

# 检查容器状态
if docker ps | grep -q qzonewall; then
    echo "✅ 容器启动成功!"
    echo ""
    echo "🌐 管理后台: http://localhost:8081"
    echo "👤 默认账号: admin"
    echo "🔑 默认密码: admin123 (请立即修改!)"
    echo ""
    echo "📊 查看日志: docker logs -f qzonewall"
    echo "🛑 停止服务: docker stop qzonewall"
    echo "🔄 重启服务: docker restart qzonewall"
else
    echo "❌ 容器启动失败，请检查配置和日志"
    echo "📊 查看日志: docker logs qzonewall"
    exit 1
fi

echo ""
echo "🎉 部署完成！请访问 http://localhost:8081 配置你的表白墙"