"""FetchItemsTaskService 全量轮模式判定与环境变量解析的单测。

覆盖纯逻辑部分（模式判定 / 间隔解析 / 并发互斥 / 全量轮完成标记条件），
通过 monkeypatch 桩替换数据库与闲鱼接口调用，不触达真实依赖。
"""
from __future__ import annotations

import asyncio
import sys
import time
from pathlib import Path
from types import SimpleNamespace

import pytest

# sys.path 引导：scheduler 目录在前（提供 app.* 包），源码根目录在后（提供 common.*）。
# 与 scheduler/_bootstrap.py 的运行时路径语义一致；backend-web/app 不在路径上，
# 两个服务的同名 app 包不会冲突（本测试进程只加载 scheduler 的 app）。
_SRC_ROOT = Path(__file__).resolve().parents[2]
for _p in (str(_SRC_ROOT / "scheduler"), str(_SRC_ROOT)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import app.services.scheduler.fetch_items_task as _mod  # noqa: E402
from app.services.scheduler.fetch_items_task import (  # noqa: E402
    DEFAULT_FULL_SYNC_INTERVAL_SECONDS,
    FULL_SYNC_INTERVAL_ENV,
    FetchItemsTaskService,
    _resolve_full_sync_interval_seconds,
)


def _make(**kwargs):
    return FetchItemsTaskService(
        task_name="测试", page_size=20, max_pages=None, **kwargs
    )


def _account(account_id: str = "acc1") -> SimpleNamespace:
    return SimpleNamespace(account_id=account_id)


@pytest.fixture(autouse=True)
def _no_session_cooldown(monkeypatch):
    # 默认无冷却；具体用例可再覆盖。
    monkeypatch.setattr(_mod, "is_account_session_cooled", lambda aid: False)


def _stub_fetch(svc, outcomes):
    """替换 _fetch_items_for_account；outcomes 为每次调用的返回 dict 列表。
    返回记录列表，元素为 (调用序号, stop_when_page_all_existing, 返回值)。"""
    calls = []

    async def fake(account, stop_when_page_all_existing=True):
        idx = len(calls)
        outcome = outcomes[idx] if idx < len(outcomes) else outcomes[-1]
        calls.append((idx, stop_when_page_all_existing, outcome))
        return outcome

    svc._fetch_items_for_account = fake  # type: ignore[method-assign]
    return calls


def _stub_accounts(svc, accounts):
    async def fake():
        return list(accounts)

    svc._get_active_accounts = fake  # type: ignore[method-assign]


def test_first_round_after_startup_is_full_sync():
    # 进程启动后首轮必须全量：清理重启前累积的下架滞留。
    svc = _make(full_sync_interval_seconds=86400)
    assert svc.should_run_full_sync() is True


def test_round_after_full_sync_returns_incremental():
    svc = _make(full_sync_interval_seconds=86400)
    svc.mark_full_sync_done()
    assert svc.should_run_full_sync() is False


def test_interval_elapsed_triggers_full_sync_again():
    svc = _make(full_sync_interval_seconds=100)
    svc.mark_full_sync_done()
    assert svc.should_run_full_sync() is False
    # 把上次全量时间拨回间隔之前（直接操作单调时钟标记，模拟时间流逝）。
    svc._last_full_sync_monotonic -= 101
    assert svc.should_run_full_sync() is True


def test_zero_interval_disables_full_sync():
    svc = _make(full_sync_interval_seconds=0)
    assert svc.should_run_full_sync() is False
    # 禁用状态下即使从未跑过、或时间标记被拨回，也恒为增量（旧行为）。
    assert svc._last_full_sync_monotonic is None
    svc._last_full_sync_monotonic = 0
    assert svc.should_run_full_sync() is False


def test_env_unset_uses_default(monkeypatch):
    monkeypatch.delenv(FULL_SYNC_INTERVAL_ENV, raising=False)
    assert _resolve_full_sync_interval_seconds() == DEFAULT_FULL_SYNC_INTERVAL_SECONDS
    svc = _make()
    assert svc.full_sync_interval_seconds == DEFAULT_FULL_SYNC_INTERVAL_SECONDS


def test_env_override_applies(monkeypatch):
    monkeypatch.setenv(FULL_SYNC_INTERVAL_ENV, "3600")
    assert _resolve_full_sync_interval_seconds() == 3600
    svc = _make()
    assert svc.full_sync_interval_seconds == 3600


def test_env_invalid_falls_back_to_default(monkeypatch):
    monkeypatch.setenv(FULL_SYNC_INTERVAL_ENV, "abc")
    assert _resolve_full_sync_interval_seconds() == DEFAULT_FULL_SYNC_INTERVAL_SECONDS


def test_env_zero_disables(monkeypatch):
    monkeypatch.setenv(FULL_SYNC_INTERVAL_ENV, "0")
    svc = _make()
    assert svc.full_sync_interval_seconds == 0
    assert svc.should_run_full_sync() is False


def test_module_singleton_uses_default_config(monkeypatch):
    # 全局实例未显式传参时走环境变量/默认值，不改变既有调用方语义。
    from app.services.scheduler import fetch_items_task as mod

    monkeypatch.delenv(FULL_SYNC_INTERVAL_ENV, raising=False)
    svc = FetchItemsTaskService()
    assert svc.full_sync_interval_seconds == DEFAULT_FULL_SYNC_INTERVAL_SECONDS
    assert mod.fetch_items_task_service.full_sync_interval_seconds > 0


# ============================
# execute() 行为（桩替数据库与上游）
# ============================

_SUCCESS = {"success": True, "total_count": 2, "saved_count": 0}
_FAIL = {"success": False, "message": "上游失败"}
_SKIP = {"success": True, "skipped": True, "total_count": 0, "saved_count": 0}


@pytest.mark.asyncio
async def test_execute_full_round_passes_full_flag_and_marks_done():
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account()])
    calls = _stub_fetch(svc, [_SUCCESS])

    await svc.execute()

    assert calls[0][1] is False  # 全量轮：stop_when_page_all_existing=False
    assert svc.should_run_full_sync() is False  # 成功后标记完成


@pytest.mark.asyncio
async def test_execute_incremental_round_passes_flag_and_no_mark():
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account()])
    calls = _stub_fetch(svc, [_SUCCESS])
    await svc.execute()  # 首轮全量
    assert calls[0][1] is False

    await svc.execute()  # 第二轮：增量
    assert calls[1][1] is True
    assert svc.should_run_full_sync() is False


@pytest.mark.asyncio
async def test_execute_all_accounts_failed_does_not_mark_full_done():
    # 复审问题2：全失败时不标记，下一轮继续全量重试（清理不被推迟一个周期）。
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account()])
    calls = _stub_fetch(svc, [_FAIL])

    await svc.execute()
    assert svc.should_run_full_sync() is True  # 未标记 → 下一轮仍全量

    # 第二轮实际仍是全量模式（传参核对，而非仅查内部状态）。
    await svc.execute()
    assert [c[1] for c in calls] == [False, False]


@pytest.mark.asyncio
async def test_execute_all_accounts_skipped_does_not_mark_full_done():
    # 全部账号锁冲突跳过时同样不标记。
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account()])
    _stub_fetch(svc, [_SKIP])

    await svc.execute()
    assert svc.should_run_full_sync() is True


@pytest.mark.asyncio
async def test_execute_partial_success_marks_full_done():
    # 复审问题2的对称面：至少一个账号成功即标记完成（个别失败不阻塞下一周期）。
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account("a1"), _account("a2")])
    _stub_fetch(svc, [_FAIL, _SUCCESS])

    await svc.execute()
    assert svc.should_run_full_sync() is False


@pytest.mark.asyncio
async def test_execute_account_query_exception_does_not_mark_full_done():
    # 复审问题2：账号查询整体异常时不标记。
    svc = _make(full_sync_interval_seconds=86400)

    async def boom():
        raise RuntimeError("db down")

    svc._get_active_accounts = boom  # type: ignore[method-assign]

    await svc.execute()  # 异常被内部捕获，不外抛
    assert svc.should_run_full_sync() is True


@pytest.mark.asyncio
async def test_execute_no_accounts_returns_without_mark():
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [])
    _stub_fetch(svc, [_SUCCESS])

    await svc.execute()
    assert svc.should_run_full_sync() is True  # 无账号不消耗全量轮


@pytest.mark.asyncio
async def test_concurrent_execute_serialized_and_single_full_round():
    # 复审问题1：并发调用 execute() 必须串行，且只有一轮判为全量。
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account()])
    events: list[tuple[str, float]] = []

    async def slow_fetch(account, stop_when_page_all_existing=True):
        flag = "full" if not stop_when_page_all_existing else "incr"
        events.append(("flag", flag))
        events.append(("enter", time.monotonic()))
        await asyncio.sleep(0.05)  # 制造重叠窗口
        events.append(("exit", time.monotonic()))
        return _SUCCESS

    svc._fetch_items_for_account = slow_fetch  # type: ignore[method-assign]

    await asyncio.gather(svc.execute(), svc.execute())

    # 两轮均执行成功，但只有第一轮是全量（第二轮进入时已被标记为增量）。
    flags = [e for e in events if e[0] == "flag"]
    assert [f[1] for f in flags] == ["full", "incr"]
    # 串行验证：第一轮 exit 早于第二轮 enter。
    ordered = [e[0] for e in events]
    assert ordered == ["flag", "enter", "exit", "flag", "enter", "exit"]
    assert svc.should_run_full_sync() is False


@pytest.mark.asyncio
async def test_execute_respects_session_cooldown_skip():
    # 冷却账号被跳过且不计成功：全失败不标记全量完成。
    svc = _make(full_sync_interval_seconds=86400)
    _stub_accounts(svc, [_account()])
    _stub_fetch(svc, [_SUCCESS])
    _mod.is_account_session_cooled = lambda aid: True  # type: ignore[assignment]

    try:
        await svc.execute()
        assert svc.should_run_full_sync() is True  # 跳过不标记
    finally:
        _mod.is_account_session_cooled = lambda aid: False  # type: ignore[assignment]
