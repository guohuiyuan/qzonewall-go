# !/bin/bash
# 单独为京东云准备的
# 下载 docker-compose 到指定目录
curl -SL https://github.com/docker/compose/releases/download/v2.24.6/docker-compose-linux-x86_64 -o /usr/local/bin/docker-compose

# 添加执行权限
chmod +x /usr/local/bin/docker-compose

# 验证安装
docker-compose --version


# 1. 创建或编辑配置文件
mkdir -p /etc/docker
tee /etc/docker/daemon.json <<-'EOF'
{
  "registry-mirrors": [
        "https://mirror.ccs.tencentyun.com",
        "https://docker.1ms.run",
        "https://docker.xuanyuan.me"
    ]
}
EOF

# 2. 重启 Docker
systemctl daemon-reload
systemctl restart docker