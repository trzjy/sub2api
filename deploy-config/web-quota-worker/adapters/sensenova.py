"""商汤日日新（SenseNova）控制台用量抓取适配器。

实现方案 ``docs-local/web-console-quota-playwright-plan.md`` §2.2-2 与 §4.2 中
``sensenova`` 上游的 Playwright 无状态采集逻辑。

纪律（与 CARD-WCQ-06 一致）：
- 不打印密码 / token / cookie / storage_state；
- 解析异常一律转 ``error``，绝不抛出；
- 不引入任何第三方依赖（仅使用标准库 + Playwright 运行时）。

本模块只通过 ``browser_context_factory`` 获取每请求独立的 ``BrowserContext``，
不接触任何明文推理 ``api_key``，不落盘任何凭据。
"""

import asyncio
import json
import time

from .base import BaseAdapter, ProbeRequest, ProbeResult

# 控制台页（未登录会重定向到 /login）。
CONSOLE_URL = "https://platform.sensenova.cn/console"
# 用量接口（带 Bearer access_token）。
POOL_USAGE_URL = "https://platform.sensenova.cn/lite/console/v1/tokenplan/pool-usage"
# 账号信息接口（best-effort，拿不到留空字符串）。
USER_CURRENT_URL = "https://platform.sensenova.cn/lite/console/v1/user-current"

# 上游响应里我们识别的字段名。原样透传，绝不静默改名；
# 不在该集合内的其它实际键名也一并原样透传。
_RECOGNIZED_POOL_KEYS = (
    "id",
    "pool_type",
    "window_5h",
    "window_7d",
    "grant_balance",
    "nearest_grant_expiring_balance",
    "nearest_grant_expiry",
)


def _now_ms() -> int:
    return int(time.time() * 1000)


def parse_pool_response(body):
    """将上游用量响应规整为严格结构 ``{"pools": [...]}``。

    规则（派发单 CARD-WCQ-07 §5）：
    - 上游字段名以实际响应为准，原样透传，不得静默改名；
    - 缺失字段省略，不填 0 冒充；
    - 支持 ``{pools:[...]}`` 与 ``{data:{pools:[...]}}`` / ``{data:[...]}`` 两种包装。
    """
    pools = None
    if isinstance(body, dict):
        if isinstance(body.get("pools"), list):
            pools = body["pools"]
        elif isinstance(body.get("data"), dict) and isinstance(
            body["data"].get("pools"), list
        ):
            pools = body["data"]["pools"]
        elif isinstance(body.get("data"), list):
            pools = body["data"]
    if pools is None:
        pools = []

    out_pools = []
    for pool in pools:
        if not isinstance(pool, dict):
            continue
        out = {}
        for key in _RECOGNIZED_POOL_KEYS:
            if key in pool:
                out[key] = pool[key]
        # 其余实际键名原样透传，不重命名（缺失字段自然省略）。
        for key, value in pool.items():
            if key not in out:
                out[key] = value
        out_pools.append(out)
    return {"pools": out_pools}


class SensenovaAdapter(BaseAdapter):
    """商汤日日新控制台用量适配器。"""

    provider = "sensenova"

    async def probe(self, req: ProbeRequest) -> ProbeResult:
        started = _now_ms()
        result = ProbeResult(
            status="error", provider=self.provider, error="unexpected"
        )
        context = None

        async def _run():
            nonlocal context
            context = await self._browser_context_factory(
                req.storage_state, req.proxy_url
            )
            page = await context.new_page()

            redirected = await self._navigate_console(page, req)
            if redirected:
                if not (req.console_username and req.console_password):
                    result.status = "needs_interactive_auth"
                    result.auth_reason = "login_required"
                    return result
                outcome = await self._do_login(page, req, req.timeout_seconds)
                if outcome == "password_rejected":
                    result.status = "auth_failed"
                    result.auth_reason = "password_rejected"
                    return result
                if outcome == "sms_or_captcha_required":
                    result.status = "needs_interactive_auth"
                    result.auth_reason = "sms_or_captcha_required"
                    return result
                # outcome == "ok"：继续后续用量采集。

            access_token = await self._get_access_token(page)
            if not access_token:
                result.status = "needs_interactive_auth"
                result.auth_reason = "login_required"
                return result

            status_code, body = await self._fetch_pool_usage(page, access_token)
            if status_code in (401, 403):
                result.status = "auth_failed"
                result.auth_reason = "token_rejected"
                return result
            if status_code is None or status_code < 200 or status_code >= 300:
                result.status = "error"
                result.error = "upstream_http_error"
                return result

            result.data = parse_pool_response(body or {})
            result.status = "ok"
            result.error = ""
            result.auth_reason = ""
            result.account_label = await self._extract_account_label(page)
            result.updated_storage_state = await self._context_storage_state(context)
            return result

        try:
            result = await asyncio.wait_for(_run(), timeout=req.timeout_seconds)
        except asyncio.TimeoutError:
            result.status = "error"
            result.error = "timeout"
        except Exception:
            # 解析/任何意外异常一律转 error，绝不抛出。
            result.status = "error"
            result.error = "parse_or_unexpected_error"
        finally:
            if context is not None:
                try:
                    await context.close()
                except Exception:
                    pass
            result.elapsed_ms = _now_ms() - started
        return result

    # ---- 以下为可被子类/测试重写的页面交互原语（薄封装，便于离线 mock）----

    async def _navigate_console(self, page, req: ProbeRequest) -> bool:
        """打开控制台页，返回是否被重定向到 /login。"""
        url = req.console_login_url or CONSOLE_URL
        await page.goto(url)
        return "/login" in (page.url or "")

    async def _do_login(self, page, req: ProbeRequest, timeout: int) -> str:
        """执行密码登录，返回 'ok' | 'password_rejected' | 'sms_or_captcha_required'。"""
        await page.click('[role="tab"]:has-text("密码登录")')
        await page.fill("#login-username", req.console_username)
        await page.fill("#login-password", req.console_password)
        await page.press("#login-password", "Enter")
        return await self._detect_login_outcome(page, timeout)

    async def _detect_login_outcome(self, page, timeout: int) -> str:
        deadline = time.monotonic() + max(timeout, 0)
        while time.monotonic() < deadline:
            token = await self._get_access_token(page)
            if token:
                return "ok"
            text = await self._page_text(page)
            if "账号或密码错误" in text or "IncorrectPassword" in text:
                return "password_rejected"
            # 仍在 /login 且无错误文案：可能等待短信/滑块等交互验证。
            await asyncio.sleep(0.05)
        # 超时仍在登录页：判定为需要交互验证。
        return "sms_or_captcha_required"

    async def _get_access_token(self, page) -> "str | None":
        try:
            val = await page.evaluate("localStorage.getItem('access_token')")
            if val:
                return val
        except Exception:
            pass
        return None

    async def _page_text(self, page) -> str:
        try:
            text = await page.evaluate(
                "document && document.body && document.body.innerText "
                "? document.body.innerText : ''"
            )
            return text or ""
        except Exception:
            return ""

    async def _fetch_pool_usage(self, page, token) -> "tuple[int | None, object]":
        """在页面上下文内请求用量接口，返回 (status_code, body)。"""
        script = (
            "async (token) => {"
            "  try {"
            "    const resp = await fetch("
            "      '" + POOL_USAGE_URL + "',"
            "      {headers: {'Authorization': 'Bearer ' + token, "
            "'Accept': 'application/json'}}"
            "    );"
            "    let body = null;"
            "    try { body = await resp.json(); } catch (e) { body = null; }"
            "    return {status: resp.status, body: body};"
            "  } catch (e) {"
            "    return {status: null, body: null};"
            "  }"
            "}"
        )
        try:
            res = await page.evaluate(script, token)
        except Exception:
            return (None, None)
        if not isinstance(res, dict):
            return (None, None)
        return (res.get("status"), res.get("body"))

    async def _extract_account_label(self, page) -> str:
        """尽力获取可见账号名 / user-current 返回的 phone / userName。"""
        try:
            script = (
                "async () => {"
                "  try {"
                "    const r = await fetch('" + USER_CURRENT_URL + "', "
                "{'headers': {'Accept': 'application/json'}});"
                "    if (!r.ok) return '';"
                "    const j = await r.json();"
                "    const d = (j && typeof j === 'object' && j.data) ? j.data : (j || {});"
                "    return d.phone || d.userName || '';"
                "  } catch (e) { return ''; }"
                "}"
            )
            val = await page.evaluate(script)
            if val:
                return val
        except Exception:
            pass
        return ""

    async def _context_storage_state(self, context) -> "str | None":
        try:
            state = await context.storage_state()
            if state is not None:
                return json.dumps(state, ensure_ascii=False)
        except Exception:
            pass
        return None
