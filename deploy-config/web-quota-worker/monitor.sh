#!/bin/bash
# Worker 监控脚本 - 跟踪反风控增强效果

CONTAINER_NAME="web-quota-worker"

echo "================================"
echo "Web Quota Worker 监控报告"
echo "================================"
echo ""
echo "生成时间：$(date '+%Y-%m-%d %H:%M:%S')"
echo ""

# 检查容器状态
if ! docker ps | grep -q "$CONTAINER_NAME"; then
    echo "✗ 容器未运行"
    exit 1
fi

echo "✓ 容器运行中"
echo ""

# 获取日志
LOGS=$(docker logs "$CONTAINER_NAME" 2>&1)

# 统计 410 错误
echo "【410 错误统计】"
ERROR_410_COUNT=$(echo "$LOGS" | grep -i "410\|gone" | wc -l)
echo "  410/Gone 错误次数：$ERROR_410_COUNT"
echo ""

# 统计登录相关
echo "【登录统计】"
LOGIN_START=$(echo "$LOGS" | grep -i "web_console_remote_login.*start\|login.*session.*start" | wc -l)
LOGIN_SUCCESS=$(echo "$LOGS" | grep -i "authenticated\|login.*success" | wc -l)
LOGIN_FAILED=$(echo "$LOGS" | grep -i "login.*fail\|session.*expired" | wc -l)

echo "  登录会话开始：$LOGIN_START"
echo "  登录成功：$LOGIN_SUCCESS"
echo "  登录失败：$LOGIN_FAILED"

if [ "$LOGIN_START" -gt 0 ]; then
    SUCCESS_RATE=$(echo "scale=2; $LOGIN_SUCCESS * 100 / $LOGIN_START" | bc)
    echo "  成功率：${SUCCESS_RATE}%"
fi
echo ""

# 最近的错误
echo "【最近的错误】（最近 10 条）"
echo "$LOGS" | grep -i "error\|fail\|exception" | tail -10
if [ $(echo "$LOGS" | grep -i "error\|fail\|exception" | wc -l) -eq 0 ]; then
    echo "  无错误"
fi
echo ""

# 容器资源使用
echo "【资源使用】"
docker stats "$CONTAINER_NAME" --no-stream --format "  CPU: {{.CPUPerc}}\n  内存: {{.MemUsage}}\n  网络: {{.NetIO}}"
echo ""

# 运行时长
echo "【运行时长】"
UPTIME=$(docker inspect -f '{{.State.StartedAt}}' "$CONTAINER_NAME")
echo "  启动时间：$UPTIME"
echo ""

echo "================================"
echo "实时监控命令："
echo "  docker logs -f $CONTAINER_NAME"
echo ""
echo "详细分析："
echo "  docker logs $CONTAINER_NAME 2>&1 | grep -i '410'"
echo "  docker logs $CONTAINER_NAME 2>&1 | grep -i 'authenticated'"
echo "================================"
