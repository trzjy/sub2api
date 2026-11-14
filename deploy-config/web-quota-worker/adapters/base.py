"""统一适配器基类与探测契约。

本模块定义 web-quota-worker 与上游具体适配器之间的契约。
后续适配器卡（compshare / sensenova / longcat / iflytek）必须继承 ``BaseAdapter``
并严格遵循下面的数据模型。

设计纪律（与方案 §3.1 一致）：
- worker 无状态：不落盘任何凭据（storage_state / 密码 / cookie 均只在内存中）。
- 只接收 ``api_key_sha256``，绝不接收明文 api_key。
- 适配器只回传刷新后的 storageState 与结构化用量，由调用方负责加密落库。
"""

from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable, Optional

# 受支持的上游标识。顺序固定，app.py 据此校验与分派。
# 已注册：compshare / sensenova / longcat / iflytek（讯飞星辰 MaaS，见方案 §3.2 ③）。
PROVIDERS = ("compshare", "sensenova", "longcat", "iflytek")

# browser_context_factory 类型：给定可选 storage_state 与 proxy，
# 返回一个可用的 Playwright BrowserContext（调用方负责关闭）。
BrowserContextFactory = Callable[
    ..., Awaitable["Any"]
]


@dataclass
class ProbeRequest:
    """由 sub2api 传入的探测请求。worker 无状态，不落盘任何凭据。"""

    provider: str
    storage_state: Optional[str] = None  # JSON 字符串（Playwright storageState）
    console_username: Optional[str] = None
    console_password: Optional[str] = None
    console_login_url: str = ""
    success_url_prefix: str = ""
    api_key_sha256: str = ""  # 仅哈希，绝不传明文 api_key
    proxy_url: Optional[str] = None
    timeout_seconds: int = 60


@dataclass
class ProbeResult:
    """探测响应。status 取值见下方常量。"""

    status: str  # "ok" | "needs_interactive_auth" | "auth_failed" | "error"
    provider: str
    updated_storage_state: Optional[str] = None
    account_label: str = ""
    data: dict[str, Any] = field(default_factory=dict)  # provider 专属结构化用量
    auth_reason: str = ""  # captcha_required / interactive_login_required / password_rejected ...
    error: str = ""  # 脱敏错误说明
    elapsed_ms: int = 0


class BaseAdapter:
    """所有上游适配器的统一基类。

    ``browser_context_factory`` 由 app.py 注入，签名为::

        async def factory(storage_state: Optional[str], proxy_url: Optional[str])
            -> playwright.async_api.BrowserContext

    适配器在 ``probe`` 内通过该工厂获取每请求独立的 BrowserContext，
    并在结束时负责 ``await context.close()``。
    """

    provider: str = ""

    def __init__(self, browser_context_factory: BrowserContextFactory):
        self._browser_context_factory = browser_context_factory

    async def probe(self, req: ProbeRequest) -> ProbeResult:
        raise NotImplementedError
