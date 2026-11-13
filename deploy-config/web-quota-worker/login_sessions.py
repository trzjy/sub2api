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
- 不猜站点结构：不写任何**站点** selector。文字输入走「运行时给页面上的
  input 打自己的标记（``data-wq-field`` / ``data-wq-btn``）再按标记填写」，
  标记由本模块现场生成、下一帧即失效，因此上游改版不需要改本模块。
  坐标点击只用于二维码 / 图形验证码这类无法用文字表达的控件。
- 日志只记 session 前缀 / provider / status，绝不打印画面、URL 参数与 cookie。

为什么不再「一切靠坐标点击」（2026-11-12 线上实证）
--------------------------------------------------
第一版让用户点缩略图坐标来聚焦输入框、再按键输入。实测 longcat 的美团
passport 登录页里，「动态码」输入框只有 **176×18 CSS 像素**，缩到管理端
面板（约 0.38 倍）后只剩 7 像素高，鼠标几乎不可能点中；点不中就相当于
没聚焦，后续输入全部丢失——用户看到的现象就是「根本登录不上」。
所以文字输入改为按字段填写（``fill_field``），坐标只保留作兜底。
"""

from __future__ import annotations

import asyncio
import base64
import json
import secrets
import time
from typing import Any, Awaitable, Callable, Optional

# 会话寿命（秒）：两条规则，都按「滑动」计算——用户一直在用就一直有效。
#
# 为什么不能是「从打开面板起算的固定窗口」：
# 真人完成一次登录要等短信验证码、可能切到别的标签页查资料、可能改错一次重来，
# 10 分钟是常态而不是异常。旧规则把会话寿命写死 600 秒且**任何操作都不延长它**，
# 于是用户在面板上操作到第 9 分 59 秒时，会话连同浏览器一起被删：
# 登录成果（cookie）当场蒸发，界面还一直说「未登录」，用户重登多少次都一样
# （2026-11-13 线上实录：用户操作了 9 分多钟，跨线后 finish 全部 404/502）。
#
# 现在两条规则：
# - 空闲超时：距上一次有效交互（轮询或任何动作）超过这个时间才回收。
#   面板开着、人在等短信或思考，都算「还在用」。
# - 绝对上限：从创建起最多存活这么久，保证浏览器上下文一定被释放
#   （内存不能无限占）。到点必收，与用户是否还在用无关。
LOGIN_SESSION_IDLE_SECONDS = 900
LOGIN_SESSION_MAX_AGE_SECONDS = 1800

# 固定视口：前端把点击坐标按「显示宽度 / 视口宽度」等比换算，
# 视口一旦可变，坐标就会错位，故这里写死。deviceScaleFactor 固定 1，
# 保证截图像素与 CSS 像素一一对应。
LOGIN_VIEWPORT_WIDTH = 1280
LOGIN_VIEWPORT_HEIGHT = 800

# 截图压缩参数：JPEG 质量越低体积越小，但短信验证码 / 二维码仍需可辨认。
LOGIN_SHOT_QUALITY = 62

# 读取页面表单结构的超时（秒）：页面卡住时不能让整帧请求被拖死。
LOGIN_INVENTORY_TIMEOUT = 3.0
# 读失败时重试一次：登录页在跳转完成的瞬间常读不到结构，一次失败就报
# 「这个页面没有可填的输入框」是错的（那是读失败，不是页面事实）。
LOGIN_INVENTORY_ATTEMPTS = 2
LOGIN_INVENTORY_RETRY_WAIT = 0.35

# 动作之后等页面「停下来」再截图。
#
# 为什么必须等：点一下「手机验证码登录」后，SPA 要到下一帧才把短信表单渲染出来。
# 动作一返回就截图，截到的是动作生效前的画面 —— 用户看到的是「点了没反应」，
# 于是反复点、点到别的地方，最后结论是「这个破玩意儿点不到」（2026-11-13 线上投诉）。
# 判定「停下来」用页面形状指纹（元素数 + 输入框数 + URL）：连续 QUIET 毫秒不变即稳定。
LOGIN_SETTLE_TIMEOUT_MS = 3000
LOGIN_SETTLE_QUIET_MS = 400
LOGIN_SETTLE_POLL_MS = 100

# 首屏切登录方式的重试次数：登录页刚渲染时可能还没挂出切换入口，
# 等一拍再看一次；再读不到就放弃（保持页面原样，不阻塞面板出现）。
LOGIN_MODE_SWITCH_ATTEMPTS = 3

# 打开登录页：容器出口网络偶发不通（实测出现过 chrome-error://chromewebdata/），
# 一次失败就给用户错误页等于「登录不上」，所以重试一次。
# 注意总耗时必须留在 Go 侧 45s 超时之内：2 次 × 18s + 退避 + 首帧等待 ≈ 40s。
LOGIN_OPEN_ATTEMPTS = 2
LOGIN_GOTO_TIMEOUT_MS = 18000

# 页面生命周期阶段（与「是否已登录」是两个互相独立的事实，不要塞进同一个字段）。
#
# 为什么必须分开上报：打开登录页要经过「启动浏览器 → 导航 → SPA 首帧」，
# 实测 longcat 单次要 16~30 秒，超过前端 30s 请求超时。早期把这段慢操作放在
# /v1/login/start 里同步等待，结果就是前端先掐断（用户看到假的「网络错误」），
# 会话号还没拿到，登录面板永远不出现（2026-11-13 线上故障）。
# 现在 start 只创建会话即返回，页面在后台打开，前端轮询拿 phase 显示进度。
LOGIN_PHASE_OPENING = "opening"
LOGIN_PHASE_READY = "ready"
LOGIN_PHASE_FAILED = "failed"

ContextFactory = Callable[[Optional[str], Optional[str]], Awaitable[Any]]


class LoginSessionError(Exception):
    """会话不存在 / 已过期 / 内部失败。

    ``payload`` 用来把「会话没了但登录成果还在」这件事带出去：
    会话过期回收前会抢救一次 storage_state（页面已经是登录态的话），
    由后端决定要不要落库——不能因为会话到点就把用户刚登进去的成果扔掉。
    """

    def __init__(self, code: str, payload: Optional[dict] = None):
        self.code = code
        self.payload = payload or {}
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
        prefer_login_mode: str = "",
    ) -> None:
        self.id = session_id
        self.provider = provider
        self.context = context
        self.page = page
        self.success_url_prefix = success_url_prefix
        self.login_url = login_url
        # 首选登录方式（sms / qrcode / password）。打开页面后由本模块替用户先切过去：
        # 「进来先落在哪个登录 Tab」是产品决策，不该让每个用户手动切一次。
        # 空串表示不干预，页面默认落哪就是哪。
        self.prefer_login_mode = (prefer_login_mode or "").strip().lower()
        self.created_at = time.monotonic()
        self.last_touch = self.created_at
        self.closed = False
        # 页面生命周期：opening（正在打开）→ ready / failed。与 authenticated() 无关。
        self.phase = LOGIN_PHASE_OPENING
        # 打不开时的原因码（给前端显示人话用，不当作协议解析）。
        self.phase_error = ""
        self.open_task: Optional[asyncio.Task] = None
        # 是否曾离开过控制台成功前缀（= 真的跳过登录页）。
        # 必要性：longcat 的 login_url 本身就落在 success_url_prefix 内
        # （/platform/usage 属于 /platform/），只看「当前 URL 命中前缀」会在
        # 还没跳转到登录页的瞬间误判为已登录，从而把游客会话当成登录态回传。
        self.left_success_prefix = False

    def touch(self) -> None:
        self.last_touch = time.monotonic()

    def expired(self, now: Optional[float] = None) -> bool:
        """会话是否该被回收。

        两条规则都跟「用户还在不在用」有关：
        - 绝对上限：从创建起超过 LOGIN_SESSION_MAX_AGE_SECONDS 必收（保证内存释放）；
        - 空闲超时：距上一次有效交互超过 LOGIN_SESSION_IDLE_SECONDS 才收。
          ``last_touch`` 由每次请求刷新，所以「一直有人在用」= 一直不过期。
        """
        now = time.monotonic() if now is None else now
        if now - self.created_at > LOGIN_SESSION_MAX_AGE_SECONDS:
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
        login_mode: str = "",
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
            prefer_login_mode=login_mode,
        )

        async with self._lock:
            await self._prune_locked(time.monotonic())
            self._sessions[session.id] = session
        # 关键：不在这里等页面打开。打开登录页最慢要几十秒，同步等就会把
        # 前端请求拖过它的超时阈值，用户拿到的是「网络错误」而不是登录画面。
        # 会话号先返回，页面在后台打开，前端靠 phase 显示「正在打开」。
        session.open_task = asyncio.create_task(self._open_login_page(session))
        return session

    async def _open_login_page(self, session: LoginSession) -> None:
        """在后台把登录页导航到可交互状态，并推进 session.phase。

        不抛异常：后台任务抛出去没人接，只会变成一条噪音日志；失败写进
        phase/phase_error，由 state 轮询报给前端。
        """
        page = session.page
        navigated = False
        try:
            # 容器出口网络偶发不通（实测出现过 chrome-error://chromewebdata/），
            # 一次失败就给用户一个错误页等于「登录不上」，所以重试一次。
            for attempt in range(LOGIN_OPEN_ATTEMPTS):
                try:
                    await page.goto(
                        session.login_url,
                        wait_until="domcontentloaded",
                        timeout=LOGIN_GOTO_TIMEOUT_MS,
                    )
                    navigated = True
                    break
                except Exception:
                    if attempt == LOGIN_OPEN_ATTEMPTS - 1:
                        break
                    await page.wait_for_timeout(1200)
            # SPA 登录页在 domcontentloaded 之后才渲染并跳转（实测 longcat 首帧
            # 仍停在 /platform/usage 的骨架页），稍等再让前端截首帧，否则是白屏。
            await page.wait_for_timeout(1500)
            # 再等页面稳定才置 ready：ready 的含义是「这扇页面已经能看、能操作」，
            # 不是「导航请求回来了」。不等就绪就报 ready，首帧会是半渲染的空壳，
            # 表单清单也因此读成空 —— 用户看到「没有可填的输入框」却毫无办法。
            if not session.closed:
                await settle(session)
            # 首屏直接落到用户要的登录方式（longcat 默认是二维码，而扫码在服务端
            # 会话里过不了风控）。失败不影响任何事，页面保持原样。
            if not session.closed and session.prefer_login_mode:
                await select_login_mode(session, session.prefer_login_mode)
        except asyncio.CancelledError:
            raise
        except Exception:  # noqa: BLE001 - 失败降级，不向上抛
            navigated = False
        if session.closed:
            return
        if navigated:
            session.phase = LOGIN_PHASE_READY
        else:
            # 打不开也让用户看到当前画面（可能是错误页），由前端「刷新画面」重试。
            session.phase = LOGIN_PHASE_FAILED
            session.phase_error = "login_page_unreachable"

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
                # 回收浏览器之前先抢救一次登录成果：页面已经是登录态的话，
                # 把 cookie 抓下来交给后端。用户登进去过就是登进去过，
                # 不能因为会话到点就把成果一起扔掉，逼他重收一遍短信验证码。
                rescued = await self._rescue_storage_state(session)
                await self._close_context(session)
                raise LoginSessionError("session_expired", payload=rescued)
            await self._prune_locked(now)
        session.touch()
        return session

    async def close(self, session_id: str) -> None:
        async with self._lock:
            session = self._sessions.pop(session_id, None)
        if session is None:
            return
        session.closed = True
        self._cancel_open_task(session)
        try:
            await session.context.close()
        except Exception:  # noqa: BLE001 - 关闭失败不向上抛
            pass

    @staticmethod
    def _cancel_open_task(session: LoginSession) -> None:
        """关会话时取消还没跑完的「打开页面」任务，避免它继续摸已关闭的页面。"""
        task = session.open_task
        session.open_task = None
        if task is not None and not task.done():
            task.cancel()

    async def _close_context(self, session: LoginSession) -> None:
        self._cancel_open_task(session)
        try:
            await session.context.close()
        except Exception:  # noqa: BLE001 - 关闭失败不向上抛
            pass

    @staticmethod
    async def _rescue_storage_state(session: LoginSession) -> dict:
        """会话回收前抢救登录成果。

        只在「页面确实是登录态」时才有东西可抢：``authenticated()`` 要求先离开过
        登录页、再回到控制台成功落点内，游客会话抢了也是白抢（存下来反而是脏数据）。
        抢不到就返回空 dict，让后端按「会话没了」正常处理。
        """
        try:
            if not session.authenticated():
                return {}
            state = await session.context.storage_state()
            raw = json.dumps(state)
        except Exception:  # noqa: BLE001 - 抢救失败不能挡住过期回收流程
            return {}
        if not raw:
            return {}
        return {"status": "authenticated", "storage_state": raw}

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
            self._cancel_open_task(session)
            try:
                await session.context.close()
            except Exception:  # noqa: BLE001
                pass


# ---------------------------------------------------------------------------
# 动作原语：全部转发到同一页面，不判断站点结构
# ---------------------------------------------------------------------------


async def capture(session: LoginSession) -> str:
    """截当前视口，返回 base64 JPEG（不带 data: 前缀）。

    截图失败不抛异常：页面卡住或正在跳转时截图可能超时，抛出去就变成前端看到的
    5xx（正是被投诉的那种「一点就报错」）。返回空串，前端显示「正在打开登录页…」
    并在下一轮轮询自然恢复。
    """
    try:
        raw = await session.page.screenshot(
            type="jpeg", quality=LOGIN_SHOT_QUALITY, full_page=False, timeout=10000
        )
    except Exception:  # noqa: BLE001
        return ""
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


# 页面「形状指纹」：元素数（穿透开放影子树）+ 输入框数 + 当前 URL。
# 只用来判断「页面还在变吗」，不参与任何业务判定 —— 具体是什么元素由各站点自己决定。
_PAGE_SIGNATURE_JS = r"""() => {
  let n = 0;
  let inputs = 0;
  const walk = (root) => {
    let nodes;
    try { nodes = root.querySelectorAll('*'); } catch (e) { return; }
    for (const el of nodes) {
      n += 1;
      const tag = el.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') inputs += 1;
      if (el.shadowRoot) walk(el.shadowRoot);
    }
  };
  walk(document);
  return n + '|' + inputs + '|' + location.href;
}"""


async def settle(
    session: LoginSession,
    timeout_ms: int = LOGIN_SETTLE_TIMEOUT_MS,
    quiet_ms: int = LOGIN_SETTLE_QUIET_MS,
) -> None:
    """等页面停止变化（最多 timeout_ms），动作之后的截图必须在它之后。

    不抛异常：这里只是「尽量等到画面稳定」，等不到也要照常截图，
    让用户至少能看到当前画面，而不是拿到一个错误。
    """
    loop = asyncio.get_running_loop()
    deadline = loop.time() + timeout_ms / 1000.0
    quiet = quiet_ms / 1000.0
    last = None
    last_change = loop.time()
    while loop.time() < deadline:
        try:
            sig = await asyncio.wait_for(session.page.evaluate(_PAGE_SIGNATURE_JS), timeout=2.0)
        except Exception:  # noqa: BLE001 - 读不到指纹就当页面仍在变，继续等
            sig = None
        now = loop.time()
        if sig != last:
            last = sig
            last_change = now
        elif now - last_change >= quiet:
            return
        await asyncio.sleep(LOGIN_SETTLE_POLL_MS / 1000.0)


async def snapshot_storage_state(session: LoginSession) -> str:
    """返回 Playwright storageState JSON 字符串（只在 finish 成功后调用）。"""
    import json

    state = await session.context.storage_state()
    return json.dumps(state)


# 生效性校验的导航超时：比首帧短，校验失败不该把 finish 拖成超时。
LOGIN_VERIFY_TIMEOUT_MS = 25000


async def verify_effective(session: LoginSession) -> bool:
    """另开一页访问控制台入口，看「没登录的会话」会不会被弹回登录页。

    为什么不能只看当前网址：上游登录失败后常常跳回原页面，网址落在控制台
    前缀内，就被误判成「登录成功」——2026-11-13 美团扫码失败，界面却提示
    「已检测到登录完成」，正是这个误判。

    改判据为**跳转落点**：拿同一个 context 新开一页访问控制台入口。
    没登录 → 被重定向到登录域名（如 passport.*）；登录有效 → 停在控制台。
    这只依赖「会不会被踢走」这个结构性事实，不猜任何站点文案。

    返回值语义：
    - True：校验通过，或压根没法校验（跳过）
    - False：明确被弹回别的域名 = 这次登录没生效
    校验过程本身出错（网络抖动）不误伤，按通过处理，避免把好登录判死。
    """
    prefix = (session.success_url_prefix or "").strip()
    if not prefix:
        return True
    probe_page = None
    try:
        probe_page = await session.context.new_page()
        await probe_page.goto(
            prefix, wait_until="domcontentloaded", timeout=LOGIN_VERIFY_TIMEOUT_MS
        )
        await probe_page.wait_for_timeout(1000)
        landed = (probe_page.url or "").strip()
        if not landed:
            return True
        return landed.startswith(prefix)
    except Exception:  # noqa: BLE001 - 校验失败不误伤
        return True
    finally:
        if probe_page is not None:
            try:
                await probe_page.close()
            except Exception:  # noqa: BLE001
                pass


# ---------------------------------------------------------------------------
# 通用表单识别：把页面上的输入框与按钮报给前端，让用户像填普通表单一样填
# ---------------------------------------------------------------------------

# 为什么要遍历影子树：美团 passport 把登录表单放在 shadow root 里，
# document.querySelectorAll('input') 返回空数组（2026-11-12 线上实测）。
# 遍历时给命中的元素打上本模块自己的标记，之后按标记定位——标记是现场生成的，
# 不依赖任何站点结构，也不写站点选择器。
_LOGIN_INVENTORY_JS = r"""() => {
  const MAX_FIELDS = 12;
  const MAX_BUTTONS = 24;
  const SHORT = 18;

  const clean = (s) => String(s == null ? '' : s).replace(/\s+/g, ' ').trim();
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    if (r.width <= 2 || r.height <= 2) return false;
    const cs = getComputedStyle(el);
    if (!cs || cs.visibility === 'hidden' || cs.display === 'none') return false;
    return Number(cs.opacity || 1) > 0.05;
  };
  const stamp = (el, attr, i) => { try { el.setAttribute(attr, String(i)); } catch (e) {} };

  const fields = [];
  const buttons = [];
  const seen = new Set();

  // 输入框：可填的文本框只在可见时报；勾选框/单选框即使被样式藏起来（0×0 自定义
  // 圆圈）也要报 —— 「请先阅读并勾选用户协议」是登录必经项，漏了就等于登不进去。
  const collectInput = (el) => {
    const type = String(el.type || 'text').toLowerCase();
    if (type === 'hidden') return;
    const isChoice = type === 'checkbox' || type === 'radio';
    const shown = visible(el);
    if (!shown && !isChoice) return;
    if (fields.length >= MAX_FIELDS) return;

    // 名字优先级：勾选项优先取它旁边的人类可读文字（「我已阅读并同意…」），
    // 文本框才用 placeholder。绝不把 name/id 当成给用户看的名字
    // （那会显示成 inputChecked 这种机器名）。
    let label = '';
    if (isChoice) {
      const box = el.closest ? el.closest('label') : null;
      if (box) label = clean(box.textContent);
      if (!label) {
        let anc = el.parentElement;
        for (let depth = 0; depth < 3 && anc && !label; depth += 1) {
          const t = clean(anc.textContent);
          // 只认「这个容器里就我一个控件」的文字；否则会抓到一整块表单的拼接文字。
          let peers = 0;
          try { peers = anc.querySelectorAll('input,textarea,select').length; } catch (e) { peers = 9; }
          if (peers <= 1 && t.length > 0 && t.length <= 40) label = t;
          anc = anc.parentElement;
        }
      }
    }
    if (!label) {
      label = clean(el.placeholder) || clean(el.getAttribute('aria-label'));
    }
    if (!label && isChoice) label = '勾选项 ' + (fields.length + 1);
    if (!label) label = clean(el.name) || clean(el.id);
    if (!label && !shown) return;

    stamp(el, 'data-wq-field', fields.length);
    fields.push({
      index: fields.length,
      type: type,
      label: (label || ('输入框 ' + (fields.length + 1))).slice(0, 40),
      filled: isChoice ? !!el.checked : clean(el.value).length > 0,
      masked: type === 'password',
    });
  };

  const collectButton = (el) => {
    if (buttons.length >= MAX_BUTTONS) return;
    let clickable = el.tagName === 'BUTTON' || el.tagName === 'A'
      || el.getAttribute('role') === 'button';
    if (!clickable) {
      const cs = getComputedStyle(el);
      clickable = !!cs && cs.cursor === 'pointer';
    }
    if (!clickable || !visible(el)) return;

    // 文字来源：优先「自己直接持有的文本节点」；没有就看整块 innerText。
    //
    // 为什么必须回退到 innerText：美团登录取证页的「手机验证码登录」是个 div，
    // 文字包在子节点里，只认直接文本节点就会得到空串并把整个入口丢掉 ——
    // 用户看到画面里明明有这行字，面板里却没有能点它的地方（2026-11-13 线上截图）。
    let label = clean(Array.from(el.childNodes)
      .filter((n) => n.nodeType === 3)
      .map((n) => n.textContent).join(''));
    if (!label) label = clean(el.innerText || el.textContent);
    if (!label || label.length > SHORT || seen.has(label)) return;

    // 最内层优先：如果自己内部还有「可点的、带文字的」元素，交给它上报。
    // 必要性：外层容器的中心点常常不在按钮上（实测点 314×80 的 wrapper 中心点
    // 落在按钮外 8 像素，点了等于没点），只有最内层元素的中心点才可靠。
    let innerClickable = false;
    let kids;
    try { kids = el.querySelectorAll('*'); } catch (e) { kids = []; }
    for (const kid of kids) {
      if (!clean(kid.innerText || '')) continue;
      const kcs = getComputedStyle(kid);
      if (kcs && (kcs.cursor === 'pointer' || kid.tagName === 'BUTTON' || kid.tagName === 'A')) {
        innerClickable = true;
        break;
      }
    }
    if (innerClickable) return;

    seen.add(label);
    stamp(el, 'data-wq-btn', buttons.length);
    buttons.push({ index: buttons.length, text: label.slice(0, 40) });
  };

  const walk = (root) => {
    let nodes;
    try { nodes = root.querySelectorAll('*'); } catch (e) { return; }
    for (const el of nodes) {
      if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') collectInput(el);
      else collectButton(el);
      if (el.shadowRoot) walk(el.shadowRoot);
    }
  };

  walk(document);
  return { fields: fields, buttons: buttons };
}"""


async def describe_form(session: "LoginSession") -> dict:
    """返回当前页面的可见输入框与可点按钮（字段顺序即 DOM 顺序）。

    每次调用都会重新给元素打标记，因此返回的 index 只对「紧随其后」的一次
    填写/点击有效；调用方应把 label / text 一起带回来校验，防止页面重排后
    点错元素。

    ``available`` 是「这次到底读到了没有」，不是「页面上有没有表单」：
    读失败（跳转中 / 超时 / 主线程卡住）与「这页确实没有输入框」在旧实现里
    都返回空清单，前端只能把后者当事实显示「这个页面没有可填的输入框」，
    用户既不知道怎么回事也没有下一步（2026-11-13 线上投诉）。
    两者必须分开表达：读失败要说「没读到，重试」，不能替页面下结论。
    """
    for attempt in range(LOGIN_INVENTORY_ATTEMPTS):
        try:
            # 页面跳转/卡死时 evaluate 可能长时间不返回，这里必须设上限：
            # 否则整帧请求（截图 + 表单）会被一个卡住的页面拖死，用户看到画面不刷新。
            result = await asyncio.wait_for(
                session.page.evaluate(_LOGIN_INVENTORY_JS), timeout=LOGIN_INVENTORY_TIMEOUT
            )
        except Exception:  # noqa: BLE001 - 跳转中读不到：稍等重试，仍失败才认输
            result = None
        if isinstance(result, dict):
            fields = result.get("fields") or []
            buttons = result.get("buttons") or []
            return {
                "fields": fields if isinstance(fields, list) else [],
                "buttons": buttons if isinstance(buttons, list) else [],
                "available": True,
            }
        if attempt < LOGIN_INVENTORY_ATTEMPTS - 1:
            await asyncio.sleep(LOGIN_INVENTORY_RETRY_WAIT)
    return {"fields": [], "buttons": [], "available": False}


def _pick_inventory(items: list, index: int, expected: str, key: str) -> Optional[int]:
    """在清单里挑出目标元素的下标。

    优先按 label / text 精确匹配（页面重绘后下标会变，文字不会），
    只有匹配不到且没给文字时才退回下标。返回 None 表示清单里已经没有它。
    """
    if expected:
        for pos, item in enumerate(items):
            if str(item.get(key) or "") == expected:
                return pos
        return None
    if 0 <= index < len(items):
        return index
    return None


# ---------------------------------------------------------------------------
# 首屏登录方式：进来先落在哪个 Tab
# ---------------------------------------------------------------------------

# 「切到短信登录」这个入口在不同站点/不同页面状态下叫法不同，按关键词认，
# 不认 DOM 结构 —— 上游改版换个 class 不影响这里。
LOGIN_SMS_BUTTON_HINTS = (
    "验证码登录",
    "验证码登陆",
    "短信登录",
    "短信登陆",
    "短信验证码",
    "手机验证码",
    "手机号登录",
    "手机登录",
)
# 已经在短信表单上：看得见手机号和/或验证码输入框（按类型 + 名字认，不认选择器）。
_LOGIN_PHONE_LABEL_HINTS = ("手机", "电话", "号码", "phone", "mobile")
_LOGIN_CODE_LABEL_HINTS = ("验证码", "动态码", "code", "captcha")


def _looks_like_sms_form(fields: list) -> bool:
    """当前页面是不是「短信登录」表单。

    判据：有可见文本框，且名字命中手机号或验证码；或者有 type=tel 的输入框。
    宁可判错放过（返回 False 只是少点一下），也不要把别的表单当成短信表单。
    """
    for field in fields or []:
        kind = str(field.get("type") or "").lower()
        label = str(field.get("label") or "").lower()
        if kind == "tel":
            return True
        if kind not in ("text", "number", "tel"):
            continue
        if any(h in label for h in _LOGIN_PHONE_LABEL_HINTS):
            return True
        if any(h in label for h in _LOGIN_CODE_LABEL_HINTS):
            return True
    return False


async def select_login_mode(session: "LoginSession", mode: str) -> bool:
    """把页面切到指定登录方式，返回是否已经切到位。

    为什么要替用户点这一下：longcat 的登录页默认是二维码，而在服务端浏览器会话里
    扫码一律被上游判「抱歉，登录失败」（2026-11-13 生产连续实测），画面上的
    「手机验证码登录」又因为结构差异不一定能被枚举成按钮，用户就被关在扫码页里出不来。
    先把页面切到短信表单，用户打开面板第一眼就是能填的手机号框。

    只用「页面把什么报给了我们」来判定（字段名/按钮文字），不写站点选择器；
    切不过去也不抛异常，保持页面原样，用户仍可自己点。
    """
    wanted = (mode or "").strip().lower()
    if wanted != "sms":
        # 其余方式（qrcode / password）不做自动切换：页面默认落点已经够用。
        return False

    for _ in range(LOGIN_MODE_SWITCH_ATTEMPTS):
        inventory = await describe_form(session)
        if not inventory.get("available"):
            await settle(session)
            continue
        if _looks_like_sms_form(inventory["fields"]):
            return True
        target = None
        for button in inventory["buttons"]:
            text = str(button.get("text") or "")
            if any(hint in text for hint in LOGIN_SMS_BUTTON_HINTS):
                target = button
                break
        if target is None:
            return False
        try:
            clicked = await click_button(session, target["index"], target["text"])
        except Exception:  # noqa: BLE001 - 点不到就放弃，别让登录面板打不开
            clicked = False
        if not clicked:
            return False
        await settle(session)

    inventory = await describe_form(session)
    return _looks_like_sms_form(inventory["fields"])


async def fill_field(
    session: "LoginSession", index: int, text: str, expected_label: str = ""
) -> bool:
    """把文字填进指定的可见输入框，返回是否真的填进去了。

    用 ``locator.fill`` 而不是 ``keyboard.type``：后者要求元素已获得焦点，
    而让用户点中 7 像素高的缩略图去聚焦本身就不可靠（2026-11-12 线上已踩）。
    ``fill`` 会先聚焦再整体赋值并派发 input 事件，React 受控组件同样生效。
    """
    inventory = await describe_form(session)
    pos = _pick_inventory(inventory["fields"], index, expected_label, "label")
    if pos is None:
        return False
    locator = session.page.locator('[data-wq-field="%d"]' % pos).first
    await locator.fill(text, timeout=10000)
    return True


async def click_button(session: "LoginSession", index: int, expected_text: str = "") -> bool:
    """点击指定的可见按钮，返回是否真的点到了（点之前按文字核对过）。"""
    inventory = await describe_form(session)
    pos = _pick_inventory(inventory["buttons"], index, expected_text, "text")
    if pos is None:
        return False
    locator = session.page.locator('[data-wq-btn="%d"]' % pos).first
    await locator.click(timeout=10000)
    return True


# 勾选/取消勾选一个 checkbox / radio。
# 为什么不用 locator.check()：这类元素常被样式做成 0×0 的自定义圆圈，
# Playwright 的可操作性检查会判它不可见而直接失败；原生 click 不受此限制。
_LOGIN_TOGGLE_JS = r"""(idx) => {
  const find = (root) => {
    let hit = null;
    try { hit = root.querySelector('[data-wq-field="' + idx + '"]'); } catch (e) { hit = null; }
    if (hit) return hit;
    let nodes;
    try { nodes = root.querySelectorAll('*'); } catch (e) { return null; }
    for (const el of nodes) {
      if (el.shadowRoot) { const r = find(el.shadowRoot); if (r) return r; }
    }
    return null;
  };
  const el = find(document);
  if (!el) return null;
  el.click();
  return !!el.checked;
}"""


async def toggle_field(session: "LoginSession", index: int, expected_label: str = "") -> bool:
    """勾选 / 取消勾选指定项，返回是否真的操作到了。"""
    inventory = await describe_form(session)
    pos = _pick_inventory(inventory["fields"], index, expected_label, "label")
    if pos is None:
        return False
    await session.page.evaluate(_LOGIN_TOGGLE_JS, pos)
    return True


def account_label(session: LoginSession, fallback: str = "") -> str:
    """尽量给一个人类可读的账号标识；取不到就走 fallback。

    只读页面标题与 URL 主机名，绝不读取 DOM 中的手机号/邮箱等 PII。
    """
    title = (session.page.title() if session.page else "") or ""
    return title.strip()[:120] or fallback
