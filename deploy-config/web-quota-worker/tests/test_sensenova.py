"""SensenovaAdapter 离线（mock）单元测试。

覆盖派发单 CARD-WCQ-07 §测试 要求的场景：
- 成功解析单池 / 双池；
- 重定向登录成功；
- 密码错误 -> auth_failed；
- 需短信 -> needs_interactive_auth；
- 401 -> auth_failed；
- 超时 -> error；
- 无账密 -> needs_interactive_auth。

不启动真实浏览器/网络：通过替换薄封装的页面交互原语（可 mock 边界）来离线验证
编排与状态映射逻辑；解析函数另行直接单测。

说明：运行环境未安装 pytest-asyncio（且派发单禁止引入第三方依赖），故异步测试
统一用 ``asyncio.run`` 在同步 ``test_*`` 函数内驱动。
"""

import asyncio
import os
import sys

# 让 whitelisted 命令 `python3 -m pytest tests/test_sensenova.py` 能从
# web-quota-worker 目录直接导入 adapters 包（不新增 conftest/__init__ 文件）。
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from unittest.mock import AsyncMock

from adapters.base import ProbeRequest
from adapters.sensenova import SensenovaAdapter, parse_pool_response


class FakePage:
    """仅用于承载 adapter 内部原语调用的占位对象（真实交互已被 mock 替换）。"""

    def __init__(self, url="https://platform.sensenova.cn/console"):
        self.url = url

    async def goto(self, url):
        self.url = url

    async def click(self, selector):
        pass

    async def fill(self, selector, value):
        pass

    async def press(self, selector, key):
        pass

    async def evaluate(self, script, *args):
        return None

    async def content(self):
        return ""


class FakeContext:
    def __init__(self, page=None):
        self._page = page or FakePage()

    async def new_page(self):
        return self._page

    async def storage_state(self):
        return {"cookies": [], "origins": []}

    async def close(self):
        pass


def _make_adapter():
    async def factory(storage_state, proxy_url):
        return FakeContext()

    return SensenovaAdapter(factory)


def _base_req(**overrides) -> ProbeRequest:
    req = ProbeRequest(provider="sensenova", timeout_seconds=10)
    for key, value in overrides.items():
        setattr(req, key, value)
    return req


SINGLE_POOL_BODY = {
    "pools": [
        {
            "id": "pool-1",
            "pool_type": "general",
            "window_5h": {
                "limit": 100,
                "remaining": 50,
                "reset_at": "2026-11-11T10:00:00Z",
            },
            "window_7d": {
                "limit": 1000,
                "remaining": 500,
                "reset_at": "2026-11-18T10:00:00Z",
            },
            "grant_balance": 12.5,
            "nearest_grant_expiring_balance": 3.0,
            "nearest_grant_expiry": "2026-12-01T00:00:00Z",
        }
    ]
}

DOUBLE_POOL_BODY = {
    "pools": [
        {
            "id": "pool-flash",
            "pool_type": "flash-lite",
            "window_5h": {"limit": 10, "remaining": 4, "reset_at": "2026-11-11T15:00:00Z"},
            "window_7d": {"limit": 70, "remaining": 20, "reset_at": "2026-11-18T15:00:00Z"},
            "grant_balance": 0.0,
            "nearest_grant_expiring_balance": 0.0,
            "nearest_grant_expiry": "2026-12-01T00:00:00Z",
        },
        {
            "id": "pool-general",
            "pool_type": "general",
            "window_5h": {"limit": 200, "remaining": 199, "reset_at": "2026-11-11T09:00:00Z"},
            "window_7d": {"limit": 1400, "remaining": 1300, "reset_at": "2026-11-18T09:00:00Z"},
            "grant_balance": 8.0,
            "nearest_grant_expiring_balance": 1.0,
            "nearest_grant_expiry": "2026-12-05T00:00:00Z",
        },
    ]
}


def test_success_single_pool():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=False)
        a._get_access_token = AsyncMock(return_value="tok-xyz")
        a._fetch_pool_usage = AsyncMock(return_value=(200, SINGLE_POOL_BODY))
        a._extract_account_label = AsyncMock(return_value="")
        a._context_storage_state = AsyncMock(return_value=None)

        res = await a.probe(_base_req())
        assert res.status == "ok", res
        assert res.auth_reason == ""
        assert res.error == ""
        pools = res.data["pools"]
        assert len(pools) == 1
        p = pools[0]
        assert p["id"] == "pool-1"
        assert p["pool_type"] == "general"
        assert p["window_5h"]["limit"] == 100
        assert p["window_5h"]["remaining"] == 50
        assert p["window_7d"]["remaining"] == 500
        assert p["grant_balance"] == 12.5
        assert p["nearest_grant_expiring_balance"] == 3.0
        assert p["nearest_grant_expiry"] == "2026-12-01T00:00:00Z"
        assert res.updated_storage_state is None
        assert res.account_label == ""

    asyncio.run(main())


def test_success_double_pool():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=False)
        a._get_access_token = AsyncMock(return_value="tok-xyz")
        a._fetch_pool_usage = AsyncMock(return_value=(200, DOUBLE_POOL_BODY))
        a._extract_account_label = AsyncMock(return_value="138****0000")
        a._context_storage_state = AsyncMock(return_value='{"cookies":[]}')

        res = await a.probe(_base_req())
        assert res.status == "ok", res
        pools = res.data["pools"]
        assert len(pools) == 2
        assert {p["pool_type"] for p in pools} == {"flash-lite", "general"}
        assert res.account_label == "138****0000"
        assert res.updated_storage_state == '{"cookies":[]}'

    asyncio.run(main())


def test_redirect_login_success():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=True)
        a._do_login = AsyncMock(return_value="ok")
        a._get_access_token = AsyncMock(return_value="tok-after-login")
        a._fetch_pool_usage = AsyncMock(return_value=(200, SINGLE_POOL_BODY))
        a._extract_account_label = AsyncMock(return_value="")
        a._context_storage_state = AsyncMock(return_value=None)

        req = _base_req(console_username="u", console_password="p")
        res = await a.probe(req)
        assert res.status == "ok", res
        a._do_login.assert_awaited_once()

    asyncio.run(main())


def test_password_rejected():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=True)
        a._do_login = AsyncMock(return_value="password_rejected")
        req = _base_req(console_username="u", console_password="wrong")
        res = await a.probe(req)
        assert res.status == "auth_failed", res
        assert res.auth_reason == "password_rejected"

    asyncio.run(main())


def test_sms_or_captcha_required():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=True)
        a._do_login = AsyncMock(return_value="sms_or_captcha_required")
        req = _base_req(console_username="u", console_password="p")
        res = await a.probe(req)
        assert res.status == "needs_interactive_auth", res
        assert res.auth_reason == "sms_or_captcha_required"

    asyncio.run(main())


def test_no_credentials():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=True)
        req = _base_req()
        res = await a.probe(req)
        assert res.status == "needs_interactive_auth", res
        assert res.auth_reason == "login_required"

    asyncio.run(main())


def test_token_rejected_401():
    async def main():
        a = _make_adapter()
        a._navigate_console = AsyncMock(return_value=False)
        a._get_access_token = AsyncMock(return_value="tok-stale")
        a._fetch_pool_usage = AsyncMock(
            return_value=(401, {"code": 401, "message": "unauthorized"})
        )
        res = await a.probe(_base_req())
        assert res.status == "auth_failed", res
        assert res.auth_reason == "token_rejected"

    asyncio.run(main())


def test_timeout():
    async def main():
        a = _make_adapter()

        async def _hang(*args, **kwargs):
            await asyncio.sleep(10)

        a._navigate_console = AsyncMock(side_effect=_hang)
        res = await a.probe(_base_req(timeout_seconds=0.05))
        assert res.status == "error", res
        assert res.error == "timeout"
        assert res.elapsed_ms >= 0

    asyncio.run(main())


# ---------------- parse 直接单测（键名透传 / 缺失字段省略）----------------


def test_parse_passthrough_canonical_keys():
    out = parse_pool_response({"pools": [SINGLE_POOL_BODY["pools"][0]]})
    p = out["pools"][0]
    assert p["pool_type"] == "general"
    assert "window_5h" in p and "window_7d" in p
    assert p["grant_balance"] == 12.5


def test_parse_omits_missing_fields():
    body = {
        "pools": [
            {
                "id": "partial",
                "pool_type": "general",
                "window_5h": {"limit": 5, "remaining": 1, "reset_at": "2026-11-11T10:00:00Z"},
            }
        ]
    }
    out = parse_pool_response(body)
    p = out["pools"][0]
    assert "window_7d" not in p
    assert "grant_balance" not in p
    assert "nearest_grant_expiry" not in p
    assert p["window_5h"]["limit"] == 5


def test_parse_unknown_pool_type_passthrough():
    body = {
        "pools": [
            {
                "id": "x",
                "pool_type": "some-new-type",
                "window_5h": {"limit": 1, "remaining": 1, "reset_at": "z"},
            }
        ]
    }
    out = parse_pool_response(body)
    assert out["pools"][0]["pool_type"] == "some-new-type"


def test_parse_unexpected_key_names_kept():
    body = {
        "pools": [
            {
                "id": "y",
                "window_5h": {"limit": 1, "remaining": 1, "reset_at": "z"},
                "five_hour_window": {"limit": 9},
            }
        ]
    }
    out = parse_pool_response(body)
    p = out["pools"][0]
    assert "five_hour_window" in p
    assert p["five_hour_window"]["limit"] == 9


def test_parse_data_wrapper():
    out = parse_pool_response({"code": 0, "data": {"pools": SINGLE_POOL_BODY["pools"]}})
    assert len(out["pools"]) == 1
    assert out["pools"][0]["id"] == "pool-1"


def test_parse_empty_body():
    assert parse_pool_response(None) == {"pools": []}
    assert parse_pool_response({}) == {"pools": []}
    assert parse_pool_response({"pools": []}) == {"pools": []}
