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
import logging
import time

from .base import BaseAdapter, ProbeRequest, ProbeResult

_LONGCAT_ORIGIN = "https://longcat.chat"
_LONGCAT_USAGE_URL = "https://longcat.chat/platform/usage"
_PASSPORT_HOST = "passport.meituan.com"


logger = logging.getLogger("web-quota-worker.longcat")


class _SessionExpired(Exception):
    """内部信号：**明确**的未登录（401/403，或被弹回登录页），会话确实失效。"""


class _ProbeUnavailable(Exception):
    """内部信号：这一次读不到用量（404/405/5xx/业务码异常等），但**不能**据此
    判定会话失效。

    为什么必须分开（2026-11-13 线上）：旧实现把所有非 2xx 都当成「登录失效」，
    于是用户刚登成功、会话明明有效，只要某个用量接口返回 404/403 就被判死，
    会话直接被丢掉，界面提示「登录已失效，请重新登录」——用户重登一次还是同样结果。
    「读不到」和「没登录」是两件事，不能混成一个结论。
    """


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
        except _SessionExpired as exc:
            # 只记「哪个接口、什么状态」——这是判断上游到底怎么拒绝的唯一线索，
            # 不带 cookie / token。线上排查「明明登上了却提示失效」就靠这一行。
            logger.warning("longcat session rejected: %s", exc)
            return ProbeResult(
                status="needs_interactive_auth",
                provider=self.provider,
                auth_reason="session_expired",
            )
        except _ProbeUnavailable as exc:
            # 判不了：不是登录问题，交给调用方决定（Go 侧视为「无结论」放行落库）。
            # 只记路径与状态码，不含任何 cookie / token。
            logger.warning("longcat probe unavailable: %s", exc)
            return ProbeResult(
                status="error",
                provider=self.provider,
                error="probe_unavailable",
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
        # 记主机名（不带 query / cookie）：区分「被弹回登录页」与「接口拒绝」两种失效。
        if _PASSPORT_HOST in (page.url or ""):
            logger.info(
                "longcat bounced to login page host=%s", page.url.split("?", 1)[0][:80]
            )
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

        # 只有 401「未认证」才算会话失效。
        #
        # 403 不列入：它更多是权限/风控/网关拦的，和「没登录」不是一回事；
        # 把它当失效，会把刚登好的会话判死（2026-11-13 线上就是这么丢的）。
        if status == 401:
            raise _SessionExpired(f"{path} status={status}")
        # 其余非 2xx 一律是「这次读不到」：接口改路径、限流、网关 502 都可能，
        # 拿它判死会话就是把用户刚登好的东西扔掉（2026-11-13 线上）。
        if status < 200 or status >= 300:
            raise _ProbeUnavailable(f"{path} status={status}")
        # 部分接口即便返回 2xx，也会在 body 里用 code 表明登录态；同样只认 401/403。
        code = payload.get("code")
        if isinstance(code, int) and code == 401:
            raise _SessionExpired(f"{path} body code={code}")
        if isinstance(code, int) and code >= 400:
            raise _ProbeUnavailable(f"{path} body code={code}")

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
