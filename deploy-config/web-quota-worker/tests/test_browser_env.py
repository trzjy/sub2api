"""浏览器环境对齐单测（不启动真浏览器）。

2026-11-13 线上：美团扫码后回「抱歉，登录失败」，但用户自己的浏览器能登。
实测容器里的裸 Playwright 对外是：

    webdriver=true / UA 明写 HeadlessChrome / languages=en-US@posix /
    timezone=UTC / window.chrome=undefined / plugins=0

上游风控看到这组特征直接判机器人。所以 make_context 必须补齐语言、时区、
UA 与浏览器对象；这里守住这几条，防止以后被谁「顺手简化」掉。
"""

import asyncio

import pytest

import app


class _FakeContext:
    def __init__(self):
        self.scripts = []

    async def add_init_script(self, script):
        self.scripts.append(script)


class _FakeBrowser:
    def __init__(self):
        self.kwargs = None
        self.context = _FakeContext()

    async def new_context(self, **kwargs):
        self.kwargs = kwargs
        return self.context


@pytest.fixture()
def fake_browser(monkeypatch):
    browser = _FakeBrowser()

    async def _get_browser():
        return browser

    monkeypatch.setattr(app, "get_browser", _get_browser)
    return browser


def _make_context(**kwargs):
    async def scenario():
        return await app.make_context(**kwargs)

    return asyncio.run(scenario())


def test_context_looks_like_a_normal_chinese_desktop(fake_browser):
    ctx = _make_context(viewport={"width": 1280, "height": 800})

    kwargs = fake_browser.kwargs
    assert "HeadlessChrome" not in kwargs["user_agent"]
    assert "Chrome/" in kwargs["user_agent"]
    assert kwargs["locale"] == "zh-CN"
    assert kwargs["timezone_id"] == "Asia/Shanghai"
    assert kwargs["extra_http_headers"]["Accept-Language"].startswith("zh-CN")
    # 登录会话要固定视口，否则截图坐标换算会错
    assert kwargs["viewport"] == {"width": 1280, "height": 800}
    assert kwargs["device_scale_factor"] == 1
    assert ctx is fake_browser.context


def test_context_patches_webdriver_and_chrome_object(fake_browser):
    _make_context()
    scripts = fake_browser.context.scripts
    assert scripts, "必须注入环境补齐脚本，否则 navigator.webdriver 仍为 true"
    joined = "\n".join(scripts)
    assert "webdriver" in joined
    assert "window.chrome" in joined
    assert "zh-CN" in joined


def test_context_passes_proxy_and_storage_state(fake_browser):
    _make_context(proxy_url="http://127.0.0.1:7890", storage_state={"cookies": [], "origins": []})
    kwargs = fake_browser.kwargs
    assert kwargs["proxy"] == {"server": "http://127.0.0.1:7890"}
    assert kwargs["storage_state"] == {"cookies": [], "origins": []}


def test_browser_launch_disables_automation_flag(monkeypatch):
    """启动参数必须去掉 --enable-automation（那是 navigator.webdriver 的来源）。"""

    class _Chromium:
        def __init__(self):
            self.kwargs = None

        async def launch(self, **kwargs):
            self.kwargs = kwargs
            return object()

    class _Playwright:
        def __init__(self):
            self.chromium = _Chromium()

    class _Manager:
        def __init__(self, pw):
            self._pw = pw

        async def start(self):
            return self._pw

    pw = _Playwright()
    import sys
    import types

    fake_api = types.ModuleType("playwright.async_api")
    fake_api.async_playwright = lambda: _Manager(pw)
    fake_pkg = types.ModuleType("playwright")
    fake_pkg.async_api = fake_api
    monkeypatch.setitem(sys.modules, "playwright", fake_pkg)
    monkeypatch.setitem(sys.modules, "playwright.async_api", fake_api)
    monkeypatch.setattr(app, "_browser", None, raising=False)
    monkeypatch.setattr(app, "_playwright", None, raising=False)

    asyncio.run(app.get_browser())

    assert "--enable-automation" in pw.chromium.kwargs["ignore_default_args"]
    assert any("AutomationControlled" in arg for arg in pw.chromium.kwargs["args"])
