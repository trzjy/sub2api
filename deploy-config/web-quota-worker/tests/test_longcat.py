"""LongCat 适配器离线单元测试（mock 浏览器上下文，不触网）。

覆盖派发单要求的五个场景：
1. 已登录成功解析（currentLot + otherLots 合并、只保留 ACTIVE）；
2. 重定向 passport.meituan.com -> interactive_login_required；
3. 会话失效 -> session_expired；
4. 超时 -> error；
5. 缺失 paygo 字段时结构正确。
"""

import asyncio
from unittest.mock import AsyncMock, MagicMock

import pytest

from adapters.base import ProbeRequest
from adapters.longcat import LongcatAdapter


def _make_request(**overrides) -> ProbeRequest:
    kwargs = {
        "provider": "longcat",
        "storage_state": None,
        "console_username": None,
        "console_password": None,
        "console_login_url": "",
        "success_url_prefix": "",
        "api_key_sha256": "",
        "proxy_url": None,
        "timeout_seconds": 60,
    }
    kwargs.update(overrides)
    return ProbeRequest(**kwargs)


def _make_resp(status: int, payload: dict):
    resp = MagicMock()
    resp.status = status
    resp.json = AsyncMock(return_value=payload)
    return resp


def _make_page(url: str, api_map: dict):
    """构造一个 mock 页面；api_map 以接口 path 映射到 (status, payload)。"""
    page = MagicMock()
    page.url = url

    def _resolve(path: str):
        # path 可能为完整 URL，取 longcat.chat 之后的部分。
        if "longcat.chat" in path:
            path = path.split("longcat.chat", 1)[1]
        return api_map[path]

    def _post(url_, **_kwargs):
        status, payload = _resolve(url_)
        return _make_resp(status, payload)

    def _get(url_, **_kwargs):
        status, payload = _resolve(url_)
        return _make_resp(status, payload)

    request = MagicMock()
    request.post = AsyncMock(side_effect=_post)
    request.get = AsyncMock(side_effect=_get)
    page.request = request
    page.goto = AsyncMock()
    return page


def _make_factory(page):
    context = MagicMock()
    context.new_page = AsyncMock(return_value=page)
    context.storage_state = AsyncMock(return_value={"cookies": [], "origins": []})
    context.close = AsyncMock()

    async def _factory(storage_state, proxy_url):
        return context

    return _factory, context


# 样本响应 ----------------------------------------------------------------

TOKEN_PACKS = {
    "data": {
        "currentLot": {
            "id": 621017,
            "status": "ACTIVE",
            "remainingToken": 13391161,
            "totalToken": 50000000,
            "expireTime": "2026-12-09 17:08:08",
        },
        "otherLots": [
            {
                "id": 621018,
                "status": "ACTIVE",
                "remainingToken": 100,
                "totalToken": 200,
                "expireTime": "2026-12-10 00:00:00",
            },
            {
                "id": 621019,
                "status": "EXPIRED",
                "remainingToken": 0,
                "totalToken": 100,
                "expireTime": "2026-01-01 00:00:00",
            },
        ],
    }
}

API_USAGE = {
    "data": {
        "paygoBalanceCent": 0,
        "paygoStatus": "NORMAL",
        "paygoBalance": {"primary": {"currency": "CNY", "amount": "0.00"}},
    }
}

USER_CURRENT = {"data": {"phone": "13800138000", "userId": "12345"}}


# 测试 1：已登录成功解析（合并 + 仅 ACTIVE）-------------------------------

def test_logged_in_success_parse():
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (200, TOKEN_PACKS),
        "/api/pay/quota/metering/api-usage/summary": (200, API_USAGE),
        "/api/v1/user-current": (200, USER_CURRENT),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    adapter = LongcatAdapter(factory)

    result = asyncio.run(adapter.probe(_make_request()))

    assert result.status == "ok"
    assert result.account_label == "13800138000"
    lots = result.data["lots"]
    # 仅保留 ACTIVE；EXPIRED 被剔除。
    assert len(lots) == 2
    # currentLot 在前且 is_current=True。
    assert lots[0]["lot_id"] == 621017
    assert lots[0]["is_current"] is True
    assert lots[0]["remaining_token"] == 13391161
    assert lots[0]["total_token"] == 50000000
    assert lots[0]["expire_time"] == "2026-12-09 17:08:08"
    # otherLots 中 ACTIVE 项 is_current=False。
    assert lots[1]["lot_id"] == 621018
    assert lots[1]["is_current"] is False
    # paygo 字段齐全。
    assert result.data["paygo_balance_cent"] == 0
    assert result.data["paygo_status"] == "NORMAL"
    assert result.data["paygo_currency"] == "CNY"
    assert result.data["paygo_amount"] == "0.00"
    # 成功时回传 storage_state（不打印，仅校验为合法 JSON 字符串）。
    assert isinstance(result.updated_storage_state, str)
    import json
    json.loads(result.updated_storage_state)


# 测试 2：重定向 passport.meituan.com -> interactive_login_required --------

def test_redirect_to_passport_interactive():
    page = _make_page(
        "https://passport.meituan.com/useraccount/login?xxx", api_map={}
    )
    factory, _ = _make_factory(page)
    adapter = LongcatAdapter(factory)

    result = asyncio.run(adapter.probe(_make_request()))

    assert result.status == "needs_interactive_auth"
    assert result.auth_reason == "interactive_login_required"
    # 不做任何自动登录尝试，不应发起同源用量请求。
    page.request.post.assert_not_called()
    page.request.get.assert_not_called()


# 测试 3：会话失效 -> session_expired --------------------------------------

def test_session_expired():
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (401, {"code": 401, "msg": "未登录"}),
        "/api/pay/quota/metering/api-usage/summary": (200, API_USAGE),
        "/api/v1/user-current": (200, USER_CURRENT),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    adapter = LongcatAdapter(factory)

    result = asyncio.run(adapter.probe(_make_request()))

    assert result.status == "needs_interactive_auth"
    assert result.auth_reason == "session_expired"


# 测试 4：超时 -> error ----------------------------------------------------

def test_timeout_returns_error():
    page = _make_page("https://longcat.chat/platform/usage", api_map={})
    # 让 goto 睡眠超过总体超时，触发 asyncio.wait_for 超时。
    async def _slow_goto(*_args, **_kwargs):
        await asyncio.sleep(0.3)
    page.goto = AsyncMock(side_effect=_slow_goto)
    factory, _ = _make_factory(page)
    adapter = LongcatAdapter(factory)

    result = asyncio.run(adapter.probe(_make_request(timeout_seconds=0.05)))

    assert result.status == "error"
    assert result.error == "timeout"


# 测试 5：缺失 paygo 字段时结构正确 ---------------------------------------

def test_missing_paygo_fields():
    # api-usage 响应不含任何 paygo 字段。
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (200, TOKEN_PACKS),
        "/api/pay/quota/metering/api-usage/summary": (200, {"data": {}}),
        "/api/v1/user-current": (200, USER_CURRENT),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    adapter = LongcatAdapter(factory)

    result = asyncio.run(adapter.probe(_make_request()))

    assert result.status == "ok"
    # lots 仍存在（ACTIVE 合并）。
    assert len(result.data["lots"]) == 2
    # paygo 字段缺失时应被省略，绝不填 0 冒充。
    assert "paygo_balance_cent" not in result.data
    assert "paygo_status" not in result.data
    assert "paygo_currency" not in result.data
    assert "paygo_amount" not in result.data


# 测试 6：404/403 只算「这次读不到」，不能判会话失效 -----------------------
#
# 2026-11-13 线上故障：旧实现把所有非 2xx 都当成 session_expired，用户刚登成功、
# 会话明明有效（worker 的跳转校验都过了），只因为某个用量接口非 2xx 就被判死，
# 会话被丢掉，界面一直提示「登录已失效，请重新登录」，重登多少次都一样。
#
# 契约：只有 401（以及 body code 401）算「明确没登录」；其余一律是「判不了」，
# 由调用方决定，适配器不许冒充「你没登录」。

def _usage_ok():
    return (200, API_USAGE)


def _user_ok():
    return (200, USER_CURRENT)


@pytest.mark.parametrize("bad_status", [400, 403, 404, 405, 429, 500, 502, 503])
def test_non_auth_failures_are_probe_unavailable(bad_status):
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (bad_status, {"code": bad_status}),
        "/api/pay/quota/metering/api-usage/summary": _usage_ok(),
        "/api/v1/user-current": _user_ok(),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    result = asyncio.run(LongcatAdapter(factory).probe(_make_request()))

    assert result.status == "error"
    assert result.error == "probe_unavailable"
    # 关键：不许变成 needs_interactive_auth —— 那会让上层把会话丢掉。
    assert result.auth_reason != "session_expired"


def test_body_code_not_401_is_probe_unavailable():
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (200, {"code": 500, "msg": "boom"}),
        "/api/pay/quota/metering/api-usage/summary": _usage_ok(),
        "/api/v1/user-current": _user_ok(),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    result = asyncio.run(LongcatAdapter(factory).probe(_make_request()))

    assert result.status == "error"
    assert result.error == "probe_unavailable"


def test_status_401_is_session_expired():
    """明确 401 才判失效：这是「真的没登录」的唯一凭据。"""
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (401, {"code": 401, "msg": "未登录"}),
        "/api/pay/quota/metering/api-usage/summary": _usage_ok(),
        "/api/v1/user-current": _user_ok(),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    result = asyncio.run(LongcatAdapter(factory).probe(_make_request()))

    assert result.status == "needs_interactive_auth"
    assert result.auth_reason == "session_expired"


def test_body_code_401_is_session_expired():
    """2xx 但 body 说 401，同样算失效。"""
    api_map = {
        "/api/pay/quota/metering/token-packs/summary": (200, {"code": 401, "msg": "未登录"}),
        "/api/pay/quota/metering/api-usage/summary": _usage_ok(),
        "/api/v1/user-current": _user_ok(),
    }
    page = _make_page("https://longcat.chat/platform/usage", api_map)
    factory, _ = _make_factory(page)
    result = asyncio.run(LongcatAdapter(factory).probe(_make_request()))

    assert result.status == "needs_interactive_auth"
    assert result.auth_reason == "session_expired"
