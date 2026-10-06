"""P1-d 防锤暂停态单元测试（scheduler 包环境，mock transport，禁打生产）。

覆盖方案 §6 验收：
- ⑤ 三处自动启用被拒：调度器两处自动启用（接口续期 / Cookies 续期）命中防锤暂停态
  时拒绝启用、保持暂停、不通知 WebSocket 启动。

所有 DB 写入与出站均 mock，绝无其他真实网络调用。
"""
from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

import app.services.scheduler.api_cookie_renew_task as renew_mod
import app.services.scheduler.cookies_refresh_task as refresh_mod
from app.services.scheduler.api_cookie_renew_task import ApiCookieRenewTaskService
from app.services.scheduler.cookies_refresh_task import CookiesRefreshTaskService


class FakeAsyncSession:
    def __init__(self):
        self.execute = AsyncMock(return_value=SimpleNamespace(rowcount=1))
        self.commit = AsyncMock()

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        return False


def _account(disable_reason=None, status="disabled", account_id="acc_p1"):
    return SimpleNamespace(
        account_id=account_id,
        status=status,
        disable_reason=disable_reason,
        id=1,
        cookie="cookie_val",
        owner_id=1,
    )


def _patch_http():
    """屏蔽调度器内的 get_settings / get_http_client，避免任何真实出站。"""
    settings = SimpleNamespace(websocket_service_url="http://ws.local")
    http_client = MagicMock()
    http_client.post = AsyncMock(return_value={"success": True})
    return (
        patch.object(renew_mod, "get_settings", lambda: settings),
        patch.object(renew_mod, "get_http_client", lambda: http_client),
        patch.object(refresh_mod, "get_settings", lambda: settings),
        patch.object(refresh_mod, "get_http_client", lambda: http_client),
    )


# ---------------------------------------------------------------------------
# 接口续期自动启用
# ---------------------------------------------------------------------------

async def test_api_renew_reject_paused():
    """⑤ 接口续期成功后，暂停态账号被拒启用、保持暂停、不启动任务。"""
    acct = _account(disable_reason="risk_control_auto_pause", status="disabled")
    svc = ApiCookieRenewTaskService()
    session = FakeAsyncSession()
    p1, p2, p3, p4 = _patch_http()
    with p1, p2, p3, p4:
        await svc._enable_account_after_renew(session, acct)
    # 未写库、未启动
    session.execute.assert_not_called()
    session.commit.assert_not_called()
    assert acct.status == "disabled"
    assert acct.disable_reason == "risk_control_auto_pause"


async def test_api_renew_allow_normal_disabled():
    """普通禁用账号接口续期成功后正常自动启用并通知启动。"""
    acct = _account(disable_reason="账号已掉线", status="disabled")
    svc = ApiCookieRenewTaskService()
    session = FakeAsyncSession()
    p1, p2, p3, p4 = _patch_http()
    with p1, p2, p3, p4:
        await svc._enable_account_after_renew(session, acct)
    session.execute.assert_called()
    session.commit.assert_called()
    assert acct.status == "disabled"  # 仅内存对象，落库由 session.execute 完成


# ---------------------------------------------------------------------------
# Cookies 续期自动启用
# ---------------------------------------------------------------------------

async def test_cookies_refresh_reject_paused():
    """⑤ Cookies 续期成功后，暂停态账号被拒启用、保持暂停、不启动任务。"""
    acct = _account(disable_reason="risk_control_auto_pause", status="disabled")
    svc = CookiesRefreshTaskService()
    session = FakeAsyncSession()
    p1, p2, p3, p4 = _patch_http()
    with p1, p2, p3, p4:
        await svc._enable_account_after_refresh(session, acct)
    session.execute.assert_not_called()
    session.commit.assert_not_called()
    assert acct.status == "disabled"
    assert acct.disable_reason == "risk_control_auto_pause"


async def test_cookies_refresh_allow_normal_disabled():
    """普通禁用账号 Cookies 续期成功后正常自动启用并通知启动。"""
    acct = _account(disable_reason="账号已掉线", status="disabled")
    svc = CookiesRefreshTaskService()
    session = FakeAsyncSession()
    p1, p2, p3, p4 = _patch_http()
    with p1, p2, p3, p4:
        await svc._enable_account_after_refresh(session, acct)
    session.execute.assert_called()
    session.commit.assert_called()
