"""「首屏直接落到短信登录」的单测（不触网、不启动浏览器）。

背景（2026-11-13 线上）：longcat 的登录页默认是二维码，而在服务端浏览器会话里
扫码一律被上游判「抱歉，登录失败」，画面里的「手机验证码登录」又因为结构差异
不一定能被枚举成按钮 —— 用户就被关在扫码页里出不来。

约定：进来先落到后端指定的登录方式（loginModes 第一个），替用户点掉那一下。
"""

import asyncio

import login_sessions
from login_sessions import select_login_mode


class _FakePage:
    """按 inventory 序列返回页面结构；click 之后切到下一份结构。"""

    def __init__(self, inventories):
        self.inventories = list(inventories)
        self.index = 0
        self.locators = []
        self.clicks = []

    def _current(self):
        return self.inventories[min(self.index, len(self.inventories) - 1)]

    async def evaluate(self, js, arg=None):
        # 页面形状指纹（settle 用）与表单清单走同一个 evaluate，按 JS 内容区分。
        if "data-wq-btn" in js or "collectButton" in js:
            return self._current()
        return "sig"

    def locator(self, selector):
        page = self

        class _Loc:
            @property
            def first(self):
                return self

            async def click(self, timeout=None):
                page.clicks.append(selector)
                page.index += 1

        page.locators.append(selector)
        return _Loc()


class _FakeSession:
    def __init__(self, page):
        self.page = page
        self.closed = False


SMS_FORM = {
    "available": True,
    "fields": [
        {"index": 0, "type": "tel", "label": "手机号", "filled": False, "masked": False},
        {"index": 1, "type": "number", "label": "动态码", "filled": False, "masked": False},
    ],
    "buttons": [{"index": 0, "text": "+86"}, {"index": 1, "text": "获取手机动态码"}],
}

QR_PAGE = {
    "available": True,
    "fields": [],
    "buttons": [
        {"index": 0, "text": "《用户协议》"},
        {"index": 1, "text": "手机验证码登录"},
    ],
}

EMPTY_PAGE = {"available": True, "fields": [], "buttons": []}


def test_switches_to_sms_when_entry_exists():
    """页面上有「手机验证码登录」就替用户点它，点完算切到位。"""
    page = _FakePage([QR_PAGE, SMS_FORM])
    ok = asyncio.run(select_login_mode(_FakeSession(page), "sms"))
    assert ok is True
    assert page.clicks, "必须真的点过那个入口"
    assert page.clicks[0].startswith("[data-wq-btn="), "下发的只能是本模块自己打的标记"


def test_does_not_click_when_already_on_sms_form():
    """已经是短信表单就别再点，白点一下会把表单切走。"""
    page = _FakePage([SMS_FORM])
    ok = asyncio.run(select_login_mode(_FakeSession(page), "sms"))
    assert ok is True
    assert page.clicks == []


def test_returns_false_without_entry_and_never_raises():
    """读不到入口时保持页面原样，不许抛异常（否则登录面板直接打不开）。"""
    page = _FakePage([EMPTY_PAGE])
    ok = asyncio.run(select_login_mode(_FakeSession(page), "sms"))
    assert ok is False
    assert page.clicks == []


def test_ignores_other_modes():
    """只处理 sms：qrcode / password 交给页面默认落点，不擅自点。"""
    page = _FakePage([QR_PAGE, SMS_FORM])
    ok = asyncio.run(select_login_mode(_FakeSession(page), "qrcode"))
    assert ok is False
    assert page.clicks == []


def test_click_failure_is_not_fatal():
    """入口在、但点不动（元素被遮住等）：返回 False，不抛。"""

    class _BrokenClick(_FakePage):
        def locator(self, selector):
            page = self

            class _Loc:
                @property
                def first(self):
                    return self

                async def click(self, timeout=None):
                    raise RuntimeError("element is not clickable")

            page.locators.append(selector)
            return _Loc()

    ok = asyncio.run(select_login_mode(_FakeSession(_BrokenClick([QR_PAGE, SMS_FORM])), "sms"))
    assert ok is False


def test_switch_attempts_are_bounded():
    """登录页一直没挂出入口时，重试次数要有上限，不能把首帧拖到天荒地老。"""
    assert 1 <= login_sessions.LOGIN_MODE_SWITCH_ATTEMPTS <= 5
