#!/usr/bin/env python3
"""
Worker 容器多进程启动器

同一容器内并行启动三个服务：
- backend-web  : uvicorn backend-web.main:app   (端口 8089, BACKEND_WEB_PORT)
- websocket    : uvicorn websocket.main:app     (端口 8090, WEBSOCKET_PORT)
- scheduler    : uvicorn scheduler.main:app     (端口 8091, SCHEDULER_PORT)

主程序通过 http://xianyu-worker-backend:8089 访问 backend-web；
backend 通过 WEBSOCKET_SERVICE_URL / SCHEDULER_SERVICE_URL 访问其余两个进程。

任一子进程退出导致整体退出，由容器 restart policy 拉起。
"""
from __future__ import annotations

import os
import signal
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BACKEND_DIR = ROOT / "backend-web"
WEBSOCKET_DIR = ROOT / "websocket"
SCHEDULER_DIR = ROOT / "scheduler"

PROCESSES: list[subprocess.Popen] = []


def _env(*, port_env: str, default_port: int, cwd: Path) -> dict:
    """构造子进程环境：注入 PYTHONPATH 与工作目录。"""
    env = dict(os.environ)
    env["PYTHONPATH"] = str(ROOT)
    env["PYTHONUNBUFFERED"] = "1"
    env[port_env] = str(os.environ.get(port_env, default_port))
    env["HOST"] = os.environ.get("HOST", "0.0.0.0")
    return env


def spawn(name: str, main: Path, cwd: Path, port_env: str, default_port: int) -> None:
    cmd = [sys.executable, str(main)]
    proc = subprocess.Popen(
        cmd,
        cwd=str(cwd),
        env=_env(port_env=port_env, default_port=default_port, cwd=cwd),
        stdout=None,
        stderr=None,
    )
    PROCESSES.append(proc)
    print(f"[launcher] started {name} (pid={proc.pid})", flush=True)


def main() -> int:
    spawn("backend-web", BACKEND_DIR / "main.py", BACKEND_DIR, "BACKEND_WEB_PORT", 8089)
    spawn("websocket", WEBSOCKET_DIR / "main.py", WEBSOCKET_DIR, "WEBSOCKET_PORT", 8090)
    spawn("scheduler", SCHEDULER_DIR / "main.py", SCHEDULER_DIR, "SCHEDULER_PORT", 8091)

    def _shutdown(signum, frame):
        print(f"[launcher] received {signum}, terminating children", flush=True)
        for proc in PROCESSES:
            if proc.poll() is None:
                proc.terminate()
        for proc in PROCESSES:
            try:
                proc.wait(timeout=15)
            except Exception:
                proc.kill()
        sys.exit(0)

    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)

    # 任一子进程退出则整体退出（restart: unless-stopped 拉起）
    while True:
        for proc in PROCESSES:
            code = proc.poll()
            if code is not None:
                print(f"[launcher] child exited code={code}", flush=True)
                for p in PROCESSES:
                    if p.poll() is None:
                        p.terminate()
                return code or 1
        time.sleep(2)


if __name__ == "__main__":
    raise SystemExit(main())
