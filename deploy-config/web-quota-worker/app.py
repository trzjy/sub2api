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
from typing import Optional, Union

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


def _is_playwright_storage_state(value: dict) -> bool:
    """判断 dict 是否满足 Playwright storageState 的最小结构约束。

    合法形态：``{"cookies": [...], "origins": [...]}``（两键均可缺省，
    但存在时必须是 list，且元素必须是 dict）。其它键允许存在
    （Playwright 忽略），但 cookies/origins 结构错误即视为无效状态。
    """
    if not isinstance(value, dict):
        return False
    for key in ("cookies", "origins"):
        if key in value:
            items = value[key]
            if not isinstance(items, list):
                return False
            if not all(isinstance(item, dict) for item in items):
                return False
    return True


def _parse_storage_state(
    storage_state: Optional[Union[str, dict]],
) -> Optional[dict]:
    """把上游传来的 storage_state 规整为 Playwright 可接受的 ``dict``。

    Playwright 的 ``browser.new_context(storage_state=...)`` 对 ``str`` 参数
    会当作**磁盘文件路径**读取（见 ``BrowserType._prepare_browser_context_params``）。
    因此绝不能把 JSON 字符串原样传下去：这里统一解析成 ``dict``，
    非法值一律丢弃（返回 ``None``），让适配器改走账密重登或
    ``needs_interactive_auth``，而不是在 ``new_context`` 阶段永久失败。
    除 JSON 语法外，还要求满足 Playwright storageState 的最小结构约束
    （见 ``_is_playwright_storage_state``）：结构非法的 dict 同样丢弃，
    否则会以 ``browser_context_init_failed`` 的形式表现为探测永久失败。

    日志纪律：失败只记固定文案，绝不打印 storage_state 原文或任何内容。
    """
    if storage_state is None:
        return None
    if isinstance(storage_state, dict):
        if _is_playwright_storage_state(storage_state):
            return storage_state
        logger.warning("invalid storage_state json, ignoring")
        return None
    if isinstance(storage_state, str):
        if not storage_state.strip():
            return None
        try:
            parsed = json.loads(storage_state)
        except (ValueError, TypeError):
            logger.warning("invalid storage_state json, ignoring")
            return None
        if isinstance(parsed, dict) and _is_playwright_storage_state(parsed):
            return parsed
        logger.warning("invalid storage_state json, ignoring")
        return None
    # 其它类型（bytes / list 等）同样不使用，避免误当作文件路径。
    logger.warning("invalid storage_state json, ignoring")
    return None


async def make_context(
    storage_state: Optional[Union[str, dict]] = None,
    proxy_url: Optional[str] = None,
):
    """每请求独立的 BrowserContext 工厂。

    由适配器在 ``probe`` 内调用；适配器负责在 ``finally`` 中关闭。
    ``storage_state`` 可以是 Playwright storageState 的 JSON 字符串或 ``dict``；
    JSON 字符串会先被解析为 ``dict`` 再传给 ``browser.new_context``，
    因为 Playwright 会把裸 ``str`` 当成磁盘文件路径。
    """
    browser = await get_browser()
    kwargs: dict = {}
    parsed_state = _parse_storage_state(storage_state)
    if parsed_state is not None:
        kwargs["storage_state"] = parsed_state
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
