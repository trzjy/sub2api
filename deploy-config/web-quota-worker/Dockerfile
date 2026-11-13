# web-quota-worker: 无状态 Playwright Sidecar 服务
# 负责三家上游（compshare / sensenova / longcat）Web 控制台用量采集与自动重登。
# 设计约束：无状态，不落盘任何凭据；只回传刷新后的 storageState 与结构化用量。
FROM mcr.microsoft.com/playwright/python:v1.49.1-jammy

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PW_BROWSERS_PATH=/ms-playwright

WORKDIR /app

# 安装中文字体（2026-11-13 反风控增强）：字体列表是指纹检测点之一，
# 容器默认只有基础拉丁字体，与真实 Windows 中文环境不匹配。
# 安装常见的中文字体让 document.fonts 看起来更真实。
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
    fonts-wqy-zenhei \
    fonts-wqy-microhei \
    fonts-noto-cjk \
    fonts-noto-cjk-extra \
    fonts-liberation \
    fonts-dejavu-core \
    fonts-freefont-ttf \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY app.py /app/app.py
COPY login_sessions.py /app/login_sessions.py
COPY adapters/ /app/adapters/

# 以非 root 用户运行（playwright 官方镜像内置 pwuser）
USER pwuser

EXPOSE 18090

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "18090"]
