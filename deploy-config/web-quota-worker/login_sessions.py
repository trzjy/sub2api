"""服务端登录会话：给「必须人工完成」的登录方式（扫码 / 短信验证码 / 滑块）
提供一条远程画面通道。

背景
----
longcat 只有扫码 / 短信登录，compshare / sensenova 的默认页也是短信登录；
这三家都无法由 worker 静默完成登录。既有实现把这件事推给用户本机的
``local_captcha_helper``（桌面通知 + 有头浏览器），代价是「必须在装了 helper
的那台电脑上操作，服务器上点不动」。

本模块把这一步搬回服务端：
- 服务端打开登录页，把画面截成 JPEG 回传；
- 用户在管理端网页上点坐标 / 打字 / 回车（全部转发到同一个页面上下文）；
- 登录成功后 ``finish`` 一次性回传 storageState，由 Go 侧落库。

设计纪律
--------
- 无状态：会话只存内存，进程重启即失效；绝不落盘 cookie / 截图。
- 不猜站点结构：不写任何站点 selector，所有交互按坐标与键盘事件转发，
  上游改版不需要改本模块（这是「不为每家写一套登录脚本」的唯一做法）。
- 日志只记 session 前缀 / provider / status，绝不打印画面、URL 参数与 cookie。
"""

from __future__ import annotations

import asyncio
import base64
import secrets
import time
from typing import Any, Awaitable, Callable, Optional

# 会话总寿命（秒）：与 Go 侧会话绑定有效期一致，超时后 context 必须释放。
LOGIN_SESSION_TTL_SECONDS = 600

# 固定视口：前端把点击坐标按「显示宽度 / 视口宽度」等比换算，
# 视口一旦可变，坐标就会错位，故这里写死。deviceScaleFactor 固定 1，
# 保证截图像素与 CSS 像素一一对应。
LOGIN_VIEWPORT_WIDTH = 1280
LOGIN_VIEWPORT_HEIGHT = 800

# 截图压缩参数：JPEG 质量越低体积越小，但短信验证码 / 二维码仍需可辨认。
LOGIN_SHOT_QUALITY = 62

# 连续无操作的自动释放时间（秒）：防止浏览器上下文长期占用内存。
LOGIN_SESSION_IDLE_SECONDS = 300

ContextFactory = Callable[[Optional[str], Optional[str]], Awaitable[Any]]


class LoginSessionError(Exception):
    """会话不存在 / 已过期 / 内部失败。"""

    def __init__(self, code: str):
        self.code = code
        super().__init__(code)


class LoginSession:
    """一次服务端登录会话：一个独立 BrowserContext + 一个页面。"""

    def __init__(
        self,
        session_id: str,
        provider: str,
        context: Any,
        page: Any,
        success_url_prefix: str,
        login_url: str,
    ) -> None:
        self.id = session_id
        self.provider = provider
        self.context = context
        self.page = page
        self.success_url_prefix = success_url_prefix
        self.login_url = login_url
        self.created_at = time.monotonic()
        self.last_touch = self.created_at
        self.closed = False
        # 是否曾离开过控制台成功前缀（= 真的跳过登录页）。
        # 必要性：longcat 的 login_url 本身就落在 success_url_prefix 内
        # （/platform/usage 属于 /platform/），只看「当前 URL 命中前缀」会在
        # 还没跳转到登录页的瞬间误判为已登录，从而把游客会话当成登录态回传。
        self.left_success_prefix = False

    def touch(self) -> None:
        self.last_touch = time.monotonic()

    def expired(self, now: Optional[float] = None) -> bool:
        now = time.monotonic() if now is None else now
        if now - self.created_at > LOGIN_SESSION_TTL_SECONDS:
            return True
        return now - self.last_touch > LOGIN_SESSION_IDLE_SECONDS

    def authenticated(self) -> bool:
        """是否已经完成登录：先离开过登录页，再回到控制台成功前缀内。

        两段判定缺一不可：
        - 只看「当前 URL 命中前缀」会误判——longcat 未登录时 URL 也在前缀内；
        - 只看「曾经离开」也不够——用户可能只是点了别的站外链接。
        不猜 cookie 名字：cookie 名会变，URL 前缀是产品自己声明的成功落点。
        """
        url = (self.page.url or "").strip()
        prefix = self.success_url_prefix.strip()
        if not url or not prefix:
            return False
        if not url.startswith(prefix):
            self.left_success_prefix = True
            return False
        return self.left_success_prefix


class LoginSessionRegistry:
    """进程内会话表。所有方法都做惰性过期清理。"""

    def __init__(self, context_factory: ContextFactory) -> None:
        self._context_factory = context_factory
        self._sessions: dict[str, LoginSession] = {}
        self._lock = asyncio.Lock()

    async def start(
        self,
        provider: str,
        login_url: str,
        success_url_prefix: str,
        proxy_url: Optional[str] = None,
        storage_state: Optional[str] = None,
        context_factory: Optional[Callable[..., Awaitable[Any]]] = None,
    ) -> LoginSession:
        # 登录画面必须固定视口：坐标换算的前端逻辑依赖「截图像素 = CSS 像素」。
        factory = context_factory or self._context_factory
        viewport = {"width": LOGIN_VIEWPORT_WIDTH, "height": LOGIN_VIEWPORT_HEIGHT}
        context = await factory(storage_state, proxy_url, viewport=viewport)
        page = await context.new_page()
        session = LoginSession(
            session_id=secrets.token_urlsafe(18),
            provider=provider,
            context=context,
            page=page,
            success_url_prefix=success_url_prefix,
            login_url=login_url,
        )
        try:
            await page.goto(login_url, wait_until="domcontentloaded", timeout=45000)
        except Exception:
            # 打开失败也要让用户看到当前画面（可能是空白页），由前端「重新加载」重试。
            pass

        async with self._lock:
            await self._prune_locked(time.monotonic())
            self._sessions[session.id] = session
        return session

    async def get(self, session_id: str) -> LoginSession:
        async with self._lock:
            now = time.monotonic()
            session = self._sessions.get(session_id)
            if session is None or session.closed:
                await self._prune_locked(now)
                raise LoginSessionError("session_not_found")
            if session.expired(now):
                # 「过期」与「不存在」必须分开：前者要提示用户重新登录，
                # 后者通常意味着前端拿着旧会话号在轮询，两者提示语不同。
                self._sessions.pop(session_id, None)
                session.closed = True
                await self._close_context(session)
                raise LoginSessionError("session_expired")
            await self._prune_locked(now)
        session.touch()
        return session

    async def close(self, session_id: str) -> None:
        async with self._lock:
            session = self._sessions.pop(session_id, None)
        if session is None:
            return
        session.closed = True
        try:
            await session.context.close()
        except Exception:  # noqa: BLE001 - 关闭失败不向上抛
            pass

    async def _close_context(self, session: LoginSession) -> None:
        try:
            await session.context.close()
        except Exception:  # noqa: BLE001 - 关闭失败不向上抛
            pass

    async def _prune_locked(self, now: float) -> None:
        stale = [sid for sid, s in self._sessions.items() if s.expired(now) or s.closed]
        for sid in stale:
            session = self._sessions.pop(sid, None)
            if session is None:
                continue
            session.closed = True
            await self._close_context(session)

    async def close_all(self) -> None:
        async with self._lock:
            sessions = list(self._sessions.values())
            self._sessions.clear()
        for session in sessions:
            session.closed = True
            try:
                await session.context.close()
            except Exception:  # noqa: BLE001
                pass


# ---------------------------------------------------------------------------
# 动作原语：全部转发到同一页面，不判断站点结构
# ---------------------------------------------------------------------------


async def capture(session: LoginSession) -> str:
    """截当前视口，返回 base64 JPEG（不带 data: 前缀）。"""
    raw = await session.page.screenshot(
        type="jpeg", quality=LOGIN_SHOT_QUALITY, full_page=False
    )
    return base64.b64encode(raw).decode("ascii")


async def tap(session: LoginSession, x: float, y: float) -> None:
    await session.page.mouse.click(float(x), float(y))


async def type_text(session: LoginSession, text: str) -> None:
    await session.page.keyboard.type(text, delay=20)


async def press_key(session: LoginSession, key: str) -> None:
    # 只放行常用键，避免把任意键盘事件（含快捷键）透传到页面。
    allowed = {
        "Enter",
        "Backspace",
        "Tab",
        "Escape",
        "ArrowLeft",
        "ArrowRight",
        "ArrowUp",
        "ArrowDown",
        "Delete",
    }
    if key not in allowed:
        raise LoginSessionError("key_not_allowed")
    await session.page.keyboard.press(key)


async def scroll(session: LoginSession, delta_y: float) -> None:
    bounded = max(-2000.0, min(2000.0, float(delta_y)))
    await session.page.mouse.move(LOGIN_VIEWPORT_WIDTH / 2, LOGIN_VIEWPORT_HEIGHT / 2)
    await session.page.mouse.wheel(0, bounded)


async def reload(session: LoginSession) -> None:
    try:
        await session.page.goto(
            session.login_url, wait_until="domcontentloaded", timeout=30000
        )
    except Exception:  # noqa: BLE001 - 重新加载失败保留当前画面
        pass


async def snapshot_storage_state(session: LoginSession) -> str:
    """返回 Playwright storageState JSON 字符串（只在 finish 成功后调用）。"""
    import json

    state = await session.context.storage_state()
    return json.dumps(state)


def account_label(session: LoginSession, fallback: str = "") -> str:
    """尽量给一个人类可读的账号标识；取不到就走 fallback。

    只读页面标题与 URL 主机名，绝不读取 DOM 中的手机号/邮箱等 PII。
    """
    title = (session.page.title() if session.page else "") or ""
    return title.strip()[:120] or fallback
