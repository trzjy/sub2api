#!/bin/bash
# Docker 构建进度监控脚本

LOG_FILE="/tmp/claude-1000/-mnt-data-sub2api/c42b2dc0-8999-465f-9c3c-e0e46b6df2c5/tasks/brxvx6nsw.output"

# 检查构建是否还在运行
if ! ps aux | grep -v grep | grep -q "docker build.*web-quota-worker"; then
    echo "Docker 构建已完成或未运行"

    # 检查最后的输出
    if tail -20 "$LOG_FILE" | grep -q "Successfully built\|Successfully tagged"; then
        echo ""
        echo "✅ 构建成功！"
        echo ""
        tail -10 "$LOG_FILE"
        echo ""
        echo "镜像已创建，可以执行部署："
        echo "  cd /mnt/data/sub2api/deploy-config/web-quota-worker"
        echo "  export WEB_QUOTA_WORKER_TOKEN=你的token"
        echo "  ./deploy.sh"
    elif tail -20 "$LOG_FILE" | grep -q "ERROR\|error\|failed"; then
        echo ""
        echo "✗ 构建失败"
        echo ""
        tail -20 "$LOG_FILE"
    fi
    exit 0
fi

# 获取最新进度
echo "Docker 构建进行中..."
echo ""

LATEST=$(tail -5 "$LOG_FILE")
echo "$LATEST"
echo ""

# 尝试提取下载进度
PROGRESS=$(echo "$LATEST" | grep -oP '\d+\.\d+MB / 729\.81MB' | tail -1)
if [ -n "$PROGRESS" ]; then
    DOWNLOADED=$(echo "$PROGRESS" | awk '{print $1}')
    PERCENT=$(echo "$DOWNLOADED" | awk '{printf "%.1f", ($1/729.81)*100}')
    echo "字体包下载进度: $PROGRESS (${PERCENT}%)"

    # 估算剩余时间
    if [ -f /tmp/docker-build-start-time ]; then
        START_TIME=$(cat /tmp/docker-build-start-time)
        CURRENT_TIME=$(date +%s)
        ELAPSED=$((CURRENT_TIME - START_TIME))
        if [ "$PERCENT" != "0" ]; then
            TOTAL_EST=$(echo "$ELAPSED $PERCENT" | awk '{printf "%.0f", $1 / ($2/100)}')
            REMAINING=$((TOTAL_EST - ELAPSED))
            REMAINING_MIN=$((REMAINING / 60))
            echo "预计还需: ${REMAINING_MIN} 分钟"
        fi
    else
        date +%s > /tmp/docker-build-start-time
    fi
fi

echo ""
echo "实时监控: tail -f $LOG_FILE"
