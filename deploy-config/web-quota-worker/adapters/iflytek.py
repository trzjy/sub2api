"""讯飞星辰 MaaS 控制台额度采集适配器。

本模块实现 ``BaseAdapter`` 契约（见 ``adapters/base.py``），
用于采集讯飞星辰 MaaS 控制台（https://maas.xfyun.cn）的收费额度：
账户余额、Token Plan 额度/席位、Coding Plan 列表。

纪律（照 compshare.py，方案 §3.2 ③ / §2.6 实测）：
- worker 无状态：不落盘任何凭据（storage_state / 密码 / cookie 仅在内存）。
- 鉴权态由传入的 ``storage_state``（含 Cookie + localStorage）承载；
  不接收明文 api_key、不接收明文账号密码。
- 任何解析异常都转换为 ``status="error"`` 的脱敏描述，绝不抛出未捕获异常。
- 绝不打印 ``console_password`` / cookie / ``storage_state`` / 明文 key / 完整手机号。
- 不实现账号密码自动登录：讯飞现有账号是手机短信登录，无法无人值守完成；
  未登录一律 ``needs_interactive_auth``，由人工登录一次落 storageState。
"""

import asyncio
import json
import time

from .base import BaseAdapter, ProbeRequest, ProbeResult

_ACCOUNT_URL = "https://maas.xfyun.cn/account"
_PASSPORT_HOST = "passport.xfyun.cn"

# 同源 fetch 接口（全部 GET，返回 {"code":0,"data":...}）。
_ENDPOINTS = [
    ("balance", "/api/v1/gpt-finetune/user/balance"),
    ("token_plan_quota", "/api/v1/gpt-finetune/token-plan/quota"),
    ("token_plan_seats", "/api/v1/gpt-finetune/token-plan/seats"),
    ("coding_plan_list", "/api/v1/gpt-finetune/coding-plan/list?page=1&size=10"),
    ("user", "/api/v1/gpt-finetune/user"),
]

# 在页面同源上下文内依次取数。credentials:include + localStorage["token"]
# 作 Authorization，头带 x-auth-source:""（与前端同款）。每个接口独立 try/catch，
# 网络异常标记 ok=False，由 Python 侧统一判定（绝不在 JS 内拼装最终 data，
# 也不把任何响应正文带出脱敏边界）。
_FETCH_JS = """
(async () => {
  const token = (localStorage.getItem('token') || '');
  const headers = {'x-auth-source': '', 'Authorization': token};
  const endpoints = __ENDPOINTS__;
  const out = {};
  for (const [key, path] of endpoints) {
    try {
      const resp = await fetch(path, {
        method: 'GET', credentials: 'include', headers: headers
      });
      const json = await resp.json();
      out[key] = {ok: true, code: json.code, data: json.data};
    } catch (e) {
      out[key] = {ok: false, error: String(e)};
    }
  }
  return out;
})()
""".replace("__ENDPOINTS__", json.dumps(_ENDPOINTS, ensure_ascii=False))


def _mask_account_label(user) -> str:
    """从 user 响应脱敏出账户标识。

    - 有 mobile：保留前 3 后 4，中间打码（13985858877 → 139****8877）；
    - 无 mobile：优先 nickname，其次 login；
    - 绝不输出完整手机号、accessKey、secretKey 等敏感键（仅抽取白名单字段）。
    """
    if not isinstance(user, dict):
        return ""
    mobile = user.get("mobile")
    if isinstance(mobile, str) and mobile:
        m = mobile
        if len(m) >= 7:
            return m[:3] + "****" + m[-4:]
        return "****"
    nickname = user.get("nickname")
    if isinstance(nickname, str) and nickname:
        return nickname
    login = user.get("login")
    if isinstance(login, str) and login:
        return login
    return ""


def build_probe_data(balance, token_plan_quota, token_plan_seats, coding_plan_list, user) -> dict:
    """模块级纯函数：把 5 个接口的**内层 data** 映射为统一 data 结构。

    不触网、不启动浏览器，便于单测。仅做结构映射，不判定 code（4001 等
    由上层 `_probe_inner` 处理）。绝不把 accessKey/secretKey 等敏感字段
    带入输出——user 仅用于抽取脱敏后的 account_label，原始 user 不进入 data。

    :param balance: user/balance 的 data → {balance, delinquent, virtualBalance}
    :param token_plan_quota: token-plan/quota 的 data → {quotas:[...]}
    :param token_plan_seats: token-plan/seats 的 data → {seats:[], total}
    :param coding_plan_list: coding-plan/list 的 data → {page,size,total,rows}
    :param user: user 的 data → {id, uid, mobile, login, nickname, email, ...}
    """
    return {
        "auth_state": "ok",
        "account_label": _mask_account_label(user),
        "balance": balance if isinstance(balance, dict) else {},
        "token_plan_quota": (
            token_plan_quota if isinstance(token_plan_quota, dict) else {"quotas": []}
        ),
        "token_plan_seats": (
            token_plan_seats
            if isinstance(token_plan_seats, dict)
            else {"seats": [], "total": 0}
        ),
        "coding_plan_list": (
            coding_plan_list
            if isinstance(coding_plan_list, dict)
            else {"page": 1, "size": 10, "total": 0, "rows": []}
        ),
    }


class IflytekAdapter(BaseAdapter):
    """讯飞星辰 MaaS 上游适配器。"""

    provider = "iflytek"

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
        page = None
        try:
            page = await context.new_page()
            await page.goto(_ACCOUNT_URL, wait_until="domcontentloaded")

            # 未登录会被重定向到 passport.xfyun.cn 登录页。
            url = getattr(page, "url", "") or ""
            if _PASSPORT_HOST in url:
                return ProbeResult(
                    status="needs_interactive_auth",
                    provider=self.provider,
                    auth_reason="login_required",
                )

            raw = await page.evaluate(_FETCH_JS)

            # 逐个接口判定：网络异常 → error；code==4001 → 需交互登录；非 0 → error。
            parts = {}
            for key, _path in _ENDPOINTS:
                entry = raw.get(key) if isinstance(raw, dict) else None
                if not isinstance(entry, dict) or not entry.get("ok"):
                    return ProbeResult(
                        status="error",
                        provider=self.provider,
                        error="api_unavailable",
                    )
                code = entry.get("code")
                if code == 4001:
                    return ProbeResult(
                        status="needs_interactive_auth",
                        provider=self.provider,
                        auth_reason="login_required",
                    )
                if code != 0:
                    return ProbeResult(
                        status="error",
                        provider=self.provider,
                        error="api_unavailable",
                    )
                parts[key] = entry.get("data")

            data = build_probe_data(
                parts.get("balance"),
                parts.get("token_plan_quota"),
                parts.get("token_plan_seats"),
                parts.get("coding_plan_list"),
                parts.get("user"),
            )

            try:
                storage_state = json.dumps(await context.storage_state())
            except Exception:
                storage_state = None

            return ProbeResult(
                status="ok",
                provider=self.provider,
                updated_storage_state=storage_state,
                account_label=data.get("account_label", ""),
                data=data,
            )
        finally:
            if page is not None:
                try:
                    await page.close()
                except Exception:
                    pass
