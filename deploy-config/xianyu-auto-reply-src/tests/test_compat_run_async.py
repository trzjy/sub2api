"""common/db/compat.py 常驻工作循环 `_run_async` 机制的 pytest 测试。

被测契约（以实现为准）：
- 进程级常驻单工作线程（daemon，名为 compat-db-worker）跑 `loop.run_forever()`，
  循环关闭后可重建；
- `DBManagerCompat._run_async(async_func, timeout)` 把 `async_func(session_maker)`
  提交到常驻循环并阻塞取结果：结果透传；协程异常按 sleep(attempt) 重试共 3 次
  失败返回 None（不抛）；future.result 超时立即返回 None 且不重试（协程继续在
  后台跑完）；循环已关闭 → 携带失败循环身份触发 `_ensure_worker_loop` 锁内
  身份比较重建后重提交（并发恢复只建一次，其余调用方复用健康循环）；
- `_worker_session_maker` 为惰性缓存属性，预填后首次调用采纳为当前循环的上下文
  （完全不触碰 create_async_engine / 真实引擎）；引擎/闸门绑定创建时的工作循环，
  循环重建后整体废弃重建（外审 P2）；并发闸门容量=池总容量，在库协程数不超过
  连接数（外审 P1）。

打桩约定：不连真实 MySQL、不发网络请求。所有 DB 依赖经 `mgr._worker_session_maker`
预填 fake 桩短路；`compat.time` 整体替换为记录型桩使重试等待变 no-op 并可断言
sleep 调用参数。测试间共享的模块级 `_worker_loop`/`_worker_thread` 按常驻语义
用完留着复用，仅"循环关闭恢复"用例自行收尾旧线程。
"""
import asyncio
import threading
import time as real_time

import pytest

import common.db.compat as compat


WORKER_NAME = compat._COMPAT_DB_WORKER_THREAD_NAME


# ---------------------------------------------------------------------------
# 桩与夹具
# ---------------------------------------------------------------------------

class RecordingTimeShim:
    """替换 compat.time 的记录桩：sleep 变 no-op 并记录参数，其余属性透传真模块。"""

    def __init__(self):
        self.sleep_calls = []

    def sleep(self, seconds):
        self.sleep_calls.append(seconds)

    def __getattr__(self, name):
        return getattr(real_time, name)


@pytest.fixture(autouse=True)
def fast_retry_sleep(monkeypatch):
    """全局把 compat.time 换成记录桩：重试路径零真实等待，测试可断言 sleep 参数。"""
    shim = RecordingTimeShim()
    monkeypatch.setattr(compat, "time", shim)
    yield shim


class FakeResult:
    """session.execute(...) 返回值的最小桩，按方法体实际消费面提供属性。"""

    def __init__(self, rowcount=0, scalar=None):
        self.rowcount = rowcount
        self._scalar = scalar

    def scalar_one_or_none(self):
        return self._scalar


class FakeSession:
    """最小异步会话桩：支持 async with / execute / commit / rollback。"""

    def __init__(self, result=None, execute_exc=None):
        self.result = result if result is not None else FakeResult(rowcount=1)
        self.execute_exc = execute_exc
        self.execute_calls = []
        self.commit_calls = 0
        self.rollback_calls = 0

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        return False

    async def execute(self, stmt, params=None):
        self.execute_calls.append(stmt)
        if self.execute_exc is not None:
            raise self.execute_exc
        return self.result

    async def commit(self):
        self.commit_calls += 1

    async def rollback(self):
        self.rollback_calls += 1


class FakeSessionMaker:
    """会话工厂桩：调用即返回预置的 FakeSession。"""

    def __init__(self, session):
        self.session = session
        self.calls = 0

    def __call__(self):
        self.calls += 1
        return self.session


@pytest.fixture
def fake_session():
    return FakeSession(result=FakeResult(rowcount=1))


@pytest.fixture
def fake_maker(fake_session):
    return FakeSessionMaker(fake_session)


@pytest.fixture
def mgr(fake_maker):
    """预填 _worker_session_maker 缓存的被测实例：_ensure_worker_session_maker
    走缓存短路，整个测试进程绝不触碰 create_async_engine / 真实引擎。"""
    manager = compat.DBManagerCompat()
    manager._worker_session_maker = fake_maker
    return manager


@pytest.fixture
def forbid_real_engine(monkeypatch):
    """双保险：桩化用例里若有人误触 create_async_engine 立即失败。"""

    def _boom(*args, **kwargs):
        raise AssertionError("桩化测试不应创建真实引擎 create_async_engine")

    monkeypatch.setattr(compat, "create_async_engine", _boom)
    return _boom


def alive_worker_threads():
    return [t for t in threading.enumerate()
            if t.name == WORKER_NAME and t.is_alive()]


def make_async_func(value):
    """返回一个接受 session_maker 的协程函数，执行时原样返回 value。"""

    async def _func(session_maker):
        return value

    return _func


# ---------------------------------------------------------------------------
# 用例
# ---------------------------------------------------------------------------

def test_worker_thread_is_singleton_and_persistent(mgr):
    """多次 _run_async 后 compat-db-worker 线程始终恰 1 个且是同一线程对象。"""
    first_thread = None
    for i in range(5):
        assert mgr._run_async(make_async_func(i)) == i
        workers = alive_worker_threads()
        assert len(workers) == 1, f"第 {i} 次调用后应只有 1 个存活工作线程"
        if first_thread is None:
            first_thread = workers[0]
        assert workers[0] is first_thread, "多次调用应复用同一线程对象"
        assert workers[0] is compat._worker_thread
        assert workers[0].daemon is True, "工作线程应为 daemon"
        assert compat._worker_loop is not None
        assert not compat._worker_loop.is_closed()

    # 跨实例同样复用同一常驻线程（线程是进程级，不随 DBManagerCompat 实例增长）
    other = compat.DBManagerCompat()
    other._worker_session_maker = mgr._worker_session_maker
    assert other._run_async(make_async_func("other")) == "other"
    workers = alive_worker_threads()
    assert len(workers) == 1
    assert workers[0] is first_thread


def test_result_passthrough(mgr):
    """协程返回值原样透传：任意对象保持同一引用，None/标量原样返回。"""
    payload = {"k": [1, 2], "inner": object()}
    assert mgr._run_async(make_async_func(payload)) is payload
    assert mgr._run_async(make_async_func(None)) is None
    assert mgr._run_async(make_async_func(42)) == 42
    assert mgr._run_async(make_async_func("text")) == "text"


def test_session_maker_passed_from_instance_cache(mgr, fake_maker, forbid_real_engine):
    """async_func 收到的 session_maker 就是实例缓存里预填的 fake_maker。"""
    received = {}

    async def _func(session_maker):
        received["maker"] = session_maker
        return "ok"

    assert mgr._run_async(_func) == "ok"
    assert received["maker"] is fake_maker


def test_exception_retries_three_times_then_none(mgr, fast_retry_sleep):
    """协程恒抛异常：返回 None 不抛；恰调用 3 次；sleep 依次收到 1、2。"""
    calls = {"n": 0}

    async def _always_fail(session_maker):
        calls["n"] += 1
        raise RuntimeError("boom")

    assert mgr._run_async(_always_fail) is None
    assert calls["n"] == 3, "共尝试 3 次"
    assert fast_retry_sleep.sleep_calls == [1, 2], "重试前按 attempt 等待 1、2 秒"


def test_exception_recovers_on_second_attempt(mgr, fast_retry_sleep):
    """第 1 次抛异常第 2 次成功：返回成功值，sleep 仅收到 1 一次。"""
    calls = {"n": 0}

    async def _flaky(session_maker):
        calls["n"] += 1
        if calls["n"] == 1:
            raise ValueError("first attempt fails")
        return "recovered"

    assert mgr._run_async(_flaky) == "recovered"
    assert calls["n"] == 2
    assert fast_retry_sleep.sleep_calls == [1]


def test_timeout_returns_none_without_retry(mgr, fast_retry_sleep):
    """结果等待超时：立即返回 None、不重试；协程继续在后台跑完（等待收尾）。"""
    calls = {"n": 0}
    finished = threading.Event()

    async def _slow(session_maker):
        calls["n"] += 1
        await asyncio.sleep(2)
        finished.set()
        return "late"

    start = real_time.monotonic()
    result = mgr._run_async(_slow, timeout=0.2)
    elapsed = real_time.monotonic() - start

    assert result is None, "超时应立即返回 None"
    assert calls["n"] == 1, "超时路径不重试"
    assert elapsed < 1.5, "应在远小于协程时长（2s）内返回"
    assert fast_retry_sleep.sleep_calls == [], "超时路径不应出现重试等待"

    # 超时协程仍在常驻工作循环上继续执行：等它跑完，避免脏状态泄漏到后续用例
    assert finished.wait(timeout=10), "遗留协程应在工作循环上跑完并置位事件"


def test_update_risk_control_log_success_true(mgr, fake_maker, forbid_real_engine, fast_retry_sleep):
    """update_risk_control_log 成功路径：execute+commit 后按 rowcount 返回 True。"""
    fake_maker.session.result = FakeResult(rowcount=1)

    result = mgr.update_risk_control_log(123, processing_status="resolved", processing_result="ok")

    assert result is True
    session = fake_maker.session
    assert len(session.execute_calls) == 1, "应恰好执行一条 UPDATE"
    assert session.commit_calls == 1
    assert session.rollback_calls == 0
    assert fast_retry_sleep.sleep_calls == [], "成功路径不应有重试等待"


def test_update_risk_control_log_exception_false(mgr, forbid_real_engine, fast_retry_sleep):
    """session.execute 抛异常：不向调用方抛，返回 falsy。

    注意：实现中 _run_async 吞掉协程异常（重试 3 次后返回 None），外层
    try/except 收不到异常，故方法实际返回 None 而非字面量 False——这里按
    实现语义断言 falsy + is None。
    """
    session = FakeSession(execute_exc=RuntimeError("db down"))
    mgr._worker_session_maker = FakeSessionMaker(session)

    result = mgr.update_risk_control_log(123, processing_status="resolved")  # 不得向调用方抛

    assert not result
    assert result is None
    assert len(session.execute_calls) == 3, "3 次尝试各自执行到 execute"
    assert fast_retry_sleep.sleep_calls == [1, 2]


def test_recovers_after_worker_loop_closed(mgr):
    """工作循环被关闭后：_run_async 自动重建循环并成功返回，仍保持单工作线程。"""
    # 1) 先跑一次成功启动常驻循环
    assert mgr._run_async(make_async_func("warmup")) == "warmup"
    old_loop = compat._worker_loop
    old_thread = compat._worker_thread
    assert old_thread is not None and old_thread.is_alive()

    # 2) 模拟工作线程异常退出：停循环并等旧线程退出（finally 中 loop.close()）
    old_loop.call_soon_threadsafe(old_loop.stop)
    old_thread.join(timeout=5)
    assert not old_thread.is_alive(), "旧工作线程应已退出"
    assert old_loop.is_closed(), "旧循环应已被关闭"

    # 3) 再提交：应自动重建循环并成功返回。
    # 循环已更换：旧引擎上下文随旧循环失效（外审 P2），桩化场景重新预填并
    # 复位上下文循环记录，让新桩在新循环上被采纳。
    fresh_session = FakeSession(result=FakeResult(rowcount=1))
    mgr._worker_session_maker = FakeSessionMaker(fresh_session)
    mgr._worker_context_loop = None
    assert mgr._run_async(make_async_func("after-restart")) == "after-restart"
    assert mgr._worker_context_loop is compat._worker_loop, "上下文应绑定重建后的循环"

    new_loop = compat._worker_loop
    new_thread = compat._worker_thread
    assert new_loop is not old_loop, "应重建新的事件循环"
    assert new_thread is not old_thread, "应重建新的工作线程"
    assert new_thread.is_alive()
    assert not new_loop.is_closed()

    workers = alive_worker_threads()
    assert len(workers) == 1, "旧线程退出后应仍是单工作线程"
    assert workers[0] is new_thread
    assert workers[0].name == WORKER_NAME

    # 4) 重建后的循环可继续服务后续提交
    assert mgr._run_async(make_async_func("post-restart")) == "post-restart"


def test_engine_context_rebuilt_on_loop_replacement(monkeypatch):
    """外审 P2 回归：工作循环重建后，旧循环绑定的引擎/会话工厂必须整体重建。"""
    manager = compat.DBManagerCompat()

    created_engines = []
    makers = []

    class FakeEngine:
        def __init__(self):
            self.dispose_calls = 0

        async def dispose(self):
            self.dispose_calls += 1

    def fake_create_async_engine(url, **kwargs):
        engine = FakeEngine()
        created_engines.append(engine)
        return engine

    def fake_async_sessionmaker(engine, expire_on_commit=False):
        maker = object()
        makers.append((maker, engine))
        return maker

    monkeypatch.setattr(compat, "create_async_engine", fake_create_async_engine)
    monkeypatch.setattr(compat, "async_sessionmaker", fake_async_sessionmaker)

    assert manager._run_async(make_async_func(1)) == 1
    assert len(created_engines) == 1
    first_maker, first_engine = makers[0]
    assert manager._worker_context_loop is compat._worker_loop

    # 杀掉工作循环，模拟工作线程异常退出
    old_loop = compat._worker_loop
    old_thread = compat._worker_thread
    old_loop.call_soon_threadsafe(old_loop.stop)
    old_thread.join(timeout=5)

    assert manager._run_async(make_async_func(2)) == 2

    assert len(created_engines) == 2, "循环重建后必须重建引擎，不得复用旧循环绑定的引擎"
    second_maker, second_engine = makers[1]
    assert second_maker is not first_maker, "会话工厂必须随循环重建"
    assert second_engine is created_engines[1]
    assert manager._worker_context_loop is compat._worker_loop

    # 旧引擎的尽力释放应在新循环上被调度执行（短暂等待后检查）
    deadline = real_time.monotonic() + 2
    while first_engine.dispose_calls == 0 and real_time.monotonic() < deadline:
        real_time.sleep(0.02)
    assert first_engine.dispose_calls >= 1, "旧引擎应被尽力释放"


def test_concurrent_recovery_reuses_single_rebuild(monkeypatch):
    """外审 R2 回归：多个调用方携带同一死循环并发恢复时只重建一次，
    其余调用方复用已建健康循环，不遗留孤儿循环/多余工作线程。"""
    # 把全局循环替换为已关闭的裸循环，制造"两个调用方同时踩到死循环"的窗口
    dead_loop = asyncio.new_event_loop()
    dead_loop.close()
    placeholder_thread = threading.Thread(target=lambda: None, name=WORKER_NAME, daemon=True)
    placeholder_thread.start()  # 占位：模拟死循环对应的已退出线程槽位
    monkeypatch.setattr(compat, "_worker_loop", dead_loop)
    monkeypatch.setattr(compat, "_worker_thread", placeholder_thread)

    before = set(alive_worker_threads())
    barrier = threading.Barrier(2)
    outcomes: list = [None, None]

    def recover(slot):
        barrier.wait(timeout=5)  # 两个调用方对齐后同时进入恢复路径
        outcomes[slot] = compat._ensure_worker_loop(dead_loop)

    threads = [threading.Thread(target=recover, args=(i,), daemon=True) for i in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=10)
        assert not t.is_alive()

    loop_a, loop_b = outcomes
    assert loop_a is not None and loop_b is not None
    assert loop_a is loop_b, "并发恢复必须复用同一次重建的健康循环"
    assert loop_a is not dead_loop and not loop_a.is_closed()
    assert compat._worker_loop is loop_a
    new_threads = set(alive_worker_threads()) - before
    assert len(new_threads) == 1, f"并发恢复只应新增 1 根工作线程，实际 {len(new_threads)}"
    assert compat._worker_thread in new_threads

    # 收尾：停掉本用例触发的重建循环，避免污染后续用例的"单工作线程"计数
    loop_a.call_soon_threadsafe(loop_a.stop)
    compat._worker_thread.join(timeout=5)


def test_concurrent_callers_share_one_worker(mgr):
    """多个调用线程并发提交：各自拿到自己的结果，结束后仍只有 1 个工作线程。"""
    n = 6
    barrier = threading.Barrier(n)
    results = [None] * n
    errors = []

    def caller(i):
        try:
            async def _func(session_maker):
                await asyncio.sleep(0.01)  # 轻微让出，制造工作循环上的排队重叠
                return ("val", i)
            barrier.wait(timeout=5)  # 对齐后同时提交
            results[i] = mgr._run_async(_func)
        except BaseException as exc:  # pragma: no cover - 仅在异常时记录
            errors.append(exc)

    threads = [threading.Thread(target=caller, args=(i,), daemon=True) for i in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=15)
        assert not t.is_alive(), "调用线程应在超时前完成"

    assert errors == []
    assert results == [("val", i) for i in range(n)], "每个调用方拿到自己的结果，无串扰"

    workers = alive_worker_threads()
    assert len(workers) == 1, "并发后工作线程仍应为 1 个"
    assert workers[0] is compat._worker_thread


def test_db_concurrency_capped_at_pool_capacity(mgr):
    """外审 P1 回归：并发闸门把在库协程数封顶在池总容量内，结果无串扰。"""
    cap = compat._WORKER_DB_CONCURRENCY
    assert cap == 5, "闸门容量应等于池总容量（pool_size 2 + max_overflow 3）"

    n = 10
    barrier = threading.Barrier(n)
    results = [None] * n
    errors = []
    state = {"inflight": 0, "max_inflight": 0}
    state_lock = threading.Lock()

    def caller(i):
        try:
            async def _func(session_maker):
                with state_lock:
                    state["inflight"] += 1
                    state["max_inflight"] = max(state["max_inflight"], state["inflight"])
                await asyncio.sleep(0.15)
                with state_lock:
                    state["inflight"] -= 1
                return ("val", i)

            barrier.wait(timeout=5)  # 对齐后同时提交，制造最大并发压力
            results[i] = mgr._run_async(_func)
        except BaseException as exc:  # pragma: no cover - 仅在异常时记录
            errors.append(exc)

    threads = [threading.Thread(target=caller, args=(i,), daemon=True) for i in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=20)
        assert not t.is_alive(), "调用线程应在超时前完成"

    assert errors == []
    assert results == [("val", i) for i in range(n)], "每个调用方拿到自己的结果，无串扰"
    assert state["max_inflight"] <= cap, (
        f"在库协程数 {state['max_inflight']} 不得超过池总容量 {cap}"
    )
    assert state["inflight"] == 0, "所有协程收尾后在库计数应归零"


# ---------------------------------------------------------------------------
# 补充：惰性会话工厂契约 + 真实读库方法的最小桩集成
# ---------------------------------------------------------------------------

def test_worker_session_maker_lazy_created_once_and_cached(monkeypatch):
    """_ensure_worker_session_maker 惰性创建且缓存：引擎只建一次，池参数符合契约。"""
    manager = compat.DBManagerCompat()
    assert manager._worker_session_maker is None, "初始应为 None（惰性）"

    engine_calls = []

    class FakeEngine:
        pass

    def fake_create_async_engine(url, **kwargs):
        engine_calls.append((url, kwargs))
        return FakeEngine()

    sentinel_maker = object()

    def fake_async_sessionmaker(engine, expire_on_commit=False):
        assert isinstance(engine, FakeEngine)
        assert expire_on_commit is False
        return sentinel_maker

    monkeypatch.setattr(compat, "create_async_engine", fake_create_async_engine)
    monkeypatch.setattr(compat, "async_sessionmaker", fake_async_sessionmaker)

    # _ensure_worker_session_maker 绑定运行中的循环，须在事件循环内调用
    async def _call_twice():
        first = manager._ensure_worker_session_maker()
        second = manager._ensure_worker_session_maker()
        return first, second

    loop = asyncio.new_event_loop()
    try:
        first, second = loop.run_until_complete(_call_twice())
    finally:
        loop.close()

    assert first is sentinel_maker
    assert second is sentinel_maker, "第二次应命中缓存"
    assert len(engine_calls) == 1, "引擎只创建一次"
    url, kwargs = engine_calls[0]
    assert kwargs["pool_size"] == 2
    assert kwargs["max_overflow"] == 3
    assert kwargs["echo"] is False


def test_get_system_setting_returns_value_from_stubbed_session(mgr, fake_maker, forbid_real_engine):
    """get_system_setting 读到值时原样返回（scalar_one_or_none 消费面）。"""
    fake_maker.session.result = FakeResult(rowcount=0, scalar="some-value")

    assert mgr.get_system_setting("feature_flag", default="fallback") == "some-value"
    assert len(fake_maker.session.execute_calls) == 1


def test_get_system_setting_falls_back_to_default_when_none(mgr, fake_maker, forbid_real_engine):
    """get_system_setting 未命中（None）时返回 default。"""
    fake_maker.session.result = FakeResult(rowcount=0, scalar=None)

    assert mgr.get_system_setting("missing_key", default="fallback") == "fallback"
