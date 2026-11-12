"""服务端登录会话离线单测（不触网、不启动浏览器）。

覆盖最容易被改坏的三条不变量：
1. 登录成功只认「URL 落在成功前缀内」，别拿 cookie 名字猜；
2. 点击/输入/按键确实转发到同一个页面上下文（不是各自新开页面）；
3. 会话 TTL 到期后必须拒绝并释放上下文（防浏览器泄漏）。

注意：每个用例整体跑在同一个事件循环里（asyncio.run 包住整个用例），
因为登记表内部的 asyncio.Lock 会绑定首次使用的 loop。
"""

import asyncio
import base64

import pytest

from login_sessions import (
    LOGIN_SESSION_TTL_SECONDS,
    LoginSessionError,
    LoginSessionRegistry,
    capture,
    press_key,
    tap,
    type_text,
)

FAKE_JPEG = b"\xff\xd8jpegbytes"


class FakePage:
    def __init__(self, url: str = "https://passport.example.com/login"):
        self.url = url
        self.goto = _async_mock()
        self.mouse = _Recorder()
        self.keyboard = _Recorder()
        self.screenshot = _async_mock(return_value=FAKE_JPEG)

    def title(self):
        return "Console"


class _Recorder:
    """记录调用参数的最小替身（click/type/press/move/wheel 共用）。"""

    def __init__(self):
        self.calls = []

    def _record(self, name, *args, **kwargs):
        self.calls.append((name, args, kwargs))

    def __getattr__(self, name):
        def _call(*args, **kwargs):
            self._record(name, *args, **kwargs)

            async def _noop():
                return None

            return _noop()

        return _call


def _async_mock(return_value=None, side_effect=None):
    class _Mock:
        def __init__(self):
            self.calls = []

        async def __call__(self, *args, **kwargs):
            self.calls.append((args, kwargs))
            if side_effect is not None:
                result = side_effect()
                if asyncio.iscoroutine(result):
                    return await result
                return result
            return return_value

        def assert_awaited_once(self):
            assert len(self.calls) == 1, f"expected 1 call, got {len(self.calls)}"

        def assert_awaited_once_with(self, *args, **kwargs):
            assert len(self.calls) == 1, f"expected 1 call, got {len(self.calls)}"
            assert self.calls[0] == (args, kwargs)

    return _Mock()


class FakeContext:
    def __init__(self, page: FakePage):
        self.page = page
        self.closed = False
        self.new_page = _async_mock(return_value=page)
        self.storage_state = _async_mock(return_value={"cookies": [], "origins": []})

        async def _close():
            self.closed = True

        self.close = _async_mock(side_effect=_close)


def _factory(context: FakeContext):
    calls = []

    async def factory(_storage_state=None, _proxy_url=None, viewport=None):
        calls.append(viewport)
        return context

    factory.calls = calls
    return factory


async def _start(registry, provider="longcat", success_prefix="https://console.example.com/"):
    return await registry.start(
        provider=provider,
        login_url="https://passport.example.com/login",
        success_url_prefix=success_prefix,
    )


def test_start_uses_fixed_viewport():
    async def case():
        page = FakePage()
        factory = _factory(FakeContext(page))
        registry = LoginSessionRegistry(factory)
        session = await _start(registry)
        assert factory.calls[0] == {"width": 1280, "height": 800}
        assert page.goto.calls and page.goto.calls[0][0][0] == "https://passport.example.com/login"
        assert session.authenticated() is False

    asyncio.run(case())


def test_authenticated_requires_leaving_login_page_first():
    async def case():
        page = FakePage()
        registry = LoginSessionRegistry(_factory(FakeContext(page)))
        session = await _start(registry)

        # 还没跳登录页：即便 URL 落在成功前缀内也不能算登录成功。
        page.url = "https://console.example.com/usage"
        assert session.authenticated() is False

        # 走到登录页（离开前缀）→ 再回到控制台 → 才算成功。
        page.url = "https://passport.example.com/login"
        assert session.authenticated() is False
        page.url = "https://console.example.com/usage"
        assert session.authenticated() is True

        # 相似域名不算命中（前缀匹配而非包含匹配）。
        page.url = "https://console.example.com.evil.test/usage"
        assert session.authenticated() is False

        page.url = ""
        assert session.authenticated() is False

    asyncio.run(case())


def test_longcat_login_url_inside_success_prefix_not_mistaken_as_signed_in():
    """longcat 的 login_url 本身就在 success 前缀内（/platform/usage 属于 /platform/），
    必须靠「先离开过」才能区分游客会话与登录成功。"""

    async def case():
        page = FakePage("https://longcat.chat/platform/usage")
        registry = LoginSessionRegistry(_factory(FakeContext(page)))
        session = await _start(
            registry, provider="longcat", success_prefix="https://longcat.chat/platform/"
        )

        assert session.authenticated() is False

        page.url = "https://passport.meituan.com/account/unitivelogin"
        assert session.authenticated() is False

        page.url = "https://longcat.chat/platform/usage"
        assert session.authenticated() is True

    asyncio.run(case())


def test_actions_forward_to_same_page():
    async def case():
        page = FakePage()
        registry = LoginSessionRegistry(_factory(FakeContext(page)))
        session = await _start(registry, provider="compshare")

        await tap(session, 120.5, 240)
        assert page.mouse.calls[-1] == ("click", (120.5, 240.0), {})

        await type_text(session, "13800000000")
        assert page.keyboard.calls[-1] == ("type", ("13800000000",), {"delay": 20})

        await press_key(session, "Enter")
        assert page.keyboard.calls[-1] == ("press", ("Enter",), {})

        with pytest.raises(LoginSessionError):
            await press_key(session, "Control+W")

    asyncio.run(case())


def test_capture_returns_base64_without_data_prefix():
    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        session = await _start(registry, provider="sensenova")
        shot = await capture(session)
        assert shot == base64.b64encode(FAKE_JPEG).decode("ascii")
        assert not shot.startswith("data:")

    asyncio.run(case())


def test_expired_session_is_rejected_and_context_closed():
    async def case():
        context = FakeContext(FakePage())
        registry = LoginSessionRegistry(_factory(context))
        session = await _start(registry)

        session.created_at -= LOGIN_SESSION_TTL_SECONDS + 1
        with pytest.raises(LoginSessionError) as excinfo:
            await registry.get(session.id)
        assert excinfo.value.code == "session_expired"
        assert context.closed is True

    asyncio.run(case())


def test_unknown_session_id_raises_not_found():
    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        with pytest.raises(LoginSessionError) as excinfo:
            await registry.get("nope")
        assert excinfo.value.code == "session_not_found"

    asyncio.run(case())
