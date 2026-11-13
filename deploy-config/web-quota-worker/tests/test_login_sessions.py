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
import time

import pytest

from login_sessions import (
    LOGIN_PHASE_FAILED,
    LOGIN_PHASE_OPENING,
    LOGIN_PHASE_READY,
    LOGIN_SESSION_IDLE_SECONDS,
    LOGIN_SESSION_MAX_AGE_SECONDS,
    LoginSessionError,
    LoginSessionRegistry,
    capture,
    press_key,
    tap,
    type_text,
    verify_effective,
)

FAKE_JPEG = b"\xff\xd8jpegbytes"


class FakePage:
    def __init__(self, url: str = "https://passport.example.com/login"):
        self.url = url
        self.goto = _async_mock()
        self.mouse = _Recorder()
        self.keyboard = _Recorder()
        self.screenshot = _async_mock(return_value=FAKE_JPEG)
        self.wait_for_timeout = _async_mock()

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


async def _await_open(session):
    """等后台「打开页面」任务跑完（start 现在不等页面，见下方用例说明）。"""
    task = session.open_task
    assert task is not None, "start 必须把打开页面的任务挂在 session.open_task 上"
    await task


def test_start_returns_immediately_with_opening_phase():
    """start 只建会话就必须返回：打开登录页要几十秒，同步等会把前端请求拖超时。

    这是 2026-11-13 线上故障的回归用例——用户看到「网络错误」、登录面板永不出现，
    根因就是 start 等页面打开等过了前端 30s 超时。
    """

    async def case():
        page = FakePage()

        async def _slow_goto(*_args, **_kwargs):
            await asyncio.sleep(5)

        page.goto = _async_mock(side_effect=_slow_goto)
        registry = LoginSessionRegistry(_factory(FakeContext(page)))

        started = asyncio.get_running_loop().time()
        session = await _start(registry)
        elapsed = asyncio.get_running_loop().time() - started

        assert elapsed < 1.0, f"start 必须秒回，实际耗时 {elapsed:.2f}s"
        assert session.phase == LOGIN_PHASE_OPENING
        assert session.open_task is not None
        session.open_task.cancel()

    asyncio.run(case())


def test_open_page_sets_ready_phase():
    async def case():
        page = FakePage()
        registry = LoginSessionRegistry(_factory(FakeContext(page)))
        session = await _start(registry)
        await _await_open(session)
        assert session.phase == LOGIN_PHASE_READY
        assert session.phase_error == ""
        assert page.goto.calls[0][0][0] == "https://passport.example.com/login"

    asyncio.run(case())


def test_open_page_failure_sets_failed_phase_without_raising():
    """打不开页面不能把后台任务抛崩，也不能让 state 接口 5xx：写进 phase 报给前端。"""

    async def case():
        page = FakePage()

        async def _boom(*_args, **_kwargs):
            raise RuntimeError("net down")

        page.goto = _async_mock(side_effect=_boom)
        registry = LoginSessionRegistry(_factory(FakeContext(page)))
        session = await _start(registry)
        await _await_open(session)
        assert session.phase == LOGIN_PHASE_FAILED
        assert session.phase_error == "login_page_unreachable"

    asyncio.run(case())


def test_close_cancels_pending_open_task():
    async def case():
        page = FakePage()

        async def _slow_goto(*_args, **_kwargs):
            await asyncio.sleep(30)

        page.goto = _async_mock(side_effect=_slow_goto)
        context = FakeContext(page)
        registry = LoginSessionRegistry(_factory(context))
        session = await _start(registry)
        task = session.open_task

        await registry.close(session.id)
        assert context.closed is True
        await asyncio.sleep(0)
        assert task.cancelled() or task.done()

    asyncio.run(case())


def test_start_uses_fixed_viewport():
    async def case():
        page = FakePage()
        factory = _factory(FakeContext(page))
        registry = LoginSessionRegistry(factory)
        session = await _start(registry)
        # 打开页面在后台任务里跑，断言前先等它完成
        await _await_open(session)
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
        # 人类化打字改为逐字符输入（2026-11-13 反风控增强），
        # 测试验证最后一个字符被正确输入即可。
        assert page.keyboard.calls[-1] == ("type", ("0",), {})
        # 验证总共调用了 11 次（11 个字符）
        type_calls = [c for c in page.keyboard.calls if c[0] == "type"]
        assert len(type_calls) == 11

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

        session.created_at -= LOGIN_SESSION_MAX_AGE_SECONDS + 1
        with pytest.raises(LoginSessionError) as excinfo:
            await registry.get(session.id)
        assert excinfo.value.code == "session_expired"
        assert context.closed is True

    asyncio.run(case())


def test_session_survives_long_usage_but_not_the_absolute_cap():
    """会话寿命必须是「滑动的」：用户一直在操作，就不能被到期踢下线。

    旧实现把寿命写死成「从打开面板起算 600 秒」，任何操作都不延长它，
    于是用户操作到第 10 分钟会话连同浏览器一起被删，登录成果全丢
    （2026-11-13 线上：用户操作了 9 分多钟，跨线后 finish 全部 404/502）。
    """

    async def case():
        context = FakeContext(FakePage())
        registry = LoginSessionRegistry(_factory(context))
        session = await _start(registry)

        # 模拟「用了很久」：创建时间很老，但刚刚还有交互（last_touch 是新的）。
        session.created_at -= LOGIN_SESSION_MAX_AGE_SECONDS - 10
        session.last_touch = time.monotonic()
        assert session.expired() is False

        # 但绝对上限仍然生效：到了就必须回收（内存不能无限占）。
        session.created_at -= LOGIN_SESSION_MAX_AGE_SECONDS + 1
        assert session.expired() is True

    asyncio.run(case())


def test_idle_session_expires_after_idle_window():
    """长时间没有任何交互（面板开着、人走开了）到期要回收。"""

    async def case():
        context = FakeContext(FakePage())
        registry = LoginSessionRegistry(_factory(context))
        session = await _start(registry)

        session.last_touch -= LOGIN_SESSION_IDLE_SECONDS + 1
        assert session.expired() is True

    asyncio.run(case())


def test_unknown_session_id_raises_not_found():
    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        with pytest.raises(LoginSessionError) as excinfo:
            await registry.get("nope")
        assert excinfo.value.code == "session_not_found"

    asyncio.run(case())


# ---------------------------------------------------------------------------
# 生效性校验：网址看着对不代表真的登上了（2026-11-13 美团扫码失败实证）
# ---------------------------------------------------------------------------


class _VerifyPage:
    """用于 verify_effective 的验证页替身：goto 之后停在 land_on。"""

    def __init__(self, land_on, raise_on_goto=False):
        self.url = "about:blank"
        self._land_on = land_on
        self._raise = raise_on_goto
        self.closed = False

    async def goto(self, url, **_kwargs):
        if self._raise:
            raise RuntimeError("net down")
        self.url = self._land_on

    async def wait_for_timeout(self, _ms):
        return None

    async def close(self):
        self.closed = True


async def _verify(session, verify_page):
    context = session.context
    context.new_page = _async_mock(return_value=verify_page)
    return await verify_effective(session)


def test_verify_effective_rejects_session_bounced_back_to_login():
    """被弹回登录域名 = 这次登录没生效，必须判失败（否则会存垃圾凭据）。"""

    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        session = await _start(registry, success_prefix="https://console.example.com/")
        page = _VerifyPage("https://passport.example.com/login")
        assert await _verify(session, page) is False
        assert page.closed is True

    asyncio.run(case())


def test_verify_effective_accepts_session_that_stays_on_console():
    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        session = await _start(registry, success_prefix="https://console.example.com/")
        page = _VerifyPage("https://console.example.com/usage")
        assert await _verify(session, page) is True

    asyncio.run(case())


def test_verify_effective_fails_open_when_check_itself_errors():
    """校验过程本身出错（网络抖动）不能把好登录判死。"""

    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        session = await _start(registry, success_prefix="https://console.example.com/")
        page = _VerifyPage("", raise_on_goto=True)
        assert await _verify(session, page) is True

    asyncio.run(case())


def test_verify_effective_skips_when_no_prefix_declared():
    async def case():
        registry = LoginSessionRegistry(_factory(FakeContext(FakePage())))
        session = await _start(registry, success_prefix="")
        page = _VerifyPage("https://passport.example.com/login")
        assert await _verify(session, page) is True
        assert page.closed is False

    asyncio.run(case())
