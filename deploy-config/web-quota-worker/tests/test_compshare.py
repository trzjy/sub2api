"""compshare 适配器离线单元测试（mock 驱动，不触网）。

覆盖：成功解析（多套餐）、RetCode!=0+无账密→needs_interactive_auth、
出现 Geetest→captcha_required、密码错误→auth_failed、超时→error、
key_hashes 为 sha256 且结果中无明文 key。
"""

import asyncio
import hashlib
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from unittest.mock import AsyncMock

from adapters.compshare import CompshareAdapter
from adapters.base import ProbeRequest, ProbeResult



PLAIN_KEYS = ["secret-api-key-001", "secret-api-key-002"]


def _make_plan(code, name, company_id=None, keys=None, **overrides):
    plan = {
        "Code": code,
        "PlanName": name,
        "Status": 1,
        "ExpireAt": 1796910909,
        "LimitPer5h": 1200,
        "UsagePer5h": 11.5,
        "UsagePer5hNextResetAt": 1794344400,
        "LimitPerWeek": 3000,
        "UsagePerWeek": 98.5,
        "UsagePerWeekNextResetAt": 1794758400,
        "LimitPerMonth": 7600,
        "UsagePerMonth": 98.5,
        "UsagePerMonthNextResetAt": 1796910909,
    }
    if company_id is not None:
        plan["CompanyID"] = company_id
    if keys is not None:
        plan["Keys"] = [{"APIKey": k} for k in keys]
    plan.update(overrides)
    return plan


class FakeResponse:
    def __init__(self, status, payload):
        self.status = status
        self._payload = payload

    async def json(self):
        return self._payload


class FakePage:
    def __init__(self, url="", content="", wait_raises=False):
        self.url = url
        self._content = content
        self._wait_raises = wait_raises
        self.goto = AsyncMock()
        self.click = AsyncMock()
        self.fill = AsyncMock()
        self.close = AsyncMock()
        self.content = AsyncMock(return_value=self._content)
        self.wait_for_url = AsyncMock(
            side_effect=(Exception("not redirected") if wait_raises else None)
        )


class FakeContext:
    def __init__(
        self,
        post_response=None,
        post_coro=None,
        cookies=None,
        storage_state=None,
        page=None,
    ):
        self._post_response = post_response
        self._post_coro = post_coro
        self._cookies = cookies if cookies is not None else []
        self._storage_state = (
            storage_state if storage_state is not None else {"cookies": []}
        )
        self._page = page
        self.request = type(
            "R",
            (),
            {
                "post": (
                    post_coro
                    if post_coro is not None
                    else AsyncMock(return_value=post_response)
                )
            },
        )()
        self.new_page = AsyncMock(return_value=page if page is not None else FakePage())
        self.cookies = AsyncMock(return_value=self._cookies)
        self.storage_state = AsyncMock(return_value=self._storage_state)
        self.close = AsyncMock()

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


def _adapter_for(context):
    async def factory(storage_state, proxy_url):
        return context

    return CompshareAdapter(factory)


def _assert_no_plaintext(result: ProbeResult, *secrets):
    blob = json.dumps(result.__dict__, ensure_ascii=False)
    for s in secrets:
        assert s not in blob, f"明文凭据泄露到结果: {s}"


# ---------------------------------------------------------------------------
# 1. 成功解析（含多套餐）
# ---------------------------------------------------------------------------
def test_success_multi_plan():
    payload = {
        "RetCode": 0,
        "UserPlans": [
            _make_plan("cppkg-1", "Basic 基础版", company_id=1001, keys=PLAIN_KEYS),
            _make_plan("cppkg-2", "Pro 专业版", keys=["only-one-key"]),
        ],
    }
    ctx = FakeContext(
        post_response=FakeResponse(200, payload),
        cookies=[
            {"name": "U_JWT_TOKEN", "domain": ".compshare.cn", "value": "jwt.x.y"},
            {
                "name": "U_USER_EMAIL",
                "domain": ".compshare.cn",
                "value": "user%40example.com",
            },
        ],
        storage_state={"cookies": [{"name": "U_JWT_TOKEN"}]},
    )
    adapter = _adapter_for(ctx)
    req = ProbeRequest(provider="compshare", timeout_seconds=60)
    result = asyncio.run(adapter.probe(req))

    assert result.status == "ok", result
    plans = result.data["plans"]
    assert len(plans) == 2
    assert plans[0]["code"] == "cppkg-1"
    assert plans[0]["plan_name"] == "Basic 基础版"
    assert plans[0]["plan_status"] == 1
    assert plans[0]["limit_5h"] == 1200
    assert plans[0]["used_5h"] == 11.5
    assert plans[0]["reset_5h"] == 1794344400
    assert plans[0]["limit_month"] == 7600
    # key_hashes 为 sha256
    assert plans[0]["key_hashes"] == [
        hashlib.sha256(k.encode()).hexdigest() for k in PLAIN_KEYS
    ]
    # account_label 优先取 U_USER_EMAIL 解码值
    assert result.account_label == "user@example.com"
    # updated_storage_state 已生成
    assert json.loads(result.updated_storage_state) == {"cookies": [{"name": "U_JWT_TOKEN"}]}
    _assert_no_plaintext(result, *PLAIN_KEYS, "user%40example.com", "jwt.x.y")


# ---------------------------------------------------------------------------
# 2. RetCode != 0 + 无账密 → needs_interactive_auth / login_required
# ---------------------------------------------------------------------------
def test_retcode_nonzero_no_creds():
    payload = {"RetCode": 1, "Msg": "not login"}
    ctx = FakeContext(post_response=FakeResponse(200, payload))
    adapter = _adapter_for(ctx)
    req = ProbeRequest(provider="compshare", timeout_seconds=60)
    result = asyncio.run(adapter.probe(req))

    assert result.status == "needs_interactive_auth"
    assert result.auth_reason == "login_required"


# ---------------------------------------------------------------------------
# 3. 出现 Geetest → captcha_required
# ---------------------------------------------------------------------------
def test_captcha_required():
    payload = {"RetCode": 1, "Msg": "unauthorized"}
    page = FakePage(url="https://passport.compshare.cn/", content="", wait_raises=True)
    ctx = FakeContext(
        post_response=FakeResponse(200, payload),
        cookies=[],  # 无 U_JWT_TOKEN
        page=page,
    )
    adapter = _adapter_for(ctx)
    req = ProbeRequest(
        provider="compshare",
        console_username="u",
        console_password="p",
        timeout_seconds=60,
    )
    result = asyncio.run(adapter.probe(req))

    assert result.status == "needs_interactive_auth"
    assert result.auth_reason == "captcha_required"
    # 登录流程确实被触发（new_page 被调用）
    assert ctx.new_page.await_count >= 1


# ---------------------------------------------------------------------------
# 4. 密码错误 → auth_failed / password_rejected
# ---------------------------------------------------------------------------
def test_password_rejected():
    payload = {"RetCode": 1, "Msg": "unauthorized"}
    page = FakePage(
        url="https://passport.compshare.cn/",
        content="账号或密码不正确",
        wait_raises=True,
    )
    ctx = FakeContext(post_response=FakeResponse(200, payload), page=page)
    adapter = _adapter_for(ctx)
    req = ProbeRequest(
        provider="compshare",
        console_username="u",
        console_password="wrong",
        timeout_seconds=60,
    )
    result = asyncio.run(adapter.probe(req))

    assert result.status == "auth_failed"
    assert result.auth_reason == "password_rejected"


# ---------------------------------------------------------------------------
# 5. 超时 → error / timeout
# ---------------------------------------------------------------------------
def test_timeout():
    async def slow_post(*args, **kwargs):
        await asyncio.sleep(0.5)
        return FakeResponse(200, {"RetCode": 0, "UserPlans": []})

    ctx = FakeContext(post_coro=slow_post)
    adapter = _adapter_for(ctx)
    req = ProbeRequest(provider="compshare", timeout_seconds=0.05)
    result = asyncio.run(adapter.probe(req))

    assert result.status == "error"
    assert result.error == "timeout"


# ---------------------------------------------------------------------------
# 6. key_hashes 为 sha256 且结果中无明文 key
# ---------------------------------------------------------------------------
def test_key_hashes_sha256_no_plaintext():
    payload = {
        "RetCode": 0,
        "UserPlans": [
            _make_plan("cppkg-x", "Basic 基础版", keys=PLAIN_KEYS),
        ],
    }
    ctx = FakeContext(
        post_response=FakeResponse(200, payload),
        cookies=[{"name": "U_JWT_TOKEN", "domain": ".compshare.cn", "value": "j"}],
        storage_state={"cookies": []},
    )
    adapter = _adapter_for(ctx)
    req = ProbeRequest(provider="compshare", timeout_seconds=60)
    result = asyncio.run(adapter.probe(req))

    kh = result.data["plans"][0]["key_hashes"]
    assert kh == [hashlib.sha256(k.encode()).hexdigest() for k in PLAIN_KEYS]
    # 明文 key 不得出现在任何结果序列化中
    _assert_no_plaintext(result, *PLAIN_KEYS)


# ---------------------------------------------------------------------------
# 7. 数值缺失时字段省略（不填 0 冒充）
# ---------------------------------------------------------------------------
def test_missing_numeric_fields_omitted():
    plan = {
        "Code": "cppkg-min",
        "PlanName": "Basic 基础版",
        # 故意缺失 Status / ExpireAt / 各类用量
        "Keys": [{"APIKey": "PLAINTEXT-SECRET-KEY"}],
    }
    payload = {"RetCode": 0, "UserPlans": [plan]}
    ctx = FakeContext(
        post_response=FakeResponse(200, payload),
        cookies=[{"name": "U_JWT_TOKEN", "domain": ".compshare.cn", "value": "j"}],
        storage_state={"cookies": []},
    )
    adapter = _adapter_for(ctx)
    req = ProbeRequest(provider="compshare", timeout_seconds=60)
    result = asyncio.run(adapter.probe(req))

    obj = result.data["plans"][0]
    assert "plan_status" not in obj
    assert "expire_at" not in obj
    assert "limit_5h" not in obj
    assert "used_month" not in obj
    assert obj["code"] == "cppkg-min"
    _assert_no_plaintext(result, "PLAINTEXT-SECRET-KEY")
