#!/bin/sh
# 启动 Xvfb 虚拟显示后运行滑块风控探针。
# 探针脚本自身也会在 DISPLAY 缺失时自动拉起 Xvfb，本脚本用于「容器入口」场景：
# 由 entrypoint/docker run 直接以 headed 方式托管显示，再执行探针。
set -e

DISPLAY="${XY_DISPLAY:-:99}"
XSOCK="/tmp/.X11-unix/X${DISPLAY#:}"

if [ ! -e "$XSOCK" ]; then
  echo "[start_xvfb] 启动 Xvfb $DISPLAY ..."
  Xvfb "$DISPLAY" -screen 0 1280x800x24 -ac +extension XTEST -retro >/tmp/xvfb.log 2>&1 &
  XVFB_PID=$!
  for _i in $(seq 1 50); do
    [ -e "$XSOCK" ] && break
    sleep 0.1
  done
  if [ ! -e "$XSOCK" ]; then
    echo "[start_xvfb] Xvfb 启动失败，日志:" >&2
    cat /tmp/xvfb.log >&2
    exit 3
  fi
  export DISPLAY
  trap 'kill "$XVFB_PID" 2>/dev/null || true' EXIT
else
  echo "[start_xvfb] 复用已有 DISPLAY=$DISPLAY"
  export DISPLAY
fi

# Xvfb -ac 关闭访问控制后，Xlib 仍会尝试读取 XAUTHORITY；缺文件会让 pyautogui 导入失败。
export XAUTHORITY="${XAUTHORITY:-/tmp/.xvfb-probe-xauthority}"
: >> "$XAUTHORITY" 2>/dev/null || true

case "${1:-}" in
  --smoke|--input-stdin) PROBE_MODE="$1" ;;
  *) PROBE_MODE="args-redacted" ;;
esac
echo "[start_xvfb] DISPLAY=$DISPLAY，执行探针: mode=$PROBE_MODE"
exec python /app/tools/xvfb_slider_probe.py "$@"
