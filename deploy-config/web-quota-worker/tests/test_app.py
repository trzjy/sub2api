"""``app.make_context`` 的 storage_state 类型契约与 ``/v1/probe`` 鉴权测试。

背景：Playwright 的 ``browser.new_context(storage_state=...)`` 会把 ``str``
当成磁盘文件路径去读取（``BrowserType._prepare_browser_context_params``）。
app 层若把 Go 服务传来的 JSON 字符串原样透传，会在 ``new_context`` 触发
``FileNotFoundError``，被适配器记成 ``browser_context_init_failed``。

因此这里用 mock 的 ``browser.new_context`` 做纯离线断言：
传给 Playwright 的 ``storage_state`` 必须是 ``dict``，非法值必须被静默忽略。
"""

import asyncio
import json
import logging
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import app as app_module  # noqa: E402 - 需先在 sys.path 中加入 worker 根目录

VALID_STATE = '{"cookies":[{"name":"k","value":"v","domain":".x.cn"}],"origins":[]}'


class _FakeBrowser:
    """记录 ``new_context`` 实参的假浏览器，不启动 Chromium。"""

    def __init__(self):
        self.calls = []

    async def new_context(self, **kwargs):
        self.calls.append(kwargs)
        return object()


@pytest.fixture()
def fake_browser(monkeypatch):
    browser = _FakeBrowser()

    async def _get_browser():
        return browser

    monkeypatch.setattr(app_module, "get_browser", _get_browser)
    return browser


def _make_context(**kwargs):
    return asyncio.run(app_module.make_context(**kwargs))


def test_json_string_storage_state_is_parsed_to_dict(fake_browser):
    """JSON 字符串必须解析成 dict 后才交给 browser.new_context。"""
    _make_context(storage_state=VALID_STATE)

    assert len(fake_browser.calls) == 1
    kwargs = fake_browser.calls[0]
    assert "storage_state" in kwargs
    passed = kwargs["storage_state"]
    assert not isinstance(passed, str)
    assert isinstance(passed, dict)
    assert passed == json.loads(VALID_STATE)


def test_dict_storage_state_passed_through(fake_browser):
    state = json.loads(VALID_STATE)
    _make_context(storage_state=state)

    assert fake_browser.calls[0]["storage_state"] == state
    assert isinstance(fake_browser.calls[0]["storage_state"], dict)


@pytest.mark.parametrize("empty", [None, "", "   ", "\n\t "])
def test_empty_storage_state_omits_key(fake_browser, empty):
    _make_context(storage_state=empty)

    assert "storage_state" not in fake_browser.calls[0]


SECRET = "secret_cookie_value_should_never_be_logged"


@pytest.mark.parametrize(
    "make_payload",
    [
        lambda s: "{" + s,                       # 非法 JSON
        lambda s: '["' + s + '"]',               # 合法 JSON 但不是 dict
        lambda s: "null" + s,                    # 尾部多余内容 -> 解析失败
        lambda s: "123" + s,                     # 同上
        lambda s: '"' + s + '"',                 # JSON 字符串而非 dict
        lambda s: ("{" + s).encode(),            # bytes，同样不得当路径
    ],
    ids=["truncated", "list", "null-suffix", "number-suffix", "bare-string", "bytes"],
)
def test_invalid_storage_state_is_ignored(fake_browser, make_payload, caplog):
    """非法 JSON / 非 dict 一律不传给 new_context，且不抛异常、不泄露原文。"""
    payload = make_payload(SECRET)

    with caplog.at_level(logging.WARNING, logger="web-quota-worker"):
        _make_context(storage_state=payload)

    assert len(fake_browser.calls) == 1
    assert "storage_state" not in fake_browser.calls[0]
    assert SECRET not in caplog.text
    assert "invalid storage_state json, ignoring" in caplog.text


def test_proxy_url_still_forwarded(fake_browser):
    _make_context(storage_state=VALID_STATE, proxy_url="http://proxy.local:8080")

    kwargs = fake_browser.calls[0]
    assert kwargs["proxy"] == {"server": "http://proxy.local:8080"}
    assert isinstance(kwargs["storage_state"], dict)


# ---------------------------------------------------------------------------
# storageState 结构校验：dict 结构非法时不得传给 Playwright
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "bad_state",
    [
        {"cookies": "not-a-list"},
        {"origins": {}},
        {"cookies": [1, 2]},
        {"cookies": [], "origins": ["not-a-dict"]},
        {"cookies": [{"name": "k"}], "origins": "oops"},
    ],
    ids=["cookies-str", "origins-dict", "cookies-int-items", "origins-str-items", "mixed"],
)
def test_structurally_invalid_dict_is_ignored(fake_browser, bad_state, caplog):
    """结构非法的 dict 来源必须丢弃，且日志不含任何内容片段。"""
    bad_state = dict(bad_state)
    bad_state["marker"] = SECRET

    with caplog.at_level(logging.WARNING, logger="web-quota-worker"):
        _make_context(storage_state=bad_state)

    assert len(fake_browser.calls) == 1
    assert "storage_state" not in fake_browser.calls[0]
    assert SECRET not in caplog.text
    assert "invalid storage_state json, ignoring" in caplog.text


@pytest.mark.parametrize(
    "good_state",
    [
        {"cookies": [], "origins": []},
        {"cookies": []},
        {"origins": []},
        {"cookies": [{"name": "k", "value": "v", "domain": ".x.cn"}], "origins": []},
        {"cookies": [], "origins": [], "unknown_key": "ignored"},
    ],
    ids=["both-empty", "cookies-only", "origins-only", "cookies-items", "extra-key"],
)
def test_structurally_valid_dict_is_forwarded(fake_browser, good_state, caplog):
    """合法 storageState 形态（含缺省键与额外键）必须正常传给 new_context。"""
    with caplog.at_level(logging.WARNING, logger="web-quota-worker"):
        _make_context(storage_state=good_state)

    assert len(fake_browser.calls) == 1
    assert fake_browser.calls[0]["storage_state"] == good_state
    assert "invalid storage_state json, ignoring" not in caplog.text


def test_json_string_with_invalid_structure_is_dropped(fake_browser, caplog):
    """JSON 字符串能解析成 dict，但结构非法时同样丢弃。"""
    payload = '{"cookies": "' + SECRET + '"}'

    with caplog.at_level(logging.WARNING, logger="web-quota-worker"):
        _make_context(storage_state=payload)

    assert len(fake_browser.calls) == 1
    assert "storage_state" not in fake_browser.calls[0]
    assert SECRET not in caplog.text
    assert "invalid storage_state json, ignoring" in caplog.text


def test_is_playwright_storage_state_rejects_non_dict():
    assert app_module._is_playwright_storage_state({}) is True
    assert app_module._is_playwright_storage_state([]) is False
    assert app_module._is_playwright_storage_state("x") is False


# ---------------------------------------------------------------------------
# /v1/probe 接口层：鉴权与 provider 白名单
# ---------------------------------------------------------------------------

_testclient = pytest.importorskip("fastapi.testclient")
TestClient = _testclient.TestClient


@pytest.fixture()
def client(monkeypatch):
    monkeypatch.setenv("WEB_QUOTA_WORKER_TOKEN", "test-token")
    return TestClient(app_module.app)


def test_probe_missing_token_returns_401(client):
    resp = client.post("/v1/probe", json={"provider": "compshare"})
    assert resp.status_code == 401


def test_probe_wrong_token_returns_401(client):
    resp = client.post(
        "/v1/probe",
        json={"provider": "compshare"},
        headers={"X-Worker-Token": "wrong-token"},
    )
    assert resp.status_code == 401


def test_probe_unknown_provider_returns_400(client):
    resp = client.post(
        "/v1/probe",
        json={"provider": "no-such-provider"},
        headers={"X-Worker-Token": "test-token"},
    )
    assert resp.status_code == 400
    assert "unknown provider" in resp.json()["detail"]


# ---------------------------------------------------------------------------
# finish 阶段凭据可用性校验（实跑 probe）：WCQ-LV-2
#
# 根因（2026-11-15 讯飞星辰实测）：verify_effective 只访问根路径并等 1 秒，
# 而根路径 4 秒内不校验登录，于是「看起来登上、实际未登录」的会话恒通过。
# 修复：finish 保存前用候选凭据在全新 context 实跑适配器 probe，
# status == "ok" 才保存；非 ok 返回 409 login_not_effective，且不关闭会话。
# ---------------------------------------------------------------------------

from adapters.base import ProbeResult  # noqa: E402


class _FakePage:
    def __init__(self, url):
        self.url = url

    async def title(self):
        return "Acct"


class _FakeContext:
    async def storage_state(self):
        return {"cookies": [], "origins": []}


class FakeSession:
    """可驱动 login_finish 走到 probe 判定的假会话。"""

    def __init__(self):
        self.id = "sid-fake"
        self.provider = "iflytek"
        self.login_url = "https://passport.xfyun.cn/login"
        self.success_url_prefix = "https://maas.xfyun.cn/account"
        self.proxy_url = "http://proxy.local:8080"
        self.page = _FakePage(self.success_url_prefix)
        self.context = _FakeContext()

    def authenticated(self):
        return True


class FakeRegistry:
    """记录 close 调用、返回固定假会话的假 registry。"""

    def __init__(self, session):
        self._session = session
        self.closed = []

    async def get(self, session_id):
        return self._session

    async def close(self, session_id):
        self.closed.append(session_id)


class FakeAdapter:
    """记录收到的 ProbeRequest 并返回指定 ProbeResult 的假适配器。"""

    def __init__(self, result):
        self.result = result
        self.received = None

    async def probe(self, req):
        self.received = req
        return self.result


async def _verify_effective_always_true(_session):
    return True


def _build_scenario(monkeypatch, probe_result):
    """装好假 registry / adapter / verify_effective，返回 (session, registry, adapter)。"""
    session = FakeSession()
    registry = FakeRegistry(session)
    monkeypatch.setattr(app_module, "_login_registry", registry)
    monkeypatch.setattr(app_module, "verify_effective", _verify_effective_always_true)

    adapter = FakeAdapter(probe_result)

    async def _fake_get_adapter(provider):
        return adapter

    monkeypatch.setattr(app_module, "get_adapter", _fake_get_adapter)
    return session, registry, adapter


def test_finish_probe_needs_interactive_auth_returns_409(client, monkeypatch):
    """probe 返回 needs_interactive_auth → 409 login_not_effective，且不关闭会话。"""
    result = ProbeResult(status="needs_interactive_auth", provider="iflytek", error="needs login")
    session, registry, adapter = _build_scenario(monkeypatch, result)

    resp = client.post(
        "/v1/login/sid-fake/finish",
        headers={"X-Worker-Token": "test-token"},
    )
    assert resp.status_code == 409
    assert resp.json() == {"status": "pending", "error": "login_not_effective"}

    # 失败关闭：不关闭会话、不回传 storage_state。
    assert registry.closed == []

    # probe 确实用了候选凭据，且 proxy_url 透传正确。
    assert adapter.received is not None
    assert adapter.received.proxy_url == session.proxy_url
    expected_state = json.dumps({"cookies": [], "origins": []})
    assert adapter.received.storage_state == expected_state


def test_finish_probe_ok_returns_200(client, monkeypatch):
    """probe 返回 ok → 200 authenticated，且带非空 storage_state、关闭会话。"""
    result = ProbeResult(status="ok", provider="iflytek", account_label="Acct")
    session, registry, adapter = _build_scenario(monkeypatch, result)

    resp = client.post(
        "/v1/login/sid-fake/finish",
        headers={"X-Worker-Token": "test-token"},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "authenticated"
    assert body["storage_state"]

    # 通过后关闭会话。
    assert registry.closed == ["sid-fake"]
