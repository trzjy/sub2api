"""iflytek 适配器离线单元测试（不触网、不启动浏览器）。

仅覆盖模块级纯函数 ``build_probe_data`` 与账号标识脱敏，符合派发单 IF-WO-1 §2 要求。
code==4001 的判定在上层 ``_probe_inner``，不在纯函数内；本文件仅验证
纯函数的结构映射与脱敏语义。
"""

import asyncio
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from adapters.base import ProbeRequest
from adapters.iflytek import (
    IflytekAdapter,
    build_prepaid_quota,
    build_probe_data,
    _build_fetch_js,
)


# ---------------------------------------------------------------------------
# 假浏览器 / 假页面：离线跑通 _probe_inner 的取数判定逻辑，不触网、不启真浏览器。
# ---------------------------------------------------------------------------
class _FakePage:
    def __init__(self, raw):
        self._raw = raw
        self.url = "https://maas.xfyun.cn/account"

    async def goto(self, *args, **kwargs):
        return None

    async def evaluate(self, js):
        return self._raw

    async def close(self):
        return None


class _FakeContext:
    def __init__(self, raw):
        self._raw = raw

    async def new_page(self):
        return _FakePage(self._raw)

    async def close(self):
        return None

    async def storage_state(self):
        return {}


def _ok_entry(data):
    """构造页面取数得到的单接口条目（ok + code=0 + data）。"""
    return {"ok": True, "code": 0, "data": data}


def _base_raw():
    """五个既有接口全部为空 data（与 §2.6 实测一致）。"""
    return {
        "balance": _ok_entry({}),
        "token_plan_quota": _ok_entry({}),
        "token_plan_seats": _ok_entry({}),
        "coding_plan_list": _ok_entry({}),
        "user": _ok_entry(_USER),
    }


_FREE_QUOTA_SAMPLE = {
    "ok": True,
    "code": 0,
    "data": {
        "claim": {
            "appId": "7d0dbc7e",
            "freeQuota": {
                "effectiveAt": "2026-11-12 17:09:37",
                "expiresAt": "2026-12-13 17:09:37",
                "quotaStatus": "ACTIVE",
                "remainingTokens": 238395567,
                "totalTokens": 1000000000,
                "usageAvailable": True,
                "usedTokens": 761604433,
            },
        },
        "modelName": "Spark-X2.6",
    },
    "failed": False,
}


async def _run_probe(raw, console_model=""):
    """用假 context 直跑 _probe_inner。"""
    req = ProbeRequest(
        provider="iflytek", console_username="x", storage_state="{}"
    )
    req.console_model = console_model
    adapter = IflytekAdapter(lambda *a, **k: None)
    return await adapter._probe_inner(req, _FakeContext(raw))



# ---------------------------------------------------------------------------
# 夹具：与 §2.6 实测一致的 5 个接口内层 data
# ---------------------------------------------------------------------------
_BALANCE = {"balance": 0, "delinquent": 0, "virtualBalance": 0}
_TOKEN_QUOTA = {
    "quotas": [
        {
            "seatType": 1,
            "seatTypeName": "标准",
            "totalCount": 0,
            "assignedCount": 0,
            "remainingCount": 0,
        }
    ]
}
_TOKEN_SEATS = {"seats": [], "total": 0}
_CODING_LIST = {"page": 1, "size": 10, "total": 0, "rows": []}
_USER = {
    "id": 123,
    "uid": "u-1",
    "mobile": "13985858877",
    "login": "13985858877",
    "nickname": "讯飞用户",
    "email": "a@b.com",
}


# ---------------------------------------------------------------------------
# 1. 正常输入 → data 结构与 auth_state == "ok"
# ---------------------------------------------------------------------------
def test_build_probe_data_normal():
    data = build_probe_data(
        _BALANCE, _TOKEN_QUOTA, _TOKEN_SEATS, _CODING_LIST, _USER
    )
    assert data["auth_state"] == "ok"
    assert data["balance"] == _BALANCE
    assert data["token_plan_quota"] == _TOKEN_QUOTA
    assert data["token_plan_seats"] == _TOKEN_SEATS
    assert data["coding_plan_list"] == _CODING_LIST
    # account_label 由 mobile 脱敏而来
    assert data["account_label"] == "139****8877"


# ---------------------------------------------------------------------------
# 2. account_label 脱敏：手机号保留前 3 后 4，输出不得含完整号码
# ---------------------------------------------------------------------------
def test_account_label_mask_mobile():
    user = {"mobile": "13985858877", "nickname": "x", "login": "x"}
    data = build_probe_data({}, {"quotas": []}, {"seats": [], "total": 0}, {}, user)
    label = data["account_label"]
    assert label == "139****8877"
    assert "13985858877" not in label
    # 序列化后的 data 也绝不含完整手机号
    blob = json.dumps(data, ensure_ascii=False)
    assert "13985858877" not in blob


def test_account_label_fallback_no_mobile():
    # 无 mobile 时优先 nickname，其次 login
    assert (
        build_probe_data({}, {"quotas": []}, {"seats": [], "total": 0}, {}, {"nickname": "昵称A"})[
            "account_label"
        ]
        == "昵称A"
    )
    assert (
        build_probe_data({}, {"quotas": []}, {"seats": [], "total": 0}, {}, {"login": "登录名B"})[
            "account_label"
        ]
        == "登录名B"
    )
    assert (
        build_probe_data({}, {"quotas": []}, {"seats": [], "total": 0}, {}, {})["account_label"]
        == ""
    )


# ---------------------------------------------------------------------------
# 3. user 含 accessKey/secretKey → data 中不得出现这些键
# ---------------------------------------------------------------------------
def test_no_accesskey_secretkey_in_data():
    user = {
        "mobile": "13985858877",
        "accessKey": "AKID-plaintext-should-not-leak",
        "secretKey": "secret-plaintext-should-not-leak",
    }
    data = build_probe_data(
        _BALANCE, _TOKEN_QUOTA, _TOKEN_SEATS, _CODING_LIST, user
    )
    blob = json.dumps(data, ensure_ascii=False)
    assert "accessKey" not in blob
    assert "secretKey" not in blob
    assert "AKID-plaintext-should-not-leak" not in blob
    assert "secret-plaintext-should-not-leak" not in blob


# ---------------------------------------------------------------------------
# 4. 纯函数只做结构映射：缺失/非 dict 入参有安全兜底，不抛异常
# ---------------------------------------------------------------------------
def test_build_probe_data_missing_inputs_safe():
    data = build_probe_data(None, None, None, None, None)
    assert data["auth_state"] == "ok"
    assert data["balance"] == {}
    assert data["token_plan_quota"] == {"quotas": []}
    assert data["token_plan_seats"] == {"seats": [], "total": 0}
    assert data["coding_plan_list"] == {"page": 1, "size": 10, "total": 0, "rows": []}
    assert data["account_label"] == ""


# ---------------------------------------------------------------------------
# 5. provider 标识正确（不触网，仅校验类属性）
# ---------------------------------------------------------------------------
def test_provider_attribute():
    assert IflytekAdapter.provider == "iflytek"


# ---------------------------------------------------------------------------
# 6. IFQ-2：free-quota 预授权额度采集
# ---------------------------------------------------------------------------
def test_build_prepaid_quota_sample():
    """build_prepaid_quota 纯函数：样本 → 正确数值与北京时间转 UTC。"""
    pq = build_prepaid_quota("spark-x2.6", _FREE_QUOTA_SAMPLE)
    assert pq is not None
    assert pq["model"] == "spark-x2.6"
    assert pq["total_tokens"] == 1000000000.0
    assert pq["used_tokens"] == 761604433.0
    assert pq["remaining_tokens"] == 238395567.0
    # 北京时间 2026-12-13 17:09:37 → UTC 09:09:37
    assert pq["expires_at"] == "2026-12-13T09:09:37Z"
    assert pq["status"] == "ACTIVE"
    assert pq["available"] is True


def test_build_prepaid_quota_invalid_returns_none():
    """结构不符 / totalTokens<=0 → None。"""
    assert build_prepaid_quota("m", None) is None
    assert build_prepaid_quota("m", {"ok": False, "error": "x"}) is None
    assert build_prepaid_quota("m", {"ok": True, "code": 4000, "data": {}}) is None
    assert build_prepaid_quota("m", {"ok": True, "code": 0, "data": {}}) is None
    no_total = {
        "ok": True,
        "code": 0,
        "data": {"claim": {"freeQuota": {"totalTokens": 0, "remainingTokens": 0}}},
    }
    assert build_prepaid_quota("m", no_total) is None


def test_probe_with_console_model_includes_prepaid_quota():
    """console_model 非空且 free-quota 返回样本 → data 含正确 prepaid_quota。"""
    raw = _base_raw()
    raw["free_quota"] = _FREE_QUOTA_SAMPLE
    result = asyncio.run(_run_probe(raw, console_model="spark-x2.6"))
    assert result.status == "ok"
    pq = result.data.get("prepaid_quota")
    assert pq is not None
    assert pq["model"] == "spark-x2.6"
    assert pq["total_tokens"] == 1000000000.0
    assert pq["used_tokens"] == 761604433.0
    assert pq["remaining_tokens"] == 238395567.0
    assert pq["expires_at"] == "2026-12-13T09:09:37Z"
    assert pq["status"] == "ACTIVE"
    assert pq["available"] is True


def test_console_model_empty_no_free_quota():
    """console_model 为空 → 请求列表不含 free-quota；结果无该字段。"""
    js = _build_fetch_js("")
    assert "free_quota" not in js
    assert "free-quota" not in js
    # 实际跑一遍：raw 里没有 free_quota 键，结果也不该有 prepaid_quota
    result = asyncio.run(_run_probe(_base_raw(), console_model=""))
    assert result.status == "ok"
    assert "prepaid_quota" not in result.data


def test_free_quota_model_not_found_still_ok():
    """free-quota 返回 code=4000（MODEL_NOT_FOUND）→ 探测仍 ok，且无 prepaid_quota。"""
    raw = _base_raw()
    raw["free_quota"] = {"ok": True, "code": 4000, "data": {}}
    result = asyncio.run(_run_probe(raw, console_model="spark-x2.6"))
    assert result.status == "ok"
    assert "prepaid_quota" not in result.data

