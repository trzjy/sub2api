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


@pytest.mark.asyncio
async def test_ensure_delivery_record_contract(monkeypatch):
    """Worker 自动发货注册订单级记录：URL/头/载荷与主程序 EnsureWorkerDeliveryRecord 契约一致。

    只传 order_no/quantity/delivery_kind，不传卡券内容（Worker 本地库存不变）。
    """
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

    ok = await mod.ensure_delivery_record("WD-1", quantity=2, delivery_kind="auto")
    assert ok is True
    assert captured["url"] == "http://sub2api:8080/api/v1/internal/xianyu/worker-deliveries"
    assert captured["headers"]["X-Internal-Token"] == "secret-token"
    assert captured["json"] == {"order_no": "WD-1", "quantity": 2, "delivery_kind": "auto"}


@pytest.mark.asyncio
async def test_ensure_delivery_record_unconfigured_skips(monkeypatch):
    from common.services import sub2api_delivery_result_client as mod

    monkeypatch.setattr(mod, "_load_config", lambda: ("", ""))
    assert await mod.ensure_delivery_record("WD-1") is False


@pytest.mark.asyncio
async def test_ensure_delivery_record_retries_then_succeeds(monkeypatch):
    """集成模式：注册失败时做有限重试，最终成功返回 True。"""
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

    ok = await mod.ensure_delivery_record("WD-1", retries=2)
    assert ok is True
    assert len(calls) == 3


@pytest.mark.asyncio
async def test_ensure_delivery_record_returns_false_after_retry_exhausted(monkeypatch):
    """集成模式：重试耗尽仍 5xx 时返回 False（上游必须硬落地终止发货）。"""
    import asyncio

    import httpx

    from common.services import sub2api_delivery_result_client as mod

    calls = []

    class FakeResp:
        status_code = 502
        text = "bad gateway"

    class FakeAsyncClient:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a, **k):
            return False

        async def post(self, url, json=None, headers=None):
            calls.append(1)
            return FakeResp()

    monkeypatch.setattr(httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))
    async def _noop_sleep(s):
        return None
    monkeypatch.setattr(asyncio, "sleep", _noop_sleep)

    ok = await mod.ensure_delivery_record("WD-1", retries=2)
    assert ok is False
    assert len(calls) == 3  # 1 + 2 retries


@pytest.mark.asyncio
async def test_ensure_delivery_record_retries_on_exception(monkeypatch):
    """集成模式：网络异常也应触发有限重试，最终失败返回 False。"""
    import asyncio

    import httpx

    from common.services import sub2api_delivery_result_client as mod

    class FakeAsyncClient:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a, **k):
            return False

        async def post(self, url, json=None, headers=None):
            raise httpx.ConnectError("connection refused")

    monkeypatch.setattr(httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))
    async def _noop_sleep(s):
        return None
    monkeypatch.setattr(asyncio, "sleep", _noop_sleep)

    ok = await mod.ensure_delivery_record("WD-1", retries=2)
    assert ok is False


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
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg", "buyer_id": "b1"},
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
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg", "buyer_id": "b1"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.data["send_status"] == "unknown"


@pytest.mark.asyncio
async def test_delivery_receipt_confirmed_only_on_explicit_success(monkeypatch):
    from common.services import sub2api_delivery_result_client as mod

    calls = []
    async def fake_report(order_no, success, confirmed=False, error=None, timeout=5.0, attempt=0, quantity_sent=None):
        calls.append({
            "order_no": order_no,
            "success": success,
            "confirmed": confirmed,
            "error": error,
            "attempt": attempt,
            "quantity_sent": quantity_sent,
        })

    monkeypatch.setattr(mod, "report_delivery_result_fire_and_forget", fake_report)
    # 显式配置主程序回传地址，绕开 is_configured 短路
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))

    # 明确成功回执 → confirmed=true（标记 sent）
    await mod.report_delivery_result_by_receipt("O1", reasons=[], any_confirmed=True, any_timeout=False)
    assert calls[-1]["order_no"] == "O1"
    assert calls[-1]["success"] is True
    assert calls[-1]["confirmed"] is True
    assert calls[-1]["error"] is None

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
# must_fix 回归测试：quantity_sent 按实发回传（任务5）
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_report_delivery_result_includes_quantity_sent_on_sent(monkeypatch):
    """sent 路径必须把 quantity_sent 写入 payload。"""
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
            captured["json"] = json
            return FakeResp()

    monkeypatch.setattr(httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))

    ok = await mod.report_delivery_result(
        "O1", True, confirmed=True, quantity_sent=2,
    )
    assert ok is True
    assert captured["json"]["quantity_sent"] == 2


@pytest.mark.asyncio
async def test_report_delivery_result_omits_quantity_sent_on_non_sent(monkeypatch):
    """pending/failed 路径不写入 quantity_sent（保留主程序默认 0）。"""
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
            captured["json"] = json
            return FakeResp()

    monkeypatch.setattr(httpx, "AsyncClient", FakeAsyncClient)
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))

    # 失败：传了 quantity_sent 但因 success=False 不写入
    await mod.report_delivery_result(
        "O1", False, confirmed=False, error="x", quantity_sent=2,
    )
    assert "quantity_sent" not in captured["json"]

    # pending（超时）：传了 quantity_sent 但因 confirmed=False 不写入
    await mod.report_delivery_result(
        "O1", True, confirmed=False, error="timeout", quantity_sent=2,
    )
    assert "quantity_sent" not in captured["json"]


@pytest.mark.asyncio
async def test_report_delivery_result_by_receipt_passes_quantity_sent_on_sent(monkeypatch):
    """by_receipt 的 sent 路径必须把 quantity_sent 透传到 fire_and_forget。"""
    from common.services import sub2api_delivery_result_client as mod

    calls = []

    async def fake_report(order_no, success, confirmed=False, error=None, timeout=5.0, attempt=0, quantity_sent=None):
        calls.append({"success": success, "confirmed": confirmed, "quantity_sent": quantity_sent})

    monkeypatch.setattr(mod, "report_delivery_result_fire_and_forget", fake_report)
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))

    # sent 路径：quantity_sent 应透传
    await mod.report_delivery_result_by_receipt(
        "O1", reasons=[], any_confirmed=True, any_timeout=False, quantity_sent=2,
    )
    assert calls[-1] == {"success": True, "confirmed": True, "quantity_sent": 2}

    # failed 路径：quantity_sent 不应透传（fire_and_forget 调用时不传）
    await mod.report_delivery_result_by_receipt(
        "O1", reasons=["CSI_FORBID"], any_confirmed=False, any_timeout=False, quantity_sent=99,
    )
    assert calls[-1]["quantity_sent"] is None

    # pending 路径：quantity_sent 不应透传
    await mod.report_delivery_result_by_receipt(
        "O1", reasons=[], any_confirmed=False, any_timeout=True, quantity_sent=99,
    )
    assert calls[-1]["quantity_sent"] is None


# ---------------------------------------------------------------------------
# must_fix 回归测试：pre-send 失败回传 success=false confirmed=false（任务4）
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_pre_send_failures_route_to_failed_receipt(monkeypatch):
    """pre-send 阶段 4 类失败（匹配失败/库存耗尽/API取码失败/发送前异常）必须
    统一回传 success=false, confirmed=false + 失败原因（避免孤儿 pending 订单）。
    """
    from common.services import sub2api_delivery_result_client as mod

    calls = []

    async def fake_report(order_no, success, confirmed=False, error=None, timeout=5.0, attempt=0, quantity_sent=None):
        calls.append({
            "order_no": order_no,
            "success": success,
            "confirmed": confirmed,
            "error": error,
            "quantity_sent": quantity_sent,
        })

    monkeypatch.setattr(mod, "report_delivery_result_fire_and_forget", fake_report)
    # 显式配置主程序回传地址，绕开 is_configured 短路
    monkeypatch.setattr(mod, "_load_config", lambda: ("http://sub2api:8080", "secret-token"))

    failure_scenarios = [
        ("匹配失败", "未找到匹配的发货规则"),
        ("库存耗尽", "卡券库存不足"),
        ("API 取码失败", "外部卡券接口 502"),
        ("发送前异常", "自动发货处理异常: KeyError"),
    ]

    for scenario_name, reason in failure_scenarios:
        # 模拟 auto_delivery_handler 在未发生发送副作用时调用 _report_delivery_receipt。
        await mod.report_delivery_result_by_receipt(
            "O1",
            reasons=[reason],
            any_confirmed=False,
            any_timeout=False,
        )
        assert calls[-1]["success"] is False, f"{scenario_name}: success 应为 False"
        assert calls[-1]["confirmed"] is False, f"{scenario_name}: confirmed 应为 False"
        assert reason in calls[-1]["error"], f"{scenario_name}: 失败原因应被记录"
        assert calls[-1]["quantity_sent"] is None, f"{scenario_name}: failed 不写 quantity_sent"


@pytest.mark.asyncio
async def test_pre_send_failure_skipped_when_unconfigured(monkeypatch):
    """standalone 模式（未配置主程序回传）：pre-send 失败不回传（避免无意义网络调用）。"""
    from common.services import sub2api_delivery_result_client as mod

    posted = []

    async def fake_fire_and_forget(*args, **kwargs):
        posted.append((args, kwargs))

    monkeypatch.setattr(mod, "report_delivery_result_fire_and_forget", fake_fire_and_forget)
    monkeypatch.setattr(mod, "_load_config", lambda: ("", ""))

    await mod.report_delivery_result_by_receipt(
        "O1",
        reasons=["未找到匹配的发货规则"],
        any_confirmed=False,
        any_timeout=False,
    )
    assert not posted, "未配置主程序回传时不应调用 fire_and_forget"


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


# ---------------------------------------------------------------------------
# must_fix 回归测试：internal API 移除 JWT fallback（仅 X-Worker-Token 唯一入口）
# ---------------------------------------------------------------------------


def _make_request_with_jwt(jwt: str | None) -> Request:
    """构造仅携带 JWT Bearer（无 X-Worker-Token）的请求：模拟普通 admin 前端登录态。"""
    headers = []
    if jwt:
        headers.append((b"authorization", f"Bearer {jwt}".encode()))
    return Request(
        scope={
            "type": "http",
            "method": "GET",
            "path": "/api/v1/internal/cookies/details",
            "headers": headers,
        }
    )


@pytest.mark.asyncio
async def test_internal_api_rejects_missing_worker_token(internal_token):
    """缺失 X-Worker-Token 必须 401，即使客户端是合法 admin 用户也不得回退。"""
    admin = FakeUser(id=1, role="ADMIN", status="ACTIVE")
    session = FakeSession(users=[admin])
    with pytest.raises(HTTPException) as exc:
        await get_service_or_user(request=_make_request_with_jwt(None), session=session)
    assert exc.value.status_code == 401
    assert "X-Worker-Token" in exc.value.detail


@pytest.mark.asyncio
async def test_internal_api_rejects_only_jwt(internal_token):
    """仅携带 admin JWT（无 X-Worker-Token）必须 401，绝不回退到 JWT 认证。"""
    admin = FakeUser(id=1, role="ADMIN", status="ACTIVE")
    session = FakeSession(users=[admin])
    with pytest.raises(HTTPException) as exc:
        await get_service_or_user(
            request=_make_request_with_jwt("admin.jwt.token"), session=session
        )
    assert exc.value.status_code == 401
    # 关键：失败原因是"缺少 X-Worker-Token"，而不是"JWT 校验失败"；
    # 防止任何 active admin JWT 越权调用服务间接口。
    assert "X-Worker-Token" in exc.value.detail


@pytest.mark.asyncio
async def test_internal_api_rejects_invalid_worker_token(internal_token):
    """错误的 X-Worker-Token 必须 401（即使客户端也带了 admin JWT）。"""
    admin = FakeUser(id=1, role="ADMIN", status="ACTIVE")
    session = FakeSession(users=[admin])
    # 同时携带错误的 worker_token + admin JWT —— 都不应通过
    request = _make_request("wrong-token")
    request.scope["headers"].append((b"authorization", b"Bearer admin.jwt.token"))
    with pytest.raises(HTTPException) as exc:
        await get_service_or_user(request=request, session=session)
    assert exc.value.status_code == 401


@pytest.mark.asyncio
async def test_internal_api_accepts_valid_worker_token(internal_token):
    """有效 X-Worker-Token 必须成功解析为现有管理员用户。"""
    admin = FakeUser(id=1, role="ADMIN", status="ACTIVE")
    session = FakeSession(users=[admin])
    # 即便同时携带 admin JWT，也必须由 X-Worker-Token 通道解析（不是 JWT 通道）。
    request = _make_request("test-internal-token")
    request.scope["headers"].append((b"authorization", b"Bearer admin.jwt.token"))
    user = await get_service_or_user(request=request, session=session)
    assert user is admin


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
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg", "buyer_id": "b1"},
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
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg", "buyer_id": "b1"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.success is False
    assert resp.data["dispatched"] is False


# ---------------------------------------------------------------------------
# must_fix 回归测试：buyer_id 全链路贯通（补发 + 公开消息 + websocket_client）
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_internal_send_message_requires_buyer_id(monkeypatch):
    """补发接口 payload 缺 buyer_id 必须失败关闭，避免下游拼成 None@goofish。"""
    from app.api.routes import internal_api as routes
    from app.services.account_service import AccountService

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        async def get_account_for_user(self, owner_id, account_id):
            return FakeAccount()

    monkeypatch.setattr(AccountService, "get_account_for_user", FakeAccountService().get_account_for_user)

    # monkeypatch websocket_client.send_message 防止真实网络调用
    called = {"flag": False}

    async def fake_send_message(**kwargs):
        called["flag"] = True
        return {"success": True, "data": {"receipt": "DISPATCHED_DEFINITE_FAILURE"}}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )

    # 缺 buyer_id：必须失败，且 send_message 不得被调用（边界前拦截）
    resp = await routes.internal_send_message(
        payload={"account_id": "a1", "chat_id": "c1", "message": "msg"},
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.success is False
    assert "buyer_id" in resp.message
    assert called["flag"] is False


@pytest.mark.asyncio
async def test_internal_send_message_passes_buyer_id_to_websocket(monkeypatch):
    """补发接口必须把 payload["buyer_id"] 透传给 websocket_client.send_message。"""
    from app.api.routes import internal_api as routes
    from app.services.account_service import AccountService

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        async def get_account_for_user(self, owner_id, account_id):
            return FakeAccount()

    monkeypatch.setattr(AccountService, "get_account_for_user", FakeAccountService().get_account_for_user)

    captured = {}

    async def fake_send_message(**kwargs):
        captured["buyer_id"] = kwargs.get("buyer_id")
        return {"success": True, "data": {"receipt": "DISPATCHED_DEFINITE_FAILURE"}}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )

    resp = await routes.internal_send_message(
        payload={
            "account_id": "a1",
            "chat_id": "c1",
            "message": "msg",
            "buyer_id": "real-buyer-123",
        },
        session=None,
        service_user=FakeUser(id=1),
    )
    assert resp.success is True
    assert captured["buyer_id"] == "real-buyer-123"


@pytest.mark.asyncio
async def test_websocket_client_send_message_rejects_missing_buyer_id(monkeypatch):
    """websocket_client.send_message 缺 buyer_id 必须立即失败关闭（不发出 None@goofish 请求）。"""
    from app.services.websocket_client import WebSocketServiceClient

    # base_url 来自 settings；monkeypatch settings 注入固定地址
    from app.core.config import get_settings
    settings = get_settings()
    monkeypatch.setattr(settings, "websocket_service_url", "http://ws:8090")

    client = WebSocketServiceClient()
    # 替 http_client，让缺 buyer_id 的快速失败路径不被实际网络调用触发
    called = {"flag": False}

    class FakeHttp:
        async def post(self, *a, **k):
            called["flag"] = True
            return {}

    monkeypatch.setattr(client, "http_client", FakeHttp())

    result = await client.send_message(
        account_id="a1",
        chat_id="c1",
        content="hello",
    )
    assert result["success"] is False
    assert "buyer_id is required" in result["message"]
    assert called["flag"] is False  # 边界前拦截，未发出请求


@pytest.mark.asyncio
async def test_websocket_client_send_message_includes_buyer_id_in_payload(monkeypatch):
    """buyer_id 必须进入 payload 透传给 Worker 端。"""
    from app.core.config import get_settings
    from app.services.websocket_client import WebSocketServiceClient

    settings = get_settings()
    monkeypatch.setattr(settings, "websocket_service_url", "http://ws:8090")

    captured = {}

    class FakeHttp:
        async def post(self, url, json=None, **k):
            captured["url"] = url
            captured["json"] = json
            return {"success": True, "data": {}}

    client = WebSocketServiceClient()
    monkeypatch.setattr(client, "http_client", FakeHttp())

    result = await client.send_message(
        account_id="a1",
        chat_id="c1",
        content="hello",
        buyer_id="real-buyer-123",
    )
    assert result["success"] is True
    assert captured["json"]["buyer_id"] == "real-buyer-123"


@pytest.mark.asyncio
async def test_message_route_passes_to_user_id_as_buyer_id(monkeypatch):
    """公开消息接口（message.py）必须把 to_user_id 作为 buyer_id 传给 websocket_client。"""
    from app.api.routes import message as msg_route

    captured = {}

    async def fake_send_message(**kwargs):
        captured["buyer_id"] = kwargs.get("buyer_id")
        return {"success": True}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )
    # 旁路 API秘钥校验以聚焦 buyer_id 透传
    monkeypatch.setattr(msg_route, "verify_api_key", lambda _k: True)

    resp = await msg_route.send_message(
        request=msg_route.SendMessageRequest(
            api_key="k",
            cookie_id="a1",
            chat_id="c1",
            to_user_id="real-buyer-456",
            message="hello",
        )
    )
    assert resp.success is True
    assert captured["buyer_id"] == "real-buyer-456"


@pytest.mark.asyncio
async def test_message_route_rejects_missing_to_user_id(monkeypatch):
    """公开消息接口缺 to_user_id 必须失败关闭（与补发一致，避免 None@goofish）。"""
    from app.api.routes import message as msg_route

    called = {"flag": False}

    async def fake_send_message(**kwargs):
        called["flag"] = True
        return {"success": True}

    monkeypatch.setattr(
        "app.services.websocket_client.websocket_client.send_message",
        fake_send_message,
    )
    monkeypatch.setattr(msg_route, "verify_api_key", lambda _k: True)

    resp = await msg_route.send_message(
        request=msg_route.SendMessageRequest(
            api_key="k",
            cookie_id="a1",
            chat_id="c1",
            to_user_id="",  # 空字符串应被既有 required_params 校验拦截
            message="hello",
        )
    )
    assert resp.success is False
    assert called["flag"] is False  # 边界前拦截，不会发出请求


# ---------------------------------------------------------------------------
# must_fix 回归测试：账号删除前校验 stop 结果（任务6）
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
async def test_delete_account_returns_502_when_stop_fails(monkeypatch):
    """Worker 停止失败时必须返回 502 且不删除账号。"""
    from fastapi import HTTPException

    from app.api.routes import cookies as cookies_route
    from app.services.websocket_client import websocket_client

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        deleted = {"flag": False}

        async def get_account_for_user(self, owner_id, account_id):
            return FakeAccount()

        async def delete_account(self, account):
            self.deleted["flag"] = True
            return None

    fake_service = FakeAccountService()

    # 旁路 _get_account_or_404 返回假账号；用 get_account_for_user 替代
    async def fake_get_or_404(*args, **kwargs):
        return FakeAccount()

    monkeypatch.setattr(cookies_route, "_get_account_or_404", fake_get_or_404)

    # stop_account 返回失败
    async def fake_stop(*a, **k):
        return {"success": False, "message": "worker offline"}

    monkeypatch.setattr(websocket_client, "stop_account", fake_stop)

    try:
        await cookies_route.delete_account(
            account_id="a1",
            current_user=FakeUser(id=1),
            account_service=fake_service,
        )
        assert False, "expected 502"
    except HTTPException as exc:
        assert exc.status_code == 502
        assert "停止账号任务失败" in exc.detail

    assert fake_service.deleted["flag"] is False, "stop 失败时不得删除账号"


@pytest.mark.asyncio
async def test_delete_account_proceeds_when_stop_succeeds(monkeypatch):
    """Worker 停止成功时正常删除账号。"""
    from app.api.routes import cookies as cookies_route
    from app.services.websocket_client import websocket_client

    class FakeAccount:
        account_id = "a1"

    class FakeAccountService:
        deleted = {"flag": False}

        async def delete_account(self, account):
            self.deleted["flag"] = True
            return None

    fake_service = FakeAccountService()

    async def fake_get_or_404(*args, **kwargs):
        return FakeAccount()

    monkeypatch.setattr(cookies_route, "_get_account_or_404", fake_get_or_404)

    async def fake_stop(*a, **k):
        return {"success": True, "message": "stopped"}

    monkeypatch.setattr(websocket_client, "stop_account", fake_stop)

    resp = await cookies_route.delete_account(
        account_id="a1",
        current_user=FakeUser(id=1),
        account_service=fake_service,
    )
    assert resp.success is True
    assert fake_service.deleted["flag"] is True
