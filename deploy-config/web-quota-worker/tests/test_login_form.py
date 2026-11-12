"""服务端登录「按字段填写」单测（不触网、不启动浏览器）。

这套逻辑解决的是 2026-11-12 线上的真实故障：让用户点缩略图坐标去聚焦输入框，
而「动态码」输入框只有 176×18 CSS 像素（缩到面板后约 7 像素高），点不中就打不进字。
改成「先枚举页面上的输入框/按钮，再按标签定位填写」。

三条必须守住的不变量：
1. 定位优先用 label / 文字，index 只是提示 —— 页面重绘（如「获取验证码」变成
   倒计时）会让下标整体位移，只认下标就会填错框；
2. 找不到目标时返回 False 而不是抛异常 —— 抛异常会让前端看到 5xx，正是被骂的点；
3. 真正下发的选择器必须是本模块当场打的标记，不能是站点选择器。
"""

import asyncio

import pytest

from login_sessions import click_button, describe_form, fill_field, toggle_field

INVENTORY = {
    "fields": [
        {"index": 0, "type": "tel", "label": "手机号", "filled": False, "masked": False},
        {"index": 1, "type": "number", "label": "动态码", "filled": False, "masked": False},
        {"index": 2, "type": "checkbox", "label": "我已阅读并同意用户协议", "filled": False, "masked": False},
    ],
    "buttons": [
        {"index": 0, "text": "获取验证码"},
        {"index": 1, "text": "登录"},
    ],
}


class FakeLocator:
    def __init__(self):
        self.filled = []
        self.clicks = 0

    @property
    def first(self):
        return self

    async def fill(self, text, timeout=None):
        self.filled.append(text)

    async def click(self, timeout=None):
        self.clicks += 1


class FakePage:
    def __init__(self, inventory=None, boom=False):
        self.inventory = inventory if inventory is not None else INVENTORY
        self.boom = boom
        self.locators = {}
        self.evaluate_calls = 0

    async def evaluate(self, _js, arg=None):
        self.evaluate_calls += 1
        self.last_arg = arg
        if self.boom:
            raise RuntimeError("page is navigating")
        return self.inventory

    def locator(self, selector):
        loc = FakeLocator()
        self.locators[selector] = loc
        return loc


class FakeSession:
    def __init__(self, page):
        self.page = page


def test_describe_form_returns_fields_and_buttons():
    page = FakePage()
    out = asyncio.run(describe_form(FakeSession(page)))
    assert [f["label"] for f in out["fields"]] == ["手机号", "动态码", "我已阅读并同意用户协议"]
    assert [b["text"] for b in out["buttons"]] == ["获取验证码", "登录"]


def test_describe_form_degrades_to_empty_on_navigation():
    """页面正在跳转时读不到结构：不抛错，但必须标记 available=False。

    这条是 2026-11-13 线上投诉的核心：读失败和「这页确实没有输入框」在旧实现里
    返回值一模一样，前端只能把空清单当事实显示「这个页面没有可填的输入框」，
    用户既不知道怎么回事也没有下一步。两者必须分得开。
    """
    out = asyncio.run(describe_form(FakeSession(FakePage(boom=True))))
    assert out["fields"] == []
    assert out["buttons"] == []
    assert out["available"] is False


def test_describe_form_reports_available_when_page_really_empty():
    """页面确实没有输入框：available=True，前端才可以说「这页没有可填的框」。"""
    page = FakePage(inventory={"fields": [], "buttons": [{"index": 0, "text": "手机验证码登录"}]})
    out = asyncio.run(describe_form(FakeSession(page)))
    assert out["available"] is True
    assert [b["text"] for b in out["buttons"]] == ["手机验证码登录"]


def test_describe_form_retries_once_before_giving_up():
    """跳转中的那一下读不到就重试，别把一次抖动当成页面事实。"""

    class FlakyPage(FakePage):
        def __init__(self):
            super().__init__()
            self.calls = 0

        async def evaluate(self, js, arg=None):
            self.calls += 1
            if self.calls == 1:
                raise RuntimeError("page is navigating")
            return await super().evaluate(js, arg)

    page = FlakyPage()
    out = asyncio.run(describe_form(FakeSession(page)))
    assert out["available"] is True
    assert [b["text"] for b in out["buttons"]] == ["获取验证码", "登录"]


def test_fill_matches_by_label_not_index():
    """页面重排后 index 已错位，仍须填进用户看到的那个框。"""
    page = FakePage()
    ok = asyncio.run(fill_field(FakeSession(page), index=0, text="13985850877", expected_label="动态码"))
    assert ok is True
    # 动态码在清单里是第 1 个，标记也必须打在 1 上
    assert '[data-wq-field="1"]' in page.locators
    assert '[data-wq-field="0"]' not in page.locators
    assert page.locators['[data-wq-field="1"]'].filled == ["13985850877"]


def test_fill_falls_back_to_index_when_label_missing():
    page = FakePage()
    ok = asyncio.run(fill_field(FakeSession(page), index=1, text="1234", expected_label=""))
    assert ok is True
    assert page.locators['[data-wq-field="1"]'].filled == ["1234"]


def test_fill_returns_false_when_target_gone():
    """找不到目标不许抛异常：抛异常会变成前端看到的 5xx。"""
    page = FakePage()
    ok = asyncio.run(fill_field(FakeSession(page), index=0, text="x", expected_label="已消失的框"))
    assert ok is False
    assert page.locators == {}


def test_click_matches_by_text():
    page = FakePage()
    ok = asyncio.run(click_button(FakeSession(page), index=5, expected_text="登录"))
    assert ok is True
    assert page.locators['[data-wq-btn="1"]'].clicks == 1


def test_click_returns_false_when_text_gone():
    page = FakePage()
    ok = asyncio.run(click_button(FakeSession(page), index=0, expected_text="提交"))
    assert ok is False


def test_selectors_are_only_our_own_markers():
    """防回归：下发的选择器必须是本模块打的标记，不能出现站点类名。"""
    page = FakePage()
    asyncio.run(fill_field(FakeSession(page), index=0, text="a", expected_label="手机号"))
    asyncio.run(click_button(FakeSession(page), index=0, expected_text="登录"))
    for selector in page.locators:
        assert selector.startswith("[data-wq-"), selector


def test_toggle_uses_label_and_reports_true():
    """勾选协议：必须打到 checkbox 那个下标上，且返回 True。"""
    page = FakePage()
    ok = asyncio.run(
        toggle_field(FakeSession(page), index=0, expected_label="我已阅读并同意用户协议")
    )
    assert ok is True
    # 协议框在清单里是第 2 个
    assert page.last_arg == 2


def test_toggle_returns_false_when_item_gone():
    page = FakePage()
    ok = asyncio.run(toggle_field(FakeSession(page), index=0, expected_label="不存在的勾选项"))
    assert ok is False
