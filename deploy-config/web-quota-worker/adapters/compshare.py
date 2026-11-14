"""优云智算（compshare）控制台用量抓取适配器。

本模块实现 ``BaseAdapter`` 契约（见 ``adapters/base.py``），
用于采集 compshare 控制台的套餐用量。

纪律（与方案 §3.1 / §4.1 一致）：
- worker 无状态：不落盘任何凭据（storage_state / 密码 / cookie 仅在内存）。
- 只接收 ``api_key_sha256``，绝不接收明文 api_key；结果中也只回传 APIKey 的
  SHA-256 哈希，绝不把明文 APIKey 放进结果或日志。
- 任何解析异常都转换为 ``status="error"`` 的脱敏描述，绝不抛出未捕获异常。
- 绝不打印 ``console_password`` / cookie / ``storage_state`` / 明文 APIKey。
"""

import asyncio
import hashlib
import json
import time
from urllib.parse import unquote

from .base import BaseAdapter, ProbeRequest, ProbeResult

_API_URL = "https://api.compshare.cn/?Action=GetOpenAPIUserPlans"
_ORIGIN = "https://console.compshare.cn"
_REFERER = "https://console.compshare.cn/light-gpu/model-manage"
_LOGIN_URL = "https://passport.compshare.cn/login"
_CONSOLE_HOST = "console.compshare.cn"
_JWT_COOKIE = "U_JWT_TOKEN"
_EMAIL_COOKIE = "U_USER_EMAIL"

# 登录后等待跳回控制台的最长时间（秒）。
_LOGIN_WAIT_SECONDS = 10


class _TeamScopeUnavailable(Exception):
    """个人侧与团队侧都没拿到套餐数据：不能用空结果判定「没有套餐」。"""


class CompshareAdapter(BaseAdapter):
    """compshare 上游适配器。"""

    provider = "compshare"

    async def probe(self, req: ProbeRequest) -> ProbeResult:
        started = time.monotonic()
        context = None
        try:
            try:
                context = await self._browser_context_factory(
                    req.storage_state, req.proxy_url
                )
            except Exception:
                return ProbeResult(
                    status="error",
                    provider=self.provider,
                    error="browser_context_init_failed",
                )

            try:
                result = await asyncio.wait_for(
                    self._probe_inner(req, context),
                    timeout=req.timeout_seconds,
                )
            except (asyncio.TimeoutError, TimeoutError):
                return ProbeResult(
                    status="error", provider=self.provider, error="timeout"
                )
            except Exception:
                return ProbeResult(
                    status="error", provider=self.provider, error="probe_failed"
                )

            result.elapsed_ms = int((time.monotonic() - started) * 1000)
            return result
        finally:
            if context is not None:
                try:
                    await context.close()
                except Exception:
                    pass

    async def _probe_inner(self, req: ProbeRequest, context) -> ProbeResult:
        try:
            ok, payload = await self._try_direct_fetch(context)
        except _TeamScopeUnavailable:
            return ProbeResult(
                status="error", provider=self.provider, error="plans_scope_unavailable"
            )
        if ok:
            return await self._build_ok(context, payload)

        # 直接取用量失败（401/403/RetCode!=0/异常），按需尝试无头登录。
        if req.console_username and req.console_password:
            outcome = await self._do_login(req, context)
            if outcome == "captcha":
                return ProbeResult(
                    status="needs_interactive_auth",
                    provider=self.provider,
                    auth_reason="captcha_required",
                )
            if outcome == "password_error":
                return ProbeResult(
                    status="auth_failed",
                    provider=self.provider,
                    auth_reason="password_rejected",
                )
            if outcome == "ok":
                try:
                    ok2, payload2 = await self._try_direct_fetch(context)
                except _TeamScopeUnavailable:
                    return ProbeResult(
                        status="error",
                        provider=self.provider,
                        error="plans_scope_unavailable",
                    )
                if ok2:
                    return await self._build_ok(context, payload2)
                return ProbeResult(
                    status="needs_interactive_auth",
                    provider=self.provider,
                    auth_reason="captcha_required",
                )

        # 未提供账密且未登录 → 需要人工交互登录。
        return ProbeResult(
            status="needs_interactive_auth",
            provider=self.provider,
            auth_reason="login_required",
        )

    async def _try_direct_fetch(self, context):
        """在页面上下文内直接请求用量接口，个人 + 团队两个作用域合并返回。

        返回 ``(ok, payload)``：``ok`` 为 True 表示成功取到含 ``UserPlans`` 的
        合法响应；否则为 False（含网络异常 / 非 200 / RetCode!=0）。

        团队套餐必须单独查（2026-11-14 线上实测）：同一个 ``GetOpenAPIUserPlans``
        带 ``IsTeam=true`` 才返回团队作用域的套餐；不带时只返回个人套餐，团队套餐
        账号会拿到空列表，被后端判成「无有效订阅」并停调账号。个人 + 团队合并后
        再交给上层判定，避免任一侧的空结果冒充「没有套餐」。

        团队作用域失败时的语义：个人侧已有数据就按个人结果判定（团队侧缺失不影响
        既有判定能力，仅记日志）；个人侧也为空则抛 ``_TeamScopeUnavailable``，
        由上层按「探测失败」处理——绝不用一次失败的空结果去停调账号。
        """
        ok, payload = await self._fetch_user_plans(context, is_team=False)
        if not ok or not isinstance(payload, dict):
            return False, None

        team_ok, team_payload = await self._fetch_user_plans(context, is_team=True)
        if not team_ok or not isinstance(team_payload, dict):
            if payload.get("UserPlans"):
                return True, payload
            raise _TeamScopeUnavailable()

        merged = list(payload.get("UserPlans") or []) + list(
            team_payload.get("UserPlans") or []
        )
        payload["UserPlans"] = merged
        return True, payload

    async def _fetch_user_plans(self, context, is_team: bool):
        """按作用域取一次套餐列表。``is_team`` 为 True 时带 IsTeam 参数。"""
        body = {
            "Action": "GetOpenAPIUserPlans",
            "_timestamp": int(time.time() * 1000),
        }
        if is_team:
            body["IsTeam"] = True
        try:
            resp = await context.request.post(
                _API_URL,
                headers={
                    "Content-Type": "application/json",
                    "Origin": _ORIGIN,
                    "Referer": _REFERER,
                },
                data=json.dumps(body),
            )
        except Exception:
            return False, None

        try:
            if getattr(resp, "status", None) != 200:
                return False, None
            payload = await resp.json()
        except Exception:
            return False, None

        if isinstance(payload, dict) and payload.get("RetCode") == 0 and (
            "UserPlans" in payload
        ):
            return True, payload
        return False, None

    async def _do_login(self, req: ProbeRequest, context) -> str:
        """尝试无头登录。

        返回以下 outcome 之一：
        - ``"ok"``：登录成功（跳回控制台且存在 U_JWT_TOKEN cookie）；
        - ``"captcha"``：出现极验/滑块或未在时限内跳回控制台；
        - ``"password_error"``：页面出现明确密码错误提示。
        """
        page = await context.new_page()
        try:
            login_url = req.console_login_url or _LOGIN_URL
            await page.goto(login_url, wait_until="domcontentloaded")
            # 切换到「账号登录」Tab。
            await page.click("text=账号登录")
            await page.fill("#username", req.console_username or "")
            await page.fill("input[type=password]", req.console_password or "")
            await page.click("text=登录")

            try:
                await page.wait_for_url(
                    f"**{_CONSOLE_HOST}/**", timeout=_LOGIN_WAIT_SECONDS * 1000
                )
            except Exception:
                pass

            try:
                content = await page.content()
            except Exception:
                content = ""

            if ("密码错误" in content) or ("账号或密码不正确" in content):
                return "password_error"

            url = getattr(page, "url", "") or ""
            if _CONSOLE_HOST in url and await self._has_jwt_cookie(context):
                return "ok"

            # 出现极验/滑块元素，或仍未跳回控制台 → 视为需要交互式验证。
            return "captcha"
        finally:
            try:
                await page.close()
            except Exception:
                pass

    async def _has_jwt_cookie(self, context) -> bool:
        try:
            cookies = await context.cookies()
        except Exception:
            return False
        if not isinstance(cookies, list):
            return False
        for c in cookies:
            if not isinstance(c, dict):
                continue
            name = c.get("name", "")
            domain = c.get("domain", "")
            if name == _JWT_COOKIE and ".compshare.cn" in domain:
                return True
        return False

    async def _build_ok(self, context, payload) -> ProbeResult:
        try:
            plans = payload.get("UserPlans") or []
            out_plans = [self._parse_plan(p) for p in plans if isinstance(p, dict)]
            data = {"plans": out_plans}
            storage_state = json.dumps(await context.storage_state())
            account_label = await self._account_label(context, payload)
            return ProbeResult(
                status="ok",
                provider=self.provider,
                updated_storage_state=storage_state,
                account_label=account_label,
                data=data,
            )
        except Exception:
            return ProbeResult(
                status="error", provider=self.provider, error="parse_payload_failed"
            )

    def _parse_plan(self, p: dict) -> dict:
        """将单个 UserPlan 映射为脱敏后的结构化用量对象。

        数值字段仅在响应中存在且非 None 时写入；缺失则省略（绝不填 0 冒充）。
        APIKey 仅以 SHA-256 哈希形式出现在 ``key_hashes``，明文永不进入结果。
        """
        obj: dict = {}

        def pick(*keys, default=None):
            for k in keys:
                if k in p and p[k] is not None:
                    return p[k]
            return default

        code = pick("Code", "code")
        if code is not None:
            obj["code"] = code

        plan_name = pick("PlanName", "plan_name")
        if plan_name is not None:
            obj["plan_name"] = plan_name

        plan_status = pick("Status", "plan_status")
        if plan_status is not None:
            obj["plan_status"] = plan_status

        # 个人套餐与团队套餐共用同一结构，靠 IsTeam 区分（2026-11-14）。
        is_team = pick("IsTeam", "is_team")
        if is_team is not None:
            obj["is_team"] = bool(is_team)

        expire_at = pick("ExpireAt", "expire_at")
        if expire_at is not None:
            obj["expire_at"] = expire_at

        def num(field_out, *src):
            v = pick(*src)
            if v is not None:
                obj[field_out] = v

        num("limit_5h", "LimitPer5h", "limit_5h")
        num("used_5h", "UsagePer5h", "used_5h")
        num("reset_5h", "UsagePer5hNextResetAt", "reset_5h")
        num("limit_week", "LimitPerWeek", "limit_week")
        num("used_week", "UsagePerWeek", "used_week")
        num("reset_week", "UsagePerWeekNextResetAt", "reset_week")
        num("limit_month", "LimitPerMonth", "limit_month")
        num("used_month", "UsagePerMonth", "used_month")
        num("reset_month", "UsagePerMonthNextResetAt", "reset_month")

        key_hashes = []
        keys = pick("Keys", "api_keys", default=[])
        if isinstance(keys, list):
            for k in keys:
                if not isinstance(k, dict):
                    continue
                api_key = k.get("APIKey")
                if not api_key:
                    continue
                key_hashes.append(
                    hashlib.sha256(str(api_key).encode("utf-8")).hexdigest()
                )
        obj["key_hashes"] = key_hashes
        return obj

    async def _account_label(self, context, payload) -> str:
        try:
            cookies = await context.cookies()
        except Exception:
            cookies = []
        if isinstance(cookies, list):
            for c in cookies:
                if not isinstance(c, dict):
                    continue
                if c.get("name") == _EMAIL_COOKIE:
                    raw = c.get("value", "") or ""
                    try:
                        return unquote(raw)
                    except Exception:
                        return raw
        plans = payload.get("UserPlans") or []
        if isinstance(plans, list) and plans:
            top = plans[0]
            if isinstance(top, dict) and top.get("CompanyID") is not None:
                return str(top.get("CompanyID"))
        return ""
