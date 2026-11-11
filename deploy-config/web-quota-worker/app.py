"""web-quota-worker：无状态 Playwright Sidecar 服务入口。

职责（方案 §3.2 模块 A）：
- 暴露 ``GET /healthz`` 与 ``POST /v1/probe``。
- 校验 ``X-Worker-Token`` 请求头与环境变量 ``WEB_QUOTA_WORKER_TOKEN`` 一致。
- 进程级复用单个 Chromium（首次启动受 ``asyncio.Lock`` 保护）。
- 每请求通过注入的 ``browser_context_factory`` 新建独立 ``BrowserContext``，
  适配器负责在 ``finally`` 中 ``await context.close()``。
- 按 provider 分派到 ``adapters.<provider>.Adapter``；未实现的适配器导入失败
  时返回 503 并说明 ``adapter not implemented``，绝不伪造成功。

日志纪律：绝不打印 storage_state / 密码 / cookie；只记 provider、status、elapsed_ms。
"""

import asyncio
import importlib
import json
import logging
import os
import time
from typing import Optional

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse

from adapters.base import PROVIDERS, BaseAdapter, ProbeRequest, ProbeResult

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("web-quota-worker")

# 仅记录非敏感字段，绝不打印 storage_state / 密码 / cookie。
logging.getLogger("web-quota-worker").addFilter(
    lambda record: True  # 保留默认；敏感字段由代码层永不构造进日志
)

# 单进程复用的 Chromium 浏览器实例与启动锁。
_browser = None
_browser_lock = asyncio.Lock()

# 已实例化适配器的缓存（provider -> adapter）。
_adapters: dict[str, BaseAdapter] = {}
_adapters_lock = asyncio.Lock()

app = FastAPI(title="web-quota-worker")


class AdapterNotImplemented(Exception):
    """请求的上游适配器尚未实现（模块缺失或缺少 Adapter 类）。"""

    def __init__(self, provider: str, reason: str = ""):
        self.provider = provider
        self.reason = reason
        super().__init__(f"adapter for provider '{provider}' not implemented: {reason}")


async def get_browser():
    """惰性启动并复用单个 Chromium 实例。"""
    global _browser
    if _browser is not None:
        return _browser
    async with _browser_lock:
        if _browser is None:
            from playwright.async_api import chromium

            _browser = await chromium.launch()
    return _browser


async def make_context(
    storage_state: Optional[str] = None,
    proxy_url: Optional[str] = None,
):
    """每请求独立的 BrowserContext 工厂。

    由适配器在 ``probe`` 内调用；适配器负责在 ``finally`` 中关闭。
    """
    browser = await get_browser()
    kwargs: dict = {}
    if storage_state:
        kwargs["storage_state"] = storage_state
    if proxy_url:
        kwargs["proxy"] = {"server": proxy_url}
    return await browser.new_context(**kwargs)


async def get_adapter(provider: str) -> BaseAdapter:
    """按 provider 加载并缓存对应适配器实例（注入 context 工厂）。"""
    cached = _adapters.get(provider)
    if cached is not None:
        return cached
    async with _adapters_lock:
        cached = _adapters.get(provider)
        if cached is not None:
            return cached
        try:
            module = importlib.import_module(f"adapters.{provider}")
            adapter_cls = getattr(module, "Adapter")
        except (ImportError, AttributeError) as exc:
            raise AdapterNotImplemented(provider, str(exc)) from exc
        adapter = adapter_cls(make_context)
        _adapters[provider] = adapter
        return adapter


@app.get("/healthz")
async def healthz():
    return {"ok": True}


@app.post("/v1/probe")
async def probe(request: Request):
    # 1) 鉴权：请求头 X-Worker-Token 必须与环境变量一致。
    expected_token = os.environ.get("WEB_QUOTA_WORKER_TOKEN", "")
    provided_token = request.headers.get("X-Worker-Token", "")
    if not provided_token or provided_token != expected_token:
        raise HTTPException(status_code=401, detail="unauthorized")

    # 2) 解析请求体为 ProbeRequest；字段缺失/类型错误 -> 422。
    raw_body = await request.body()
    try:
        payload = json.loads(raw_body or b"{}")
        req = ProbeRequest(**payload)
    except Exception as exc:  # noqa: BLE001 - 统一拒绝非法请求体
        raise HTTPException(status_code=422, detail=f"invalid request body: {exc}")

    # 3) provider 必须在白名单内。
    if req.provider not in PROVIDERS:
        raise HTTPException(status_code=400, detail=f"unknown provider: {req.provider}")

    start = time.monotonic()
    try:
        adapter = await get_adapter(req.provider)  # 未实现 -> AdapterNotImplemented
        result = await asyncio.wait_for(adapter.probe(req), timeout=req.timeout_seconds)
    except AdapterNotImplemented as exc:
        # 绝不伪造成功：明确告知上游适配器未实现。
        logger.warning("provider=%s status=error reason=adapter_not_implemented", req.provider)
        return JSONResponse(
            status_code=503,
            content={"error": "adapter not implemented", "provider": req.provider},
        )
    except asyncio.TimeoutError:
        elapsed_ms = int((time.monotonic() - start) * 1000)
        result = ProbeResult(status="error", provider=req.provider, error="timeout", elapsed_ms=elapsed_ms)
    except Exception as exc:  # noqa: BLE001 - 适配层异常统一降级为 error 结果
        elapsed_ms = int((time.monotonic() - start) * 1000)
        # 脱敏：只保留 safe 描述，绝不回传 storage_state / 密码。
        result = ProbeResult(
            status="error",
            provider=req.provider,
            error=f"probe failed: {type(exc).__name__}",
            elapsed_ms=elapsed_ms,
        )

    elapsed_ms = int((time.monotonic() - start) * 1000)
    result.elapsed_ms = elapsed_ms
    # 日志纪律：仅记录 provider / status / elapsed_ms。
    logger.info("provider=%s status=%s elapsed_ms=%d", req.provider, result.status, elapsed_ms)
    return result.__dict__
