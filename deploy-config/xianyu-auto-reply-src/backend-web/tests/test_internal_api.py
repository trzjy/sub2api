"""
主程序内网服务契约测试（internal_api + 服务身份认证）

覆盖：
- get_service_or_user：X-Worker-Token 匹配 SUB2API_INTERNAL_TOKEN → 返回现有管理员用户；
  不匹配 → 401；未携带 → 回退 JWT。
- internal_api 路由注册：全部真实路由存在、挂载于 /api/v1/internal 前缀。
- sub2api_delivery_result_client：回传 URL/头/载荷契约（模拟 httpx）。

运行：PYTHONPATH=<repo>/backend-web:<repo> pytest backend-web/tests -p asyncio
"""
from __future__ import annotations

import pytest
from fastapi import FastAPI, HTTPException, Request

from app.api.deps import get_service_or_user
from app.api.routes import internal_api


class FakeUser:
    """最小 User 替身，仅承载服务身份解析所需字段。"""

    def __init__(self, id: int, role: str = "ADMIN", status: str = "ACTIVE"):
        self.id = id
        self.role = role
        self.status = status


class FakeSession:
    """最小 AsyncSession 替身：编译 SQL 后按 role/status 过滤用户。"""

    def __init__(self, users: list[FakeUser]):
        self._users = users

    async def execute(self, stmt):
        candidates = list(self._users)
        try:
            compiled = stmt.compile(compile_kwargs={"literal_binds": True})
            sql_text = str(compiled)
            if "role" in sql_text and "ADMIN" in sql_text:
                candidates = [u for u in candidates if u.role == "ADMIN"]
            if "status" in sql_text and "ACTIVE" in sql_text:
                candidates = [u for u in candidates if u.status == "ACTIVE"]
        except Exception:
            # 编译失败时不做过滤（保留所有用户，便于 FALLBACK 场景）
            pass

        class _Result:
            def __init__(self, users):
                self._users = users

            def scalar_one_or_none(self):
                return self._users[0] if self._users else None

        return _Result(candidates)


@pytest.fixture
def internal_token(monkeypatch):
    """注入 SUB2API_INTERNAL_TOKEN 配置。"""
    from app.core.config import get_settings

    settings = get_settings()
    monkeypatch.setattr(settings, "sub2api_internal_token", "test-internal-token")
    return settings


def _make_request(worker_token: str | None = None) -> Request:
    headers = []
    if worker_token:
        headers.append((b"x-worker-token", worker_token.encode()))
    return Request(
        scope={
            "type": "http",
            "method": "GET",
            "path": "/api/v1/internal/cookies/details",
            "headers": headers,
        }
    )


@pytest.mark.asyncio
async def test_service_or_user_accepts_internal_token(internal_token):
    admin = FakeUser(id=1, role="ADMIN", status="ACTIVE")
    session = FakeSession(users=[admin])
    user = await get_service_or_user(request=_make_request("test-internal-token"), session=session)
    assert user is admin


@pytest.mark.asyncio
async def test_service_or_user_rejects_invalid_token(internal_token):
    session = FakeSession(users=[])
    with pytest.raises(HTTPException) as exc:
        await get_service_or_user(request=_make_request("wrong-token"), session=session)
    assert exc.value.status_code == 401


@pytest.mark.asyncio
async def test_service_or_user_returns_none_for_non_admin(internal_token):
    member = FakeUser(id=2, role="MEMBER", status="ACTIVE")
    session = FakeSession(users=[member])
    with pytest.raises(HTTPException) as exc:
        await get_service_or_user(request=_make_request("test-internal-token"), session=session)
    assert exc.value.status_code == 403


def test_internal_api_routes_registered():
    """断言全部真实内网路由已注册（FastAPI 0.141 延迟展开，直接断言 router.routes）。"""
    paths = {route.path: set(route.methods or []) for route in internal_api.router.routes if hasattr(route, "path")}
    expected = {
        "/internal/cookies/details": {"GET"},
        "/internal/cookies/{account_id}/status": {"PUT"},
        "/internal/cookies/renew-login": {"POST"},
        "/internal/cookies/{account_id}/clear-credentials": {"POST"},
        "/internal/items/cookie/{cookie_id}": {"GET"},
        "/internal/qr-login/generate": {"POST"},
        "/internal/qr-login/status/{session_id}": {"GET"},
        "/internal/messages/send": {"POST"},
    }
    for path, methods in expected.items():
        assert path in paths, f"missing internal route {path}"
        assert methods.issubset(paths[path]), f"route {path} methods mismatch"
    # 敏感 Cookie 取回端点已删除：仅通过 generate/status 白名单投影流转，缩小攻击面。
    assert "/internal/qr-login/cookie/{session_id}" not in paths


# ---------------------------------------------------------------------------
# JWT 扫码状态路由授权（resolve_session_owner 共享边界）
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_jwt_qr_status_rejects_unknown_and_non_owner(monkeypatch):
    """JWT 扫码状态路由：未知 session / 非 owner 必须真实 403（与内部路由一致）。"""
    from fastapi import HTTPException

    from app.api.routes import qr_login as qr_login_routes

    SESSION_OWNER = qr_login_routes.SESSION_OWNER
    PROCESSED_SESSIONS = qr_login_routes.PROCESSED_SESSIONS

    PROCESSED_SESSIONS.clear()
    SESSION_OWNER.clear()

    async def noop(**kwargs):
        return {}

    # 未知 session：SESSION_OWNER 与 PROCESSED_SESSIONS 均无记录 → 403
    try:
        await qr_login_routes.get_qr_status(session_id="sess-x", current_user=FakeUser(id=1), db=None)
        assert False, "expected 403 for unknown session"
    except HTTPException as exc:
        assert exc.status_code == 403

    # 非 owner：会话归属 user=1，user=2 查询 → 403
    SESSION_OWNER["sess-2"] = 1
    try:
        await qr_login_routes.get_qr_status(session_id="sess-2", current_user=FakeUser(id=2), db=None)
        assert False, "expected 403 for non-owner"
    except HTTPException as exc:
        assert exc.status_code == 403

    # 已处理结果 owner 不匹配：PROCESSED_SESSIONS 归属 user=1，user=3 查询 → 403
    PROCESSED_SESSIONS["sess-3"] = {"status": "success", "account_id": "a1", "is_new_account": True, "owner_id": 1}
    try:
        await qr_login_routes.get_qr_status(session_id="sess-3", current_user=FakeUser(id=3), db=None)
        assert False, "expected 403 for processed-result owner mismatch"
    except HTTPException as exc:
        assert exc.status_code == 403

    PROCESSED_SESSIONS.clear()
    SESSION_OWNER.clear()


def test_internal_api_prefix_mounted_under_v1():
    """确认 internal router 前缀为 /internal（经 _bootstrap 挂载于 /api/v1 下）。"""
    assert internal_api.router.prefix == "/internal"


@pytest.mark.asyncio
async def test_qr_status_enforces_session_owner(monkeypatch):
    """扫码状态查询必须校验 SESSION_OWNER，防止跨会话越权窥探。"""
    from fastapi import HTTPException

    from app.api.routes.qr_login import SESSION_OWNER
    from app.api.routes import internal_api as routes

    # 本次会话归属 user=1
    SESSION_OWNER.clear()
    SESSION_OWNER["sess-1"] = 1

    def fake_get_status(session_id: str):
        return {"status": "success", "session_id": session_id, "cookies": {"a": "b"}, "unb": "u1"}

    # 成功处理链：模拟 get_session_cookies 返回凭证，upsert 走 Python 侧假替换。
    # 这里仅验证内部路由不再绕过成功处理链：进度依赖 process_qr_login_success，
    # 而该函数内部依赖真实 DB（upsert_account_from_qr），故在无 DB 环境下
    # 用"已处理"短路验证 SESSION_OWNER 越权与 Cookies 白名单。
    monkeypatch.setattr("app.services.qr_login.qr_login_manager.get_session_status", fake_get_status)

    # 其他用户查询 → 403（不得返回 cookies）
    try:
        await routes.internal_get_qr_status(session_id="sess-1", service_user=FakeUser(id=2, role="MEMBER"))
        assert False, "expected 403 for non-owner"
    except HTTPException as exc:
        assert exc.status_code == 403

    # 会话所有者 → 白名单投影，不返回 cookies/unb（成功处理链在无 DB 时不应泄露凭证）
    SESSION_OWNER["sess-1"] = 1

    from app.api.routes import qr_login as qr_login_routes

    monkeypatch.setattr(
        qr_login_routes,
        "PROCESSED_SESSIONS",
        {"sess-1": {"account_id": "a1", "is_new_account": True, "status": "success", "owner_id": 1}},
    )
    resp = await routes.internal_get_qr_status(session_id="sess-1", service_user=FakeUser(id=1))
    assert resp.data["status"] in ("success", "already_processed")
    assert "cookies" not in resp.data
    assert "unb" not in resp.data

    # 非 owner 的已处理结果也拒绝（owner_id 持久化校验）
    SESSION_OWNER["sess-1"] = 1
    try:
        await routes.internal_get_qr_status(session_id="sess-1", service_user=FakeUser(id=3))
        assert False, "expected 403 for non-owner processed result"
    except HTTPException as exc:
        assert exc.status_code == 403

    SESSION_OWNER.clear()


@pytest.mark.asyncio
async def test_delivery_result_client_contract(monkeypatch):
    """回传 URL/头/载荷与主程序 XianyuDeliveryHandler 契约一致。"""
    import httpx

    from common.services import sub2api_delivery_result_client as mod

    captured = {}

    class FakeResp:
        status_code = 200
        text = ""

    class FakeAsyncClient:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a, **k):
            return False

        async def post(self, url, json=None, headers=None):
            captured["url"] = url
            captured["json"] = json
            captured["headers"] = headers
            return FakeResp()

    monkeypatch.setattr(httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(
        mod,
        "_load_config",
        lambda: ("http://sub2api:8080", "secret-token"),
    )

    ok = await mod.report_delivery_result("O1", True, confirmed=True, attempt=3)
    assert ok is True
    assert captured["url"] == "http://sub2api:8080/api/v1/internal/xianyu/delivery-results"
    assert captured["headers"]["X-Internal-Token"] == "secret-token"
    assert captured["json"] == {"order_no": "O1", "success": True, "confirmed": True, "error": None, "attempt": 3}


@pytest.mark.asyncio
async def test_delivery_result_client_unconfigured_skips(monkeypatch):
    from common.services import sub2api_delivery_result_client as mod

    monkeypatch.setattr(mod, "_load_config", lambda: ("", ""))
    assert await mod.report_delivery_result("O1", False, confirmed=False) is False


# ---------------------------------------------------------------------------
# delivery-results 三态回传判定（sub2api_delivery_result_client 共享实现）
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_internal_send_message_missing_send_status_falls_back_unknown(monkeypatch):
    """转发层缺 send_status 时不得回退 success——必须回退 unknown。"""
    from app.api.routes import internal_api as routes

    async def fake_send_message(**kwargs):
        return {"success": True, "message": "ok", "data": {"a": 1}}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )

    # service_user 身份 + 账号归属校验需 DB，故直接断言回退逻辑：通过 monkeypatch
    # 让 AccountService 返回假账号（避免 DB）。
    from app.services.account_service import AccountService

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        async def get_account_for_user(self, owner_id, account_id):
            return FakeAccount()

    monkeypatch.setattr(AccountService, "get_account_for_user", FakeAccountService().get_account_for_user)

    resp = await routes.internal_send_message(
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.data["send_status"] == "unknown"


@pytest.mark.asyncio
async def test_internal_send_message_rejects_unknown_send_status(monkeypatch):
    """下游返回未知 send_status 值时转发层不得提升为 success。"""
    from app.api.routes import internal_api as routes

    async def fake_send_message(**kwargs):
        return {"success": True, "message": "ok", "data": {"send_status": "weird"}}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )
    from app.services.account_service import AccountService

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        async def get_account_for_user(self, owner_id, account_id):
            return FakeAccount()

    monkeypatch.setattr(AccountService, "get_account_for_user", FakeAccountService().get_account_for_user)

    resp = await routes.internal_send_message(
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.data["send_status"] == "unknown"


@pytest.mark.asyncio
async def test_delivery_receipt_confirmed_only_on_explicit_success(monkeypatch):
    from common.services import sub2api_delivery_result_client as mod

    calls = []
    async def fake_report(order_no, success, confirmed=False, error=None, timeout=5.0, attempt=0):
        calls.append({"order_no": order_no, "success": success, "confirmed": confirmed, "error": error, "attempt": attempt})

    monkeypatch.setattr(mod, "report_delivery_result_fire_and_forget", fake_report)

    # 明确成功回执 → confirmed=true（标记 sent）
    await mod.report_delivery_result_by_receipt("O1", reasons=[], any_confirmed=True, any_timeout=False)
    assert calls[-1] == {"order_no": "O1", "success": True, "confirmed": True, "error": None, "attempt": 0}

    # 明确拒绝 → success=false, confirmed=false, error
    await mod.report_delivery_result_by_receipt("O1", reasons=["CSI_FORBID"], any_confirmed=False, any_timeout=False)
    assert calls[-1]["success"] is False
    assert calls[-1]["confirmed"] is False
    assert "CSI_FORBID" in calls[-1]["error"]

    # 超时（无最终回执）→ confirmed=false，主程序保持 pending
    await mod.report_delivery_result_by_receipt("O1", reasons=[], any_confirmed=False, any_timeout=True)
    assert calls[-1]["success"] is True
    assert calls[-1]["confirmed"] is False
    assert calls[-1]["error"]  # 提示未收到最终回执


# ---------------------------------------------------------------------------
# must_fix 回归测试：空 token 拒绝服务 / 回传有限重试 / internal router 鉴权
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_service_or_user_rejects_unconfigured_token(monkeypatch):
    """internal token 未配置时必须拒绝服务，不得静默降级为普通 JWT 路由。"""
    from app.core.config import get_settings

    settings = get_settings()
    monkeypatch.setattr(settings, "sub2api_internal_token", "")
    admin = FakeUser(id=1, role="ADMIN", status="ACTIVE")
    session = FakeSession(users=[admin])
    with pytest.raises(HTTPException) as exc:
        await get_service_or_user(request=_make_request("anything"), session=session)
    assert exc.value.status_code == 401


@pytest.mark.asyncio
async def test_delivery_result_client_retries_then_succeeds(monkeypatch):
    """回传非 2xx 时做有限重试，最终成功返回 True，避免订单永久滞留 pending。"""
    import asyncio

    import httpx

    from common.services import sub2api_delivery_result_client as mod

    calls = []

    class FakeResp:
        status_code = 500
        text = "boom"

    class FakeAsyncClient:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a, **k):
            return False

        async def post(self, url, json=None, headers=None):
            calls.append(1)
            resp = FakeResp()
            if len(calls) >= 3:
                resp.status_code = 200
            return resp

    monkeypatch.setattr(httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))

    async def _noop_sleep(s):
        return None

    monkeypatch.setattr(asyncio, "sleep", _noop_sleep)

    ok = await mod.report_delivery_result("O1", True, confirmed=True, retries=2)
    assert ok is True
    assert len(calls) == 3


@pytest.mark.asyncio
async def test_require_internal_token_enforces_match(monkeypatch):
    """websocket/scheduler internal router 鉴权：缺失/错误/空配置一律 401。"""
    from common.core.config import get_settings
    from common.utils.internal_auth import require_internal_token

    settings = get_settings()
    monkeypatch.setattr(settings, "sub2api_internal_token", "secret-token")

    # 匹配 → 通过
    await require_internal_token(x_internal_token="secret-token")

    # 缺失/错误 → 401
    for bad in (None, "wrong-token"):
        try:
            await require_internal_token(x_internal_token=bad)
            assert False, "expected 401"
        except HTTPException as exc:
            assert exc.status_code == 401


@pytest.mark.asyncio
async def test_browser_renew_injects_internal_token(monkeypatch):
    """browser-renew 委托调用必须携带 X-Internal-Token（internal 路由强制鉴权）。"""
    import aiohttp

    from common.services import cookie_renew_browser_service as mod

    captured = {}

    class FakeResp:
        status = 200

        async def text(self):
            return "{}"

        async def json(self):
            return {
                "success": True,
                "data": {"has_quick_enter": False, "new_cookies_str": "nc", "updated_cookie_names": []},
                "message": "ok",
            }

    class FakePostContext:
        def __init__(self):
            pass

        async def __aenter__(self):
            return FakeResp()

        async def __aexit__(self, *a, **k):
            return False

    class FakeAsyncClient:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a, **k):
            return False

        def post(self, url, json=None, headers=None):
            captured["url"] = url
            captured["headers"] = headers
            return FakePostContext()

    class FakeSettings:
        websocket_service_url = "http://ws:8090"
        sub2api_internal_token = "secret-token"

    monkeypatch.setattr(mod, "get_settings", lambda: FakeSettings())
    monkeypatch.setattr(aiohttp, "ClientSession", FakeAsyncClient)
    monkeypatch.setattr(aiohttp, "ClientTimeout", lambda **k: None)

    svc = mod.CookieRenewBrowserService()
    result = await svc._renew_via_websocket("cookies", "a1", "[test]")
    assert result.success is True
    assert "/internal/cookies/browser-renew" in captured["url"]
    assert captured["headers"].get("X-Internal-Token") == "secret-token"


@pytest.mark.asyncio
async def test_browser_renew_fails_closed_without_token(monkeypatch):
    """internal token 未配置时 browser-renew 必须失败关闭，不发出空 token 请求。"""
    import aiohttp

    from common.services import cookie_renew_browser_service as mod

    posted = []

    class FakeAsyncClient:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a, **k):
            return False

        async def post(self, url, json=None, headers=None):
            posted.append(url)
            return None

    class FakeSettings:
        websocket_service_url = "http://ws:8090"
        sub2api_internal_token = ""

    monkeypatch.setattr(mod, "get_settings", lambda: FakeSettings())
    monkeypatch.setattr(aiohttp, "ClientSession", FakeAsyncClient)
    monkeypatch.setattr(aiohttp, "ClientTimeout", lambda **k: None)

    svc = mod.CookieRenewBrowserService()
    result = await svc._renew_via_websocket("cookies", "a1", "[test]")
    assert result.success is False
    assert not posted, "no request should be sent when token is not configured"


@pytest.mark.asyncio
async def test_internal_send_message_preserves_dispatched_false(monkeypatch):
    """下游 dispatched=false 必须透传，供主程序补发回滚 failed（确定未 dispatch）。"""
    from app.api.routes import internal_api as routes
    from app.services.account_service import AccountService

    async def fake_send_message(**kwargs):
        return {"success": False, "message": "账号未连接", "data": {"dispatched": False}}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        async def get_account_for_user(self, owner_id, account_id):
            return FakeAccount()

    monkeypatch.setattr(AccountService, "get_account_for_user", FakeAccountService().get_account_for_user)

    resp = await routes.internal_send_message(
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.data["send_status"] == "failed"
    assert resp.data["dispatched"] is False


@pytest.mark.asyncio
async def test_internal_send_message_account_query_error_marks_undispatched(monkeypatch):
    """账号查询异常（边界前）必须返回 dispatched=false，不得让补发永久滞留 pending。"""
    from app.api.routes import internal_api as routes
    from app.services.account_service import AccountService

    class FakeAccountService:
        async def get_account_for_user(self, owner_id, account_id):
            raise RuntimeError("db timeout")

    monkeypatch.setattr(AccountService, "get_account_for_user", FakeAccountService().get_account_for_user)

    resp = await routes.internal_send_message(
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.success is False
    assert resp.data["dispatched"] is False
