"""纯常量/纯函数测试：短信 Tab 识别必须覆盖讯飞「手机快捷登录」。

不触网、不启动浏览器。
"""
from login_sessions import LOGIN_SMS_BUTTON_HINTS


def test_phone_quick_login_in_hints():
    assert "手机快捷登录" in LOGIN_SMS_BUTTON_HINTS


def test_real_iflytek_tab_texts_match_only_sms_tab():
    sms_tab = "手机快捷登录"
    other_tab = "账号密码登录"

    # 真实讯飞 Tab 文案做包含匹配：只有短信 Tab 命中。
    assert any(hint in sms_tab for hint in LOGIN_SMS_BUTTON_HINTS)
    assert not any(hint in other_tab for hint in LOGIN_SMS_BUTTON_HINTS)
