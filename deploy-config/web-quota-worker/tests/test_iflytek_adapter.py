"""iflytek 适配器离线单元测试（不触网、不启动浏览器）。

仅覆盖模块级纯函数 ``build_probe_data`` 与账号标识脱敏，符合派发单 IF-WO-1 §2 要求。
code==4001 的判定在上层 ``_probe_inner``，不在纯函数内；本文件仅验证
纯函数的结构映射与脱敏语义。
"""

import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from adapters.iflytek import IflytekAdapter, build_probe_data


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
