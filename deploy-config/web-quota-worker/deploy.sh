#!/bin/bash
# Web Quota Worker 部署脚本
# 用于自动化部署带反风控增强的 Worker

set -e

echo "================================"
echo "Web Quota Worker 反风控增强部署"
echo "================================"
echo ""

# 配置
IMAGE_NAME="web-quota-worker:anti-detect"
CONTAINER_NAME="web-quota-worker"
PORT="18090"

# 检查镜像是否存在
if ! docker images | grep -q "web-quota-worker.*anti-detect"; then
    echo "错误：镜像 $IMAGE_NAME 不存在"
    echo "请先运行：docker build -t $IMAGE_NAME ."
    exit 1
fi

echo "✓ 找到镜像：$IMAGE_NAME"
echo ""

# 检查旧容器
if docker ps -a | grep -q "$CONTAINER_NAME"; then
    echo "发现旧容器，准备停止并删除..."
    docker stop "$CONTAINER_NAME" 2>/dev/null || true
    docker rm "$CONTAINER_NAME" 2>/dev/null || true
    echo "✓ 旧容器已清理"
    echo ""
fi

# 读取环境变量
if [ -z "$WEB_QUOTA_WORKER_TOKEN" ]; then
    echo "错误：未设置 WEB_QUOTA_WORKER_TOKEN 环境变量"
    echo "请运行：export WEB_QUOTA_WORKER_TOKEN=your-token"
    exit 1
fi

echo "✓ 环境变量已设置"
echo ""

# 启动新容器
echo "启动新容器..."
docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    -p "$PORT:18090" \
    -e WEB_QUOTA_WORKER_TOKEN="$WEB_QUOTA_WORKER_TOKEN" \
    "$IMAGE_NAME"

echo "✓ 容器已启动"
echo ""

# 等待健康检查
echo "等待服务启动..."
sleep 3

# 检查容器状态
if docker ps | grep -q "$CONTAINER_NAME"; then
    echo "✓ 容器运行正常"
    echo ""

    # 显示日志
    echo "最近的日志："
    docker logs --tail 20 "$CONTAINER_NAME"
    echo ""

    echo "================================"
    echo "✓ 部署成功！"
    echo "================================"
    echo ""
    echo "容器名称：$CONTAINER_NAME"
    echo "镜像：$IMAGE_NAME"
    echo "端口：$PORT"
    echo ""
    echo "常用命令："
    echo "  查看日志：docker logs -f $CONTAINER_NAME"
    echo "  重启容器：docker restart $CONTAINER_NAME"
    echo "  停止容器：docker stop $CONTAINER_NAME"
    echo "  查看状态：docker ps | grep $CONTAINER_NAME"
    echo ""
else
    echo "✗ 容器启动失败"
    echo ""
    echo "错误日志："
    docker logs "$CONTAINER_NAME" 2>&1 || true
    exit 1
fi
