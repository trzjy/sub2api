"""动作之后「等页面停下来再截图」的单测（不触网、不启动浏览器）。

修的是 2026-11-13 的线上投诉：点「手机验证码登录」后立刻截图，截到的是点击
之前的画面，用户看到「点了没反应」，于是反复点、点错地方，结论是「点不到」。

三条必须守住的不变量：
1. 页面稳定（形状指纹连续静默）就立刻返回，白等就是白花用户时间；
2. 页面一直在变（加载中）也要有上限，不能把请求拖死；
3. 读不到指纹（跳转中 evaluate 报错）不许抛异常 —— 那会让用户拿到 5xx。
"""

import asyncio

import login_sessions
from login_sessions import LOGIN_SETTLE_QUIET_MS, LOGIN_SETTLE_TIMEOUT_MS, settle


class _Clock:
    """假表：让 settle 的等待在测试里瞬间走完，但仍按真实逻辑推进。"""

    def __init__(self):
        self.now = 0.0

    def time(self):
        return self.now


class _FakeLoop:
    def __init__(self, clock):
        self._clock = clock

    def time(self):
        return self._clock.now


class _FakePage:
    """signatures 依次返回指纹，用完后重复最后一个。"""

    def __init__(self, signatures, boom=False):
        self.signatures = list(signatures)
        self.boom = boom
        self.calls = 0

    async def evaluate(self, _js):
        self.calls += 1
        if self.boom:
            raise RuntimeError("page is navigating")
        if len(self.signatures) > 1:
            return self.signatures.pop(0)
        return self.signatures[0]


class _FakeSession:
    def __init__(self, page):
        self.page = page
        self.closed = False


def _elapsed(monkeypatch, page):
    """在假时钟里跑一次 settle，返回（耗时秒, 页面调用情况）。"""
    clock = _Clock()

    async def _fake_sleep(seconds):
        clock.now += seconds

    async def _main():
        monkeypatch.setattr(login_sessions.asyncio, "get_running_loop", lambda: _FakeLoop(clock))
        monkeypatch.setattr(login_sessions.asyncio, "sleep", _fake_sleep)
        started = clock.now
        await settle(_FakeSession(page))
        return clock.now - started

    return asyncio.run(_main()), page


def test_settle_returns_once_signature_stops_changing(monkeypatch):
    """指纹连续静默 400ms 就返回，不等满 3 秒。"""
    page = _FakePage(["43|0|a", "135|3|a"])  # 点完之后页面重排一次
    elapsed, fake = _elapsed(monkeypatch, page)
    assert fake.calls >= 2
    assert elapsed < LOGIN_SETTLE_TIMEOUT_MS / 1000.0


def test_settle_gives_up_at_deadline_when_page_keeps_changing(monkeypatch):
    """页面一直在变（一直加载）也要封顶，不能无限等。"""

    class _Growing(_FakePage):
        async def evaluate(self, _js):
            self.calls += 1
            return f"{self.calls}|0|a"

    elapsed, _ = _elapsed(monkeypatch, _Growing([]))
    assert elapsed >= LOGIN_SETTLE_TIMEOUT_MS / 1000.0
    assert elapsed <= LOGIN_SETTLE_TIMEOUT_MS / 1000.0 + 0.5


def test_settle_swallows_evaluate_errors(monkeypatch):
    """读不到指纹只意味着「不知道稳没稳」，不能变成异常。"""
    elapsed, _ = _elapsed(monkeypatch, _FakePage(["x"], boom=True))
    assert elapsed <= LOGIN_SETTLE_TIMEOUT_MS / 1000.0 + 0.5


def test_quiet_window_is_shorter_than_deadline():
    """静默窗口必须明显小于总上限，否则等于每次都等满。"""
    assert LOGIN_SETTLE_QUIET_MS < LOGIN_SETTLE_TIMEOUT_MS / 2
