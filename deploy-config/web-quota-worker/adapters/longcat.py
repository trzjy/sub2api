"""美团 LongCat 控制台用量抓取适配器。

本模块实现 ``web-quota-worker`` 的 LongCat 上游适配器，严格遵循
``adapters/base.py`` 定义的 ``BaseAdapter`` 契约与方案文档 §2.2-3 / §4.3。

设计纪律（与方案 §3.1 一致）：
- worker 无状态：不落盘、不打印任何凭据（cookie / storage_state 等敏感值）。
- 美团 LongCat 控制台仅提供扫码 / 短信登录，无账号密码登录入口；
  适配器**绝不做任何自动登录尝试**，一旦检测到被重定向到 passport 登录页，
  立即返回 ``needs_interactive_auth`` 且不长时间等待。
- 不引入任何第三方依赖（仅依赖 base.py 与标准库）。
"""

import asyncio
import json
import time

from .base import BaseAdapter, ProbeRequest, ProbeResult

_LONGCAT_ORIGIN = "https://longcat.chat"
_LONGCAT_USAGE_URL = "https://longcat.chat/platform/usage"
_PASSPORT_HOST = "passport.meituan.com"


class _SessionExpired(Exception):
    """内部信号：同源用量接口返回未登录 / 非 2xx，表明会话失效。"""


class LongcatAdapter(BaseAdapter):
    """美团 LongCat 用量探测器。

    通过注入的 ``browser_context_factory`` 获取每请求独立的 Playwright
    ``BrowserContext``，在同一个已登录页面上下文内发起同源用量请求，
    解析后回传结构化用量与刷新后的 storageState。
    """

    provider = "longcat"

    async def probe(self, req: ProbeRequest) -> ProbeResult:
        start = time.monotonic()
        context = None
        try:
            context = await self._browser_context_factory(
                req.storage_state, req.proxy_url
            )
            try:
                result = await asyncio.wait_for(
                    self._run(req, context),
                    timeout=req.timeout_seconds,
                )
            except asyncio.TimeoutError:
                result = ProbeResult(
                    status="error",
                    provider=self.provider,
                    error="timeout",
                )
            return result
        except _SessionExpired:
            return ProbeResult(
                status="needs_interactive_auth",
                provider=self.provider,
                auth_reason="session_expired",
            )
        except Exception:
            # 脱敏：不向外暴露任何内部异常细节或凭据。
            return ProbeResult(
                status="error",
                provider=self.provider,
                error="probe_failed",
            )
        finally:
            if context is not None:
                await context.close()

    async def _run(self, req: ProbeRequest, context) -> ProbeResult:
        page = await context.new_page()
        target_url = req.console_login_url or _LONGCAT_USAGE_URL
        await page.goto(
            target_url,
            wait_until="domcontentloaded",
            timeout=req.timeout_seconds * 1000,
        )

        # 被重定向到美团 passport 登录页：仅扫码 / 短信登录，绝不做自动登录。
        if _PASSPORT_HOST in (page.url or ""):
            return ProbeResult(
                status="needs_interactive_auth",
                provider=self.provider,
                auth_reason="interactive_login_required",
            )

        token_packs = await self._api_json(
            page, "POST", "/api/pay/quota/metering/token-packs/summary", {}
        )
        api_usage = await self._api_json(
            page, "POST", "/api/pay/quota/metering/api-usage/summary", {}
        )
        user_current = await self._api_json(page, "GET", "/api/v1/user-current", None)

        data = self._build_data(token_packs, api_usage, user_current)
        account_label = self._account_label(user_current)

        updated_storage_state = json.dumps(await context.storage_state())

        return ProbeResult(
            status="ok",
            provider=self.provider,
            updated_storage_state=updated_storage_state,
            account_label=account_label,
            data=data,
        )

    async def _api_json(self, page, method: str, path: str, body):
        """在同一个页面上下文内发起同源请求并解析 JSON。

        非 2xx 或响应体明确表明登录失效时抛出 ``_SessionExpired``。
        """
        headers = {
            "Content-Type": "application/json",
            "Origin": _LONGCAT_ORIGIN,
            "Referer": _LONGCAT_USAGE_URL,
        }
        url = _LONGCAT_ORIGIN + path
        if method == "POST":
            resp = await page.request.post(url, data=json.dumps(body), headers=headers)
        else:
            resp = await page.request.get(url, headers=headers)

        status = getattr(resp, "status", 0)
        try:
            payload = await resp.json()
        except Exception:
            payload = {}

        if not isinstance(payload, dict):
            payload = {}

        if status < 200 or status >= 300:
            raise _SessionExpired(f"non-2xx status={status}")
        # 部分接口即便返回 2xx，也会在 body 中以 code 字段表明登录失效。
        code = payload.get("code")
        if isinstance(code, int) and code >= 400:
            raise _SessionExpired(f"body code={code}")

        return payload

    def _build_data(self, token_packs, api_usage, user_current) -> dict:
        data: dict = {}
        lots = self._build_lots(token_packs)
        if lots:
            data["lots"] = lots
        paygo = self._build_paygo(api_usage)
        data.update(paygo)
        return data

    def _build_lots(self, token_packs) -> list:
        if not isinstance(token_packs, dict):
            return []
        d = token_packs.get("data", {})
        if not isinstance(d, dict):
            d = {}

        merged = []
        current = d.get("currentLot")
        if isinstance(current, dict):
            merged.append((current, True))
        other = d.get("otherLots")
        if isinstance(other, list):
            for lot in other:
                if isinstance(lot, dict):
                    merged.append((lot, False))

        records = []
        for lot, is_current in merged:
            if lot.get("status") != "ACTIVE":
                continue
            rec = {}
            lot_id = lot.get("id", lot.get("lotId"))
            if lot_id is not None:
                rec["lot_id"] = lot_id
            if "status" in lot:
                rec["status"] = lot["status"]
            if "remainingToken" in lot:
                rec["remaining_token"] = lot["remainingToken"]
            if "totalToken" in lot:
                rec["total_token"] = lot["totalToken"]
            if "expireTime" in lot:
                rec["expire_time"] = lot["expireTime"]
            rec["is_current"] = is_current
            records.append(rec)
        return records

    def _build_paygo(self, api_usage) -> dict:
        if not isinstance(api_usage, dict):
            return {}
        d = api_usage.get("data", {})
        if not isinstance(d, dict):
            d = {}

        paygo: dict = {}
        if "paygoBalanceCent" in d:
            paygo["paygo_balance_cent"] = d["paygoBalanceCent"]
        if "paygoStatus" in d:
            paygo["paygo_status"] = d["paygoStatus"]

        primary = (d.get("paygoBalance") or {}).get("primary") or {}
        if isinstance(primary, dict):
            if "currency" in primary:
                paygo["paygo_currency"] = primary["currency"]
            if "amount" in primary:
                paygo["paygo_amount"] = primary["amount"]
        return paygo

    def _account_label(self, user_current) -> str:
        if not isinstance(user_current, dict):
            return ""
        d = user_current.get("data", user_current)
        if not isinstance(d, dict):
            return ""
        val = d.get("phone")
        if val in (None, ""):
            val = d.get("userId")
        if val in (None, ""):
            return ""
        return str(val)
