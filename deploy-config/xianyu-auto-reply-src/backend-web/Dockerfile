# Backend-Web 服务 Dockerfile
# 单阶段构建，直接使用Python源码

# 基础镜像走 docker.io 官方名（由 buildx 的 buildkitd 镜像加速配置拉取，见流水线 BUILDKIT 配置）
FROM python:3.11-slim

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# 配置国内镜像源
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources 2>/dev/null || \
    sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list 2>/dev/null || true
# pip 源：清华镜像对当前构建网络返回 403（2026-08-30 实测），官方 PyPI 可达，
# 故回退官方源以保证镜像可构建；不改动任何依赖版本。
RUN pip config set global.index-url https://pypi.org/simple

WORKDIR /app

RUN apt-get update && apt-get install -y curl ca-certificates gcc \
    fonts-dejavu-core fonts-liberation \
    && update-ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# 复制项目源码
COPY common /app/common
COPY backend-web /app/backend-web
COPY websocket /app/websocket
COPY scheduler /app/scheduler
COPY launcher /app/launcher

# 从pyproject.toml提取并安装依赖
RUN python -c "\
import tomllib; \
deps = tomllib.load(open('/app/backend-web/pyproject.toml','rb')).get('project',{}).get('dependencies',[]); \
open('/tmp/requirements.txt','w').write('\n'.join(deps))" && \
    pip install --no-cache-dir -r /tmp/requirements.txt

RUN python -c "\
import tomllib; \
deps = tomllib.load(open('/app/websocket/pyproject.toml','rb')).get('project',{}).get('dependencies',[]); \
open('/tmp/requirements-ws.txt','w').write('\n'.join(deps))" && \
    pip install --no-cache-dir -r /tmp/requirements-ws.txt

RUN python -c "\
import tomllib; \
deps = tomllib.load(open('/app/scheduler/pyproject.toml','rb')).get('project',{}).get('dependencies',[]); \
open('/tmp/requirements-sch.txt','w').write('\n'.join(deps))" && \
    pip install --no-cache-dir -r /tmp/requirements-sch.txt

RUN python -m playwright install --with-deps chromium
RUN python -m patchright install --with-deps chromium

# 创建日志目录
RUN mkdir -p /app/backend-web/logs /app/websocket/logs /app/scheduler/logs /app/static/uploads /app/browser_data

# 暴露端口（backend 8089 / websocket 8090 / scheduler 8091）
EXPOSE 8089 8090 8091

ENV PYTHONUNBUFFERED=1
ENV PYTHONPATH=/app
ENV TZ=Asia/Shanghai

# 启动服务（launcher 并行启动 backend-web / websocket / scheduler 三个进程）
CMD ["python", "launcher/entrypoint.py"]
