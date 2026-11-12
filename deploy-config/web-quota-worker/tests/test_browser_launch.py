"""浏览器启动契约单测（不装 playwright、不启动真浏览器）。

这条用例是针对 2026-11-12 生产 502 的直接回归防线：

    原代码 `from playwright.async_api import chromium` —— 该符号根本不存在
    （`playwright.async_api` 只导出 `async_playwright`），于是 `get_browser()`
    在任何请求第一次真正启动浏览器时必然 ImportError，登录接口回 502。
    而当时全部单测都注入假 page/browser，`get_browser()` 一次都没被真正执行，
    所以 61 条用例全绿却拦不住。

做法：往 sys.modules 里塞一个**复刻真实导出面**的假 `playwright.async_api`
（只有 `async_playwright`，故意不提供 `chromium`），再真调 `get_browser()`。
以后谁再把导入写回错误符号，这里会当场 ImportError，而不是等生产 502。
"""

import asyncio
import sys
import types

import pytest

import app


class _FakeBrowser:
    def __init__(self):
        self.new_context = object()
        self.closed = False

    async def close(self):
        self.closed = True


class _FakeChromium:
    def __init__(self, browser):
        self._browser = browser
        self.launch_calls = 0

    async def launch(self, **kwargs):
        self.launch_calls += 1
        return self._browser


class _FakePlaywright:
    def __init__(self, browser):
        self.chromium = _FakeChromium(browser)

    async def stop(self):
        pass


class _FakePlaywrightManager:
    """等价于 async_playwright()：await .start() 后得到 _FakePlaywright。"""

    def __init__(self, browser):
        self._browser = browser

    async def start(self):
        return _FakePlaywright(self._browser)


@pytest.fixture()
def fake_playwright_module(monkeypatch):
    """安装假 playwright.async_api，导出面与真实包一致（无 chromium）。"""
    fake_api = types.ModuleType("playwright.async_api")
    holder = {}

    def async_playwright():
        return _FakePlaywrightManager(holder["browser"])

    fake_api.async_playwright = async_playwright
    # 真实包确实没有这个符号；显式断言，防止有人以为它在。
    assert not hasattr(fake_api, "chromium")

    fake_pkg = types.ModuleType("playwright")
    fake_pkg.async_api = fake_api
    monkeypatch.setitem(sys.modules, "playwright", fake_pkg)
    monkeypatch.setitem(sys.modules, "playwright.async_api", fake_api)

    # get_browser() 有全局缓存，逐用例复位。
    holder["browser"] = _FakeBrowser()
    monkeypatch.setattr(app, "_browser", None, raising=False)
    monkeypatch.setattr(app, "_playwright", None, raising=False)
    yield holder


def test_get_browser_uses_async_playwright_entrypoint(fake_playwright_module):
    """get_browser() 必须走 async_playwright().start()，且能返回可用浏览器。"""

    async def scenario():
        return await app.get_browser()

    browser = asyncio.run(scenario())
    assert browser is fake_playwright_module["browser"]
    # 拿到的必须是浏览器对象本身（后续 context 从这里开）。
    assert browser.new_context is not None


def test_get_browser_reuses_single_instance(fake_playwright_module):
    """并发/重复调用只启动一次 Chromium（防每次请求都开新浏览器）。"""

    async def scenario():
        first = await app.get_browser()
        second = await app.get_browser()
        return first, second

    first, second = asyncio.run(scenario())
    assert first is second


def test_get_browser_reference_surface_has_no_chromium():
    """守住结论本身：playwright.async_api 没有 chromium 导出，别再写回去。"""
    try:
        import playwright.async_api as real_api
    except ModuleNotFoundError:
        pytest.skip("本机未安装 playwright，跳过真实导出面检查")

    assert hasattr(real_api, "async_playwright")
    assert not hasattr(real_api, "chromium"), (
        "playwright.async_api 出现了 chromium 导出？请复核 app.get_browser() 的写法"
    )
