# web-quota-worker: 无状态 Playwright Sidecar 服务
# 负责上游（compshare / longcat）Web 控制台用量采集与自动重登。
# 设计约束：无状态，不落盘任何凭据；只回传刷新后的 storageState 与结构化用量。
#
# 2026-11-20 用户裁定「根治部署重复下载」：基础环境（Playwright 运行时 + 中文字体 +
# Python 依赖）固化在 web-quota-worker-base:v1，本文件只负责 COPY 业务代码。
# 字体层与 pip 层不再出现在每次部署的构建路径里，部署构建为秒级。
# 基础镜像的重建时机与命令见 Dockerfile.base 头部说明；
# deploy-to-server.sh 会在基础镜像缺失时自动构建一次。
FROM web-quota-worker-base:v1

WORKDIR /app

COPY app.py /app/app.py
COPY login_sessions.py /app/login_sessions.py
COPY adapters/ /app/adapters/

# 以非 root 用户运行（playwright 官方镜像内置 pwuser）
USER pwuser

EXPOSE 18090

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "18090"]
