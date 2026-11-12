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

# 动作完成后的沉淀时间（秒）：点按钮 / 填字段之后，页面往往还要再渲染一拍
# （美团 passport 切到短信表单要几百毫秒）。等不及就回一帧空画面，
# 用户会以为「点了没反应」。
LOGIN_ACTION_SETTLE_SECONDS = 0.9

# 仅记录非敏感字段，绝不打印 storage_state / 密码 / cookie。
logging.getLogger("web-quota-worker").addFilter(
    lambda record: True  # 保留默认；敏感字段由代码层永不构造进日志
)

# 单进程复用的 Chromium 浏览器实例与启动锁。
_browser = None
_browser_lock = asyncio.Lock()
# async_playwright().start() 返回的驱动对象；必须与 browser 同生命周期，
# 提前停止会让 Browser/Context 全部失效。
_playwright = None

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
    """惰性启动并复用单个 Chromium 实例。

    注意：``playwright.async_api`` 并不导出 ``chromium``（只有 ``async_playwright``）。
    此前这里是 ``from playwright.async_api import chromium``，任何调用都会
    ImportError —— 也就是说整条采集链路在真正启动浏览器这一步必然失败，
    只是过去没有请求走到这里（2026-11-12 登录接口上线后暴露）。
    正确入口是 ``async_playwright().start()`` 之后取 ``.chromium``。
    """
    global _browser, _playwright
    if _browser is not None:
        return _browser
    async with _browser_lock:
        if _browser is None:
            from playwright.async_api import async_playwright

            if _playwright is None:
                _playwright = await async_playwright().start()
            _browser = await _playwright.chromium.launch()
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
    viewport: Optional[dict] = None,
):
    """每请求独立的 BrowserContext 工厂。

    由适配器在 ``probe`` 内调用；适配器负责在 ``finally`` 中关闭。
    ``storage_state`` 可以是 Playwright storageState 的 JSON 字符串或 ``dict``；
    JSON 字符串会先被解析为 ``dict`` 再传给 ``browser.new_context``，
    因为 Playwright 会把裸 ``str`` 当成磁盘文件路径。

    ``viewport`` 仅登录会话使用（固定尺寸才能把前端点击坐标等比换算）；
    探测路径不传，保持浏览器默认尺寸，行为不变。
    """
    browser = await get_browser()
    kwargs: dict = {}
    parsed_state = _parse_storage_state(storage_state)
    if parsed_state is not None:
        kwargs["storage_state"] = parsed_state
    if proxy_url:
        kwargs["proxy"] = {"server": proxy_url}
    if viewport:
        kwargs["viewport"] = viewport
        # 截图像素必须与 CSS 像素一致，否则前端坐标会偏移。
        kwargs["device_scale_factor"] = 1
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


# ---------------------------------------------------------------------------
# 服务端登录会话（扫码 / 短信 / 滑块）
#
# 为什么要这一层：longcat 只有扫码与短信登录，compshare / sensenova 的默认页
# 也是短信登录。它们都必须由真人完成，worker 无法静默登录。既有实现把这件事
# 推给用户本机的 local_captcha_helper；本层把它搬回服务端——worker 打开登录页，
# 管理端网页显示画面，点击 / 输入 / 回车全部转发回同一个页面上下文，登录成功后
# 一次性回传 storageState。
#
# 纪律：不写任何站点 selector（上游改版无需改本层），会话只存内存，
# 日志只记 session 前缀与 provider，绝不打印画面与 cookie。
# ---------------------------------------------------------------------------

from login_sessions import (  # noqa: E402  (路由段集中在此，便于审阅)
    LOGIN_PHASE_OPENING,
    LOGIN_PHASE_READY,
    LOGIN_SESSION_TTL_SECONDS,
    LOGIN_VIEWPORT_HEIGHT,
    LOGIN_VIEWPORT_WIDTH,
    LoginSessionError,
    LoginSessionRegistry,
    capture,
    click_button,
    describe_form,
    fill_field,
    press_key,
    reload as reload_login_page,
    scroll as scroll_login_page,
    snapshot_storage_state,
    tap as tap_login_page,
    toggle_field,
    type_text,
)

_login_registry = LoginSessionRegistry(make_context)


def _require_token(request: Request) -> None:
    expected_token = os.environ.get("WEB_QUOTA_WORKER_TOKEN", "")
    provided_token = request.headers.get("X-Worker-Token", "")
    if not provided_token or provided_token != expected_token:
        raise HTTPException(status_code=401, detail="unauthorized")


async def _json_body(request: Request) -> dict:
    raw_body = await request.body()
    try:
        payload = json.loads(raw_body or b"{}")
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=422, detail=f"invalid request body: {exc}")
    if not isinstance(payload, dict):
        raise HTTPException(status_code=422, detail="request body must be an object")
    return payload


def _session_error_response(exc: LoginSessionError) -> JSONResponse:
    status = 404 if exc.code == "session_not_found" else 410
    return JSONResponse(status_code=status, content={"error": exc.code})


def _login_view(session) -> dict:
    """会话对外视图：不含 cookie，不含 storageState。"""
    return {
        "session_id": session.id,
        "provider": session.provider,
        "status": "authenticated" if session.authenticated() else "pending",
        # phase 是页面生命周期（opening/ready/failed），与 status（是否已登录）
        # 是两个独立事实：一个说「页面能不能看」，一个说「人有没有登进去」。
        "phase": session.phase,
        "message": session.phase_error,
        "url": session.page.url,
        "viewport_width": LOGIN_VIEWPORT_WIDTH,
        "viewport_height": LOGIN_VIEWPORT_HEIGHT,
        "expires_in": LOGIN_SESSION_TTL_SECONDS,
    }


async def _login_view_with_form(session) -> dict:
    """在会话视图上附带当前页面的输入框 / 按钮清单。

    前端的表单区直接按这两份清单渲染，用户填真表单即可，不必去点缩略图坐标。
    页面还没打开时不做枚举：那 3 秒超时对用户毫无价值，还会拖慢首帧。
    """
    view = _login_view(session)
    if session.phase != LOGIN_PHASE_READY:
        view["fields"] = []
        view["buttons"] = []
        return view
    view.update(await describe_form(session))
    return view


@app.post("/v1/login/start")
async def login_start(request: Request):
    _require_token(request)
    payload = await _json_body(request)
    provider = str(payload.get("provider") or "")
    if provider not in PROVIDERS:
        raise HTTPException(status_code=400, detail=f"unknown provider: {provider}")
    login_url = str(payload.get("login_url") or "").strip()
    if not login_url:
        raise HTTPException(status_code=400, detail="login_url is required")
    success_url_prefix = str(payload.get("success_url_prefix") or "").strip()

    try:
        session = await _login_registry.start(
            provider=provider,
            login_url=login_url,
            success_url_prefix=success_url_prefix,
            proxy_url=payload.get("proxy_url") or None,
        )
    except Exception as exc:  # noqa: BLE001 - 统一降级，不泄漏内部细节
        logger.warning("login provider=%s status=start_failed error=%s", provider, type(exc).__name__)
        return JSONResponse(status_code=502, content={"error": "login_session_start_failed"})

    # 立刻返回会话号：页面在后台打开，前端靠 phase 显示「正在打开登录页…」。
    # 这里绝不能等页面打开——最慢要几十秒，会把前端请求拖过超时阈值，
    # 用户看到的是「网络错误」，而且拿不到会话号，登录面板永远不出现。
    view = await _login_view_with_form(session)
    logger.info("login provider=%s status=started phase=%s", provider, session.phase)
    return view


@app.get("/v1/login/{session_id}/state")
async def login_state(session_id: str, request: Request):
    _require_token(request)
    try:
        session = await _login_registry.get(session_id)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    try:
        # 还在打开页面时不做截图：截到的只会是空白页，白花一次截图时间。
        screenshot = "" if session.phase == LOGIN_PHASE_OPENING else await capture(session)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=frame_failed error=%s", type(exc).__name__)
        return JSONResponse(status_code=502, content={"error": "login_frame_failed"})

    view = await _login_view_with_form(session)
    view["screenshot"] = screenshot
    return view


@app.post("/v1/login/{session_id}/tap")
async def login_tap(session_id: str, request: Request):
    _require_token(request)
    payload = await _json_body(request)
    try:
        x = float(payload.get("x"))
        y = float(payload.get("y"))
    except (TypeError, ValueError):
        raise HTTPException(status_code=422, detail="x and y must be numbers")
    try:
        session = await _login_registry.get(session_id)
        await tap_login_page(session, x, y)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    return {"ok": True}


@app.post("/v1/login/{session_id}/type")
async def login_type(session_id: str, request: Request):
    _require_token(request)
    payload = await _json_body(request)
    text = payload.get("text")
    if not isinstance(text, str) or len(text) > 512:
        raise HTTPException(status_code=422, detail="text must be a string under 512 chars")
    try:
        session = await _login_registry.get(session_id)
        await type_text(session, text)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    return {"ok": True}


@app.post("/v1/login/{session_id}/fill")
async def login_fill(session_id: str, request: Request):
    """把文字填进页面上的某个输入框（按 label 定位，不依赖坐标）。

    返回刷新后的整帧，前端据此立刻更新表单与画面，不必等下一次轮询。
    """
    _require_token(request)
    payload = await _json_body(request)
    text = payload.get("text")
    if not isinstance(text, str) or len(text) > 512:
        raise HTTPException(status_code=422, detail="text must be a string under 512 chars")
    label = str(payload.get("label") or "")
    try:
        index = int(payload.get("index") or 0)
    except (TypeError, ValueError):
        raise HTTPException(status_code=422, detail="index must be an integer")

    applied = False
    try:
        session = await _login_registry.get(session_id)
        applied = await fill_field(session, index, text, label)
        if applied:
            # 填完稍等一拍：框架要把值回写到 UI，立刻截图会拍到旧状态。
            await asyncio.sleep(LOGIN_ACTION_SETTLE_SECONDS)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    except Exception as exc:  # noqa: BLE001 - 不把内部异常抛给前端
        logger.warning("login status=fill_failed error=%s", type(exc).__name__)
    logger.info("login status=fill applied=%s", applied)

    try:
        view = await _login_view_with_form(session)
        view["screenshot"] = await capture(session)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=frame_failed error=%s", type(exc).__name__)
        return JSONResponse(status_code=502, content={"error": "login_frame_failed"})
    view["applied"] = applied
    return view


@app.post("/v1/login/{session_id}/click")
async def login_click(session_id: str, request: Request):
    """点击页面上的某个按钮（按按钮文字定位）。"""
    _require_token(request)
    payload = await _json_body(request)
    text = str(payload.get("text") or "")
    try:
        index = int(payload.get("index") or 0)
    except (TypeError, ValueError):
        raise HTTPException(status_code=422, detail="index must be an integer")

    applied = False
    try:
        session = await _login_registry.get(session_id)
        applied = await click_button(session, index, text)
        if applied:
            # 点完要等页面完成切换：美团 passport 切到短信表单有明显延迟，
            # 等不及就回一帧「没有任何输入框」，用户会以为点坏了。
            await asyncio.sleep(LOGIN_ACTION_SETTLE_SECONDS)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=click_failed error=%s", type(exc).__name__)
    logger.info("login status=click applied=%s", applied)

    try:
        view = await _login_view_with_form(session)
        view["screenshot"] = await capture(session)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=frame_failed error=%s", type(exc).__name__)
        return JSONResponse(status_code=502, content={"error": "login_frame_failed"})
    view["applied"] = applied
    return view


@app.post("/v1/login/{session_id}/toggle")
async def login_toggle(session_id: str, request: Request):
    """勾选 / 取消勾选页面上的勾选项（如「我已阅读并同意用户协议」）。

    这一项漏了会直接把登录拦死：点登录只弹一句「请先阅读并勾选用户协议」。
    页面上这个勾选框通常是个十几像素的小圆圈，缩到面板里更小，点不中。
    """
    _require_token(request)
    payload = await _json_body(request)
    label = str(payload.get("label") or "")
    try:
        index = int(payload.get("index") or 0)
    except (TypeError, ValueError):
        raise HTTPException(status_code=422, detail="index must be an integer")

    applied = False
    try:
        session = await _login_registry.get(session_id)
        applied = await toggle_field(session, index, label)
        if applied:
            await asyncio.sleep(LOGIN_ACTION_SETTLE_SECONDS)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=toggle_failed error=%s", type(exc).__name__)
    logger.info("login status=toggle applied=%s", applied)

    try:
        view = await _login_view_with_form(session)
        view["screenshot"] = await capture(session)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=frame_failed error=%s", type(exc).__name__)
        return JSONResponse(status_code=502, content={"error": "login_frame_failed"})
    view["applied"] = applied
    return view


@app.post("/v1/login/{session_id}/key")
async def login_key(session_id: str, request: Request):
    _require_token(request)
    payload = await _json_body(request)
    key = str(payload.get("key") or "")
    try:
        session = await _login_registry.get(session_id)
        await press_key(session, key)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    return {"ok": True}


@app.post("/v1/login/{session_id}/scroll")
async def login_scroll(session_id: str, request: Request):
    _require_token(request)
    payload = await _json_body(request)
    try:
        delta_y = float(payload.get("delta_y"))
    except (TypeError, ValueError):
        raise HTTPException(status_code=422, detail="delta_y must be a number")
    try:
        session = await _login_registry.get(session_id)
        await scroll_login_page(session, delta_y)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    return {"ok": True}


@app.post("/v1/login/{session_id}/reload")
async def login_reload(session_id: str, request: Request):
    _require_token(request)
    try:
        session = await _login_registry.get(session_id)
        await reload_login_page(session)
    except LoginSessionError as exc:
        return _session_error_response(exc)
    return {"ok": True}


@app.post("/v1/login/{session_id}/finish")
async def login_finish(session_id: str, request: Request):
    """登录成功后回传 storageState。

    未登录（仍在登录页）时返回 409 + status=pending，由前端继续等待；
    绝不伪造成功——这是本链路唯一允许产生凭据的出口。
    """
    _require_token(request)
    try:
        session = await _login_registry.get(session_id)
    except LoginSessionError as exc:
        return _session_error_response(exc)

    if not session.authenticated():
        return JSONResponse(
            status_code=409,
            content={"status": "pending", "error": "not_authenticated"},
        )

    try:
        storage_state = await snapshot_storage_state(session)
    except Exception as exc:  # noqa: BLE001
        logger.warning("login status=storage_state_failed error=%s", type(exc).__name__)
        return JSONResponse(status_code=502, content={"error": "storage_state_failed"})

    label = ""
    try:
        label = (await session.page.title() or "").strip()[:120]
    except Exception:  # noqa: BLE001
        label = ""

    logger.info("login provider=%s status=finished", session.provider)
    await _login_registry.close(session_id)
    return {"status": "authenticated", "storage_state": storage_state, "account_label": label}


@app.delete("/v1/login/{session_id}")
async def login_close(session_id: str, request: Request):
    _require_token(request)
    await _login_registry.close(session_id)
    return {"ok": True}
