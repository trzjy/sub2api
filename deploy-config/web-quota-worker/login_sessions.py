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

# 读取页面表单结构的超时（秒）：页面卡住时不能让整帧请求被拖死。
LOGIN_INVENTORY_TIMEOUT = 3.0

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


async def snapshot_storage_state(session: LoginSession) -> str:
    """返回 Playwright storageState JSON 字符串（只在 finish 成功后调用）。"""
    import json

    state = await session.context.storage_state()
    return json.dumps(state)


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
    // 只认「自己直接持有文字」的最内层元素，避免把整块容器当按钮。
    const own = clean(Array.from(el.childNodes)
      .filter((n) => n.nodeType === 3)
      .map((n) => n.textContent).join(''));
    if (!own || own.length > SHORT || seen.has(own)) return;
    seen.add(own);
    stamp(el, 'data-wq-btn', buttons.length);
    buttons.push({ index: buttons.length, text: own.slice(0, 40) });
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
    """
    try:
        # 页面跳转/卡死时 evaluate 可能长时间不返回，这里必须设上限：
        # 否则整帧请求（截图 + 表单）会被一个卡住的页面拖死，用户看到画面不刷新。
        result = await asyncio.wait_for(
            session.page.evaluate(_LOGIN_INVENTORY_JS), timeout=LOGIN_INVENTORY_TIMEOUT
        )
    except Exception:  # noqa: BLE001 - 页面跳转中读不到就当没有表单
        return {"fields": [], "buttons": []}
    if not isinstance(result, dict):
        return {"fields": [], "buttons": []}
    fields = result.get("fields") or []
    buttons = result.get("buttons") or []
    return {
        "fields": fields if isinstance(fields, list) else [],
        "buttons": buttons if isinstance(buttons, list) else [],
    }


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
