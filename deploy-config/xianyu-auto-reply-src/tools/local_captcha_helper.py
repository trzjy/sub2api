#!/usr/bin/env python3
"""本地人工打码助手（xianyu-captcha-helper）

实现 Worker「远程过滑块」协议的本地求解端，Worker 侧零改动：

- 契约 A（token 续期链路，对应 common/services/captcha/remote_solver.py 与
  orchestrator._call_remote_solve）：
    POST /solve
    body: {"secret_key", "account_id", "url"(punish 链接), "browser_timeout"[, "cookies", "device_id"]}
    成功 → {"success": true, "data": {"cookies": {x5*: ...}}}
    失败 → {"success": false, "message": "..."}
    链接过期 → {"success": false, "message": "...", "data": {"url_expired": true}}
      （Worker 编排收到 url_expired 会刷新 URL 后重试）
- 契约 B（商品监控，对应 common/services/monitor_remote_risk_client.py）：
    POST /risk   header: X-API-Key: <secret>
    body: {"type": "x5sec_ali", "data": {"url": "..."}}
    → {"success": bool, "message": str, "data": {"x5sec", "bx-pp", "bx_et"}}

人工流程：请求到达 → 桌面通知 → 弹出有头 Chromium 加载验证链接 → 人工拖滑块 →
检测到 x5* cookie（与 orchestrator._has_x5sec 同口径：名称以 x5 开头或含 x5sec）→
按契约返回 → 浏览器停留数秒供确认后关闭。

失败语义：超时 / 忙 / 浏览器被关闭 / 页面异常 → 返回失败，Worker 编排自动回退
本机真实鼠标与 Playwright 引擎，不劣于现状。

超时上限：契约 A 的 Worker 读超时下限为 300s（remote_timeout.get_remote_solve_timeout），
本端 deadline 固定压在 285s 内；契约 B（商品监控）总超时 120s，deadline 压在 110s 内。

隐私：不请求、不存储账号 Cookie；pass_cookies 开启时请求体携带的 Cookie 仅存在于
请求内存对象中。日志不打印任何 cookie 值与完整验证链接。

配置：~/.config/xianyu-captcha-helper/config.json（首次运行自动生成 32 位 secret）。
"""
from __future__ import annotations

import argparse
import asyncio
import hmac
import json
import os
import secrets
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, Optional

from aiohttp import web
from playwright.async_api import async_playwright

DEFAULT_CONFIG_PATH = Path.home() / ".config" / "xianyu-captcha-helper" / "config.json"
DEFAULT_PORT = 18089
# 下列参数来自官方线上 bundle 的既有取证，不从请求接收，也不允许配置成任意脚本 URL。
KIMI_CAPTCHA_SDK_URL = "https://cstaticdun.126.net/load.min.js"
KIMI_CAPTCHA_ID = "2752f01d87dc45948de1a1e0ac2b7160"
GLM_CAPTCHA_SDK_URL = "https://chatglm.cn/smcp/smcp.min.js"
GLM_CAPTCHA_ORGANIZATION = "599zinRadlRxTLrOkTR9"
# 挑战页必须运行在**真实 http 源**下：about:blank / data: 等不透明源会被浏览器拒绝
# 读取 document.cookie（"Access is denied for this document"），导致易盾/数美 SDK 初始化
# 抛异常、页面卡在“正在加载官方验证组件…”。该地址仅作为页面源，HTML 由 Playwright 路由
# 拦截注入，不实际访问该路径（端口无需与监听端口一致）。
CHALLENGE_PAGE_URL = "http://127.0.0.1:18089/__challenge_page__"
# 与 Worker 读超时对齐：契约 A ≥300s、契约 B =120s，各留网络余量。
MAX_WAIT_CONTRACT_A = 285
MAX_WAIT_CONTRACT_B = 110
EXPIRED_MARKER = "页面访问出现了问题"


def log(msg: str) -> None:
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}", flush=True)


def load_or_create_config(path: Path) -> Dict[str, Any]:
    """读取配置；不存在则生成含 32 位 secret 的默认配置（0600）。"""
    try:
        cfg = json.loads(path.read_text(encoding="utf-8"))
        if not cfg.get("secret"):
            raise ValueError("secret 为空")
        return cfg
    except FileNotFoundError:
        pass
    except Exception as exc:  # 配置损坏时显式失败，避免静默换 secret
        log(f"配置读取失败（{path}）：{exc}；如需重置请删除该文件")
        raise SystemExit(2)
    cfg = {
        "secret": secrets.token_hex(16),
        "bind": "127.0.0.1",
        "port": DEFAULT_PORT,
        "max_wait": MAX_WAIT_CONTRACT_A,
        "challenge_max_wait": 180,
        "headless": False,
        "notify": True,
        "post_success_keep_secs": 8,
        "browser_channel": "",
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.chmod(path, 0o600)
    log(f"已生成初始配置 {path}（secret 见该文件，需填入 Worker /captcha/remote-config）")
    return cfg


class Solver:
    """单槽位人工求解器：同一时刻只跑一个人工验证。"""

    def __init__(self, cfg: Dict[str, Any]):
        self.cfg = cfg
        self.lock = asyncio.Lock()
        self.pw: Any = None

    async def start(self) -> None:
        self.pw = await async_playwright().start()

    async def stop(self) -> None:
        if self.pw:
            await self.pw.stop()
            self.pw = None

    # Baxia 页面加载时会下发 cookie 可用性探针（如 bx-cookie-test），并非通过凭证。
    # 若误当作通过信号，人工尚未完成就提前返回，Worker 侧也会因缺少 x5sec 判失败。
    _PROBE_COOKIES = frozenset({"bx-cookie-test"})

    @staticmethod
    def _x5_cookies(cookies: list) -> Dict[str, str]:
        """提取验证相关 cookie。

        返回集合口径：x5* / 含 x5sec（与 orchestrator._has_x5sec 及
        cookie_token_manager 成功判定同口径）+ bx* 家族（bx-pp / bx_et 等 Baxia 系，
        契约 B 依赖），剔除页面加载探针。
        """
        out: Dict[str, str] = {}
        for c in cookies:
            name = str(c.get("name", ""))
            low = name.lower()
            if low in Solver._PROBE_COOKIES:
                continue
            if low.startswith("x5") or "x5sec" in low or low.startswith("bx"):
                out[name] = str(c.get("value", ""))
        return out

    @staticmethod
    def _has_pass_signal(cookies: Dict[str, str]) -> bool:
        """是否已出现真正的通过凭证（x5* / 含 x5sec），与 Worker 成功判定同口径。

        仅有 bx* 家族 cookie 时不算通过：真实放行凭证是 x5sec，Worker 侧
        （cookie_token_manager 成功分支）也只认 x5* 集合。
        """
        return any(
            low.startswith("x5") or "x5sec" in low
            for low in (str(n).lower() for n in cookies)
        )

    async def _notify(self, account_id: str, deadline: int) -> None:
        if not self.cfg.get("notify", True):
            return
        try:
            proc = await asyncio.create_subprocess_exec(
                "notify-send", "-u", "critical", "-a", "xianyu-captcha-helper",
                "闲鱼人工验证",
                f"账号 {account_id or '?'} 需要过滑块，请在弹出的浏览器中完成（限时 {deadline}s）",
                stdout=asyncio.subprocess.DEVNULL,
                stderr=asyncio.subprocess.DEVNULL,
            )
            await asyncio.wait_for(proc.wait(), timeout=5)
        except Exception as exc:
            log(f"桌面通知发送失败（不影响求解）：{exc}")

    async def _notify_face(self, account_id: str, qr_path: str) -> None:
        if not self.cfg.get("notify", True):
            return
        front_base = str(self.cfg.get("front_base_url") or "").strip().rstrip("/")
        body = f"账号 {account_id or '?'} 需要人脸验证，请打开二维码完成"
        if front_base:
            body += f"：{front_base}/admin/xianyu/accounts"
        try:
            proc = await asyncio.create_subprocess_exec(
                "notify-send", "-u", "critical", "-a", "xianyu-captcha-helper",
                "-i", qr_path, "闲鱼人脸验证", body,
                stdout=asyncio.subprocess.DEVNULL,
                stderr=asyncio.subprocess.DEVNULL,
            )
            await asyncio.wait_for(proc.wait(), timeout=5)
        except Exception as exc:
            log(f"人脸桌面通知发送失败（不影响登录链路）：{exc}")

    async def solve(self, url: str, account_id: str, deadline: int) -> tuple[str, Dict[str, str], Optional[bool]]:
        """打开浏览器等待人工过滑块。

        Returns:
            (status, cookies, url_expired)
            status: 'ok' / 'fail' / 'busy'
            url_expired 仅契约 A 使用：True 时 Worker 会刷新 URL 重试。
        """
        if self.lock.locked():
            return "busy", {}, None
        async with self.lock:
            if self.pw is None:
                log("求解器未初始化（未调用 start）")
                return "fail", {}, None
            headless = bool(self.cfg.get("headless", False))
            channel = self.cfg.get("browser_channel") or None
            keep_secs = int(self.cfg.get("post_success_keep_secs", 8))
            log(f"开始求解 account={account_id} host={_host_of(url)} deadline={deadline}s headless={headless}")
            await self._notify(account_id, deadline)
            browser = None
            try:
                browser = await self.pw.chromium.launch(
                    headless=headless,
                    channel=channel,
                    args=["--window-size=520,680", "--disable-blink-features=AutomationControlled"],
                )
                context = await browser.new_context(locale="zh-CN", no_viewport=True)
                page = await context.new_page()
                await page.goto(url, wait_until="domcontentloaded", timeout=30_000)
                loop_start = time.monotonic()
                last_content_check = 0.0
                probe_logged = False
                while True:
                    elapsed = time.monotonic() - loop_start
                    if elapsed >= deadline:
                        log(f"求解超时 account={account_id}")
                        return "fail", {}, None
                    cookies = self._x5_cookies(await context.cookies())
                    if cookies and not self._has_pass_signal(cookies):
                        # 仅有 bx* 家族/探针类 cookie，尚无 x5* 凭证：不算通过，继续等人工完成
                        if not probe_logged:
                            probe_logged = True
                            log(f"捕获到非凭证 cookie {sorted(cookies)}（值不落日志），忽略并继续等待")
                        cookies = {}
                    if cookies:
                        log(f"人工验证通过 account={account_id} cookies={sorted(cookies)}（值不落日志）")
                        if keep_secs > 0 and not headless:
                            await asyncio.sleep(keep_secs)  # 停留片刻让人工看到通过结果
                        return "ok", cookies, None
                    if time.monotonic() - last_content_check >= 2.0:
                        last_content_check = time.monotonic()
                        try:
                            if page.is_closed():
                                log(f"浏览器被手动关闭 account={account_id}")
                                return "fail", {}, None
                            content = await page.content()
                            if EXPIRED_MARKER in content:
                                log(f"验证链接已过期 account={account_id}（Worker 应刷新 URL 重试）")
                                return "fail", {}, True
                        except Exception:
                            pass  # 页面跳转瞬间 content() 可能抛错，下一轮再查
                    await asyncio.sleep(1.0)
            except Exception as exc:
                log(f"求解异常 account={account_id}：{type(exc).__name__}: {exc}")
                return "fail", {}, None
            finally:
                if browser:
                    try:
                        await browser.close()
                    except Exception:
                        pass


@dataclass
class ChallengeSession:
    session_id: str
    platform: str
    login_session_id: str
    phone: str
    phone_code: str
    deadline: float
    status: str = "pending"
    result: Dict[str, str] = field(default_factory=dict)
    context_gap: bool = False
    message: str = ""
    task: Optional[asyncio.Task] = None
    consumed: bool = False


class ManualChallengeManager:
    """GLM/Kimi 官方 SDK 人工挑战；单槽位、短 TTL、结果一次性消费。"""

    SUPPORTED = frozenset({"glm", "kimi"})
    PLATFORM_ALIASES = {"glm": "glm", "zhipu": "glm", "kimi": "kimi"}

    def __init__(self, solver: Solver, cfg: Dict[str, Any]):
        self.solver = solver
        self.cfg = cfg
        self.max_wait = max(1, min(int(cfg.get("challenge_max_wait", 180)), 300))
        self._session: Optional[ChallengeSession] = None
        self._lock = asyncio.Lock()

    @staticmethod
    def _normalize_platform(platform: str) -> str:
        return ManualChallengeManager.PLATFORM_ALIASES.get(str(platform).strip().lower(), "")

    @staticmethod
    def _valid_binding(value: str, max_length: int) -> bool:
        return bool(value) and len(value) <= max_length and all(ord(char) >= 32 for char in value)

    @staticmethod
    def _sdk_html(session: ChallengeSession) -> str:
        """返回仅加载已取证官方 SDK 的本地页面；不会导航到 SDK 脚本 URL。"""
        common = """
<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>网页登录人工验证</title>
<style>
  html, body { margin: 0; padding: 20px 10px; min-height: 100vh; box-sizing: border-box; overflow-y: auto; }
  /* SDK 宿主宽度必须受限，避免固定高度的验证码图片随过宽视口横向拉伸；
     min-height 保留足够的组件空间，body 可滚动兜底。 */
  #glm-captcha { width: 100%; max-width: 480px; min-height: 520px; margin: 20px auto 0; }
  /* 易盾嵌入式挑战默认以 40px 智能检测条为锚点向上展开；改为文档流向下展开，避免覆盖标题和状态。 */
  #glm-captcha .yidun_classic-container {
    position: relative !important;
    top: auto !important;
    bottom: auto !important;
    left: auto !important;
  }
</style>
</head><body>
<h3>请在此窗口完成人工验证</h3><p id="status">正在加载官方验证组件…</p>
<div id="glm-captcha"></div><script>
const statusNode = document.getElementById('status');
function fail(message) {
  statusNode.textContent = message || '官方验证组件未完成';
  window.challengeFail(message || 'sdk_error');
}
"""
        if session.platform == "kimi":
            return common + f"""
function handleValidate(value) {{
  if (typeof value !== 'string' || !value) return;
  statusNode.textContent = '验证完成，可以返回管理页';
  window.challengeComplete({{validate: value}});
}}
const script = document.createElement('script');
script.src = {json.dumps(KIMI_CAPTCHA_SDK_URL)};
script.onload = () => {{
  if (typeof window.initNECaptcha !== 'function') return fail('易盾 SDK 未就绪');
  window.initNECaptcha({{
    captchaId: {json.dumps(KIMI_CAPTCHA_ID)}, element: '#glm-captcha', mode: 'embed', apiVersion: 2,
    width: '100%',
    onVerify: (err, data) => {{ if (err) return; handleValidate(data && data.validate); }},
    onSuccess: (_instance, data) => handleValidate(data && data.validate),
    onError: () => fail('易盾验证失败'),
    onClose: () => fail('验证窗口已关闭')
  }});
}};
script.onerror = () => fail('易盾 SDK 加载失败');
document.head.appendChild(script);
// 兜底：易盾成功后会写入隐藏输入 NECaptchaValidate，覆盖回调名差异与无感知自动通过场景。
setInterval(() => {{
  document.querySelectorAll('input[name=NECaptchaValidate]').forEach((el) => handleValidate(el.value));
}}, 500);
</script></body></html>"""
        return common + f"""
const script = document.createElement('script');
script.src = {json.dumps(GLM_CAPTCHA_SDK_URL)};
script.onload = () => {{
  if (typeof window.initSMCaptcha !== 'function') return fail('数美 SDK 未就绪');
  const instance = window.initSMCaptcha({{
    organization: {json.dumps(GLM_CAPTCHA_ORGANIZATION)}, appendTo: '#glm-captcha',
    product: 'embed', width: '100%'
  }});
  if (!instance || typeof instance.onSuccess !== 'function') return fail('数美组件初始化失败');
  instance.onSuccess((data) => {{
    if (!data || typeof data.rid !== 'string' || !data.rid) return fail('数美回调缺少 rid');
    if (typeof data.md5 !== 'string' || !data.md5) return fail('数美回调缺少已取证 md5');
    statusNode.textContent = '验证完成，可以返回管理页';
    window.challengeComplete({{rid: data.rid, md5: data.md5}});
  }});
  if (typeof instance.onClose === 'function') instance.onClose(() => fail('验证窗口已关闭'));
}};
script.onerror = () => fail('数美 SDK 加载失败');
document.head.appendChild(script);
</script></body></html>"""

    @staticmethod
    async def _capture_kimi_layout(page: Any) -> Optional[Dict[str, Any]]:
        """图片挑战出现后采集一次不含内容与凭证的布局元数据。"""
        return await page.evaluate("""
() => {
  const imageSelectors = ['.yidun_bgimg', '.yidun_bg-img'];
  const hasVisibleImage = imageSelectors.some((selector) => {
    const element = document.querySelector(selector);
    if (!element) return false;
    const rect = element.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0;
  });
  if (!hasVisibleImage) return null;

  const number = (value) => Number.isFinite(value) ? Math.round(value * 100) / 100 : null;
  const css = (value) => String(value || '').slice(0, 160);
  const snapshot = (element) => {
    if (!element) return null;
    const rect = element.getBoundingClientRect();
    const computed = getComputedStyle(element);
    const result = {
      className: (typeof element.className === 'string' ? element.className : '').slice(0, 160),
      rect: {
        x: number(rect.x), y: number(rect.y),
        width: number(rect.width), height: number(rect.height)
      },
      computed: {
        position: css(computed.position), top: css(computed.top), bottom: css(computed.bottom),
        left: css(computed.left), overflow: css(computed.overflow), display: css(computed.display),
        boxSizing: css(computed.boxSizing), padding: css(computed.padding),
        transform: css(computed.transform)
      }
    };
    if (element instanceof HTMLImageElement) {
      result.naturalWidth = number(element.naturalWidth);
      result.naturalHeight = number(element.naturalHeight);
    }
    return result;
  };
  const selectors = [
    '.yidun_intellisense', '.yidun_classic-container', '.yidun_cover-frame',
    '.yidun_classic-wrapper', '.yidun', '.yidun_panel',
    '.yidun_panel-placeholder', '.yidun_bgimg', '.yidun_bg-img', '.yidun_control'
  ];
  const elements = {};
  selectors.forEach((selector) => { elements[selector] = snapshot(document.querySelector(selector)); });
  return {
    window: {
      innerWidth: number(window.innerWidth), innerHeight: number(window.innerHeight),
      outerWidth: number(window.outerWidth), outerHeight: number(window.outerHeight),
      devicePixelRatio: number(window.devicePixelRatio)
    },
    landmarks: {
      h3: snapshot(document.querySelector('h3')),
      status: snapshot(document.querySelector('#status')),
      host: snapshot(document.querySelector('#glm-captcha'))
    },
    elements,
    document: {
      bodyScrollWidth: number(document.body && document.body.scrollWidth),
      bodyScrollHeight: number(document.body && document.body.scrollHeight),
      documentScrollWidth: number(document.documentElement.scrollWidth),
      documentScrollHeight: number(document.documentElement.scrollHeight)
    }
  };
}
""")

    async def start(self, platform: str, login_session_id: str, phone: str,
                    phone_code: str = "", timeout: Optional[int] = None) -> ChallengeSession:
        platform = self._normalize_platform(platform)
        if platform not in self.SUPPORTED:
            raise ValueError("platform 必须是 glm(zhipu) 或 kimi")
        login_session_id = str(login_session_id).strip()
        phone = str(phone).strip()
        phone_code = str(phone_code).strip()
        if not self._valid_binding(login_session_id, 256):
            raise ValueError("login_session_id 无效")
        if not self._valid_binding(phone, 64):
            raise ValueError("phone 无效")
        if phone_code and not self._valid_binding(phone_code, 8):
            raise ValueError("phone_code 无效")
        if platform == "glm" and not phone_code:
            raise ValueError("GLM 挑战缺少 phone_code")
        async with self._lock:
            await self._release_expired_locked()
            if self._session is not None:
                raise RuntimeError("已有人工挑战占用单槽位")
            wait = self.max_wait if timeout is None else max(1, min(int(timeout), self.max_wait))
            session = ChallengeSession(
                session_id=secrets.token_urlsafe(24), platform=platform,
                login_session_id=login_session_id, phone=phone, phone_code=phone_code,
                deadline=time.monotonic() + wait,
            )
            self._session = session
            session.task = asyncio.create_task(self._run(session))
            return session

    async def _release_expired_locked(self) -> None:
        session = self._session
        if session is None or time.monotonic() < session.deadline:
            return
        if session.task and not session.task.done():
            session.task.cancel()
        self._session = None

    async def _run(self, session: ChallengeSession) -> None:
        browser = None
        page = None
        done = asyncio.Event()
        callback_result: Dict[str, str] = {}
        callback_failure = ""
        kimi_layout_captured = False
        kimi_layout_error_logged = False

        async def challenge_complete(_source: Any, payload: Any) -> None:
            if not isinstance(payload, dict):
                return
            allowed = ("validate",) if session.platform == "kimi" else ("rid", "md5")
            values = {
                field_name: str(payload.get(field_name) or "").strip()
                for field_name in allowed
                if isinstance(payload.get(field_name), str) and str(payload.get(field_name)).strip()
            }
            callback_result.clear()
            callback_result.update(values)
            done.set()

        async def challenge_fail(_source: Any, reason: Any = None) -> None:
            nonlocal callback_failure
            callback_failure = str(reason or "sdk_error")[:80]
            done.set()

        try:
            if self.solver.pw is None:
                raise RuntimeError("求解器未初始化")
            headless = bool(self.solver.cfg.get("headless", False))
            channel = self.solver.cfg.get("browser_channel") or None
            browser = await self.solver.pw.chromium.launch(
                headless=headless, channel=channel,
                args=["--window-size=520,680", "--disable-blink-features=AutomationControlled"],
            )
            context = await browser.new_context(locale="zh-CN", no_viewport=True)
            page = await context.new_page()
            await page.expose_binding("challengeComplete", challenge_complete)
            await page.expose_binding("challengeFail", challenge_fail)
            challenge_html = self._sdk_html(session)

            async def _serve_challenge(route: Any) -> None:
                await route.fulfill(
                    status=200, content_type="text/html; charset=utf-8", body=challenge_html
                )

            # 用真实源承载内联页面（路由拦截注入 HTML，不实际访问远端）：不透明源下
            # SDK 读 document.cookie 会被拒绝，导致组件初始化失败。
            await page.route(CHALLENGE_PAGE_URL, _serve_challenge)
            await page.goto(CHALLENGE_PAGE_URL, wait_until="domcontentloaded", timeout=30_000)
            session.status = "running"
            log(f"已打开 SDK 人工挑战 platform={session.platform} deadline={max(0, int(session.deadline - time.monotonic()))}s")
            while time.monotonic() < session.deadline and not done.is_set():
                if page.is_closed():
                    callback_failure = "browser_closed"
                    break
                if session.platform == "kimi" and not kimi_layout_captured:
                    try:
                        layout = await self._capture_kimi_layout(page)
                        if layout is not None:
                            payload = json.dumps(layout, ensure_ascii=False, separators=(",", ":"))
                            if len(payload) > 24000:
                                raise ValueError("layout_snapshot_too_large")
                            log("Kimi 图片挑战布局（脱敏） " + payload)
                            kimi_layout_captured = True
                    except Exception as exc:
                        if not kimi_layout_error_logged:
                            kimi_layout_error_logged = True
                            log(f"Kimi 图片挑战布局（脱敏）采集失败：{type(exc).__name__}")
                await asyncio.sleep(0.25)
            required = {"validate"} if session.platform == "kimi" else {"rid", "md5"}
            if done.is_set() and required.issubset(callback_result):
                session.result = dict(callback_result)
                if session.platform == "glm":
                    session.result["phone_code"] = session.phone_code
                session.status = "ok"
                session.message = "人工挑战完成"
            else:
                session.context_gap = True
                session.status = "context_gap"
                session.message = "官方 SDK 回调缺少所需字段；未猜测或伪造结果"
                if callback_failure:
                    log(f"SDK 人工挑战未完成 platform={session.platform} reason={callback_failure}")
        except asyncio.CancelledError:
            session.context_gap = True
            session.status = "context_gap"
            session.message = "人工挑战已过期"
        except Exception as exc:
            session.context_gap = True
            session.status = "context_gap"
            session.message = f"人工挑战未完成：{type(exc).__name__}"
            log(f"人工挑战异常 platform={session.platform}：{type(exc).__name__}")
        finally:
            if browser:
                try:
                    await browser.close()
                except Exception:
                    pass

    async def status(self, session_id: str, platform: str, login_session_id: str,
                     phone: str) -> Optional[Dict[str, Any]]:
        async with self._lock:
            session = self._session
            if session is None or session.session_id != session_id:
                return None
            if not self._matches(session, platform, login_session_id, phone):
                raise PermissionError("挑战会话绑定不匹配")
            if time.monotonic() >= session.deadline and session.status not in {"ok", "context_gap"}:
                session.context_gap = True
                session.status = "context_gap"
                session.message = "人工挑战已过期"
                if session.task and not session.task.done():
                    session.task.cancel()
            return {
                "status": session.status, "session_id": session.session_id,
                "platform": session.platform, "login_session_id": session.login_session_id,
                "expires_in": max(0, int(session.deadline - time.monotonic())),
            }

    async def result(self, session_id: str, platform: str, login_session_id: str,
                     phone: str) -> Optional[Dict[str, Any]]:
        async with self._lock:
            session = self._session
            if session is None or session.session_id != session_id:
                return None
            if not self._matches(session, platform, login_session_id, phone):
                raise PermissionError("挑战会话绑定不匹配")
            if time.monotonic() >= session.deadline and session.status not in {"ok", "context_gap"}:
                session.context_gap = True
                session.status = "context_gap"
                session.message = "人工挑战已过期"
                if session.task and not session.task.done():
                    session.task.cancel()
            if session.status not in {"ok", "context_gap"}:
                return {"status": session.status, "session_id": session.session_id}
            payload: Dict[str, Any] = {
                "status": session.status, "session_id": session.session_id,
                "platform": session.platform, "login_session_id": session.login_session_id,
                "message": session.message,
            }
            if session.status == "ok":
                payload["data"] = dict(session.result)
            else:
                payload["context_gap"] = True
            session.consumed = True
            self._session = None
            return payload

    @classmethod
    def _matches(cls, session: ChallengeSession, platform: str,
                 login_session_id: str, phone: str) -> bool:
        return (
            cls._normalize_platform(platform) == session.platform
            and hmac.compare_digest(str(login_session_id), session.login_session_id)
            and hmac.compare_digest(str(phone), session.phone)
        )


def _host_of(url: str) -> str:
    try:
        from urllib.parse import urlsplit
        return urlsplit(url).netloc[:60]
    except Exception:
        return "<unparsed>"


def _check_secret(provided: str, secret: str) -> bool:
    return bool(provided) and hmac.compare_digest(provided, secret)


def save_face_qr(data_url: str) -> Path:
    """把 data-url 二维码写入 0600 临时文件，供 notify-send 图标展示。"""
    import base64
    import tempfile

    if not data_url.startswith("data:image/"):
        raise ValueError("face_qr_url 必须是图片 data-url")
    encoded = data_url.split(",", 1)
    if len(encoded) != 2:
        raise ValueError("face_qr_url 缺少 base64 数据")
    payload = base64.b64decode(encoded[1], validate=True)
    fd, filename = tempfile.mkstemp(prefix="xianyu-face-", suffix=".png")
    with os.fdopen(fd, "wb") as image_file:
        image_file.write(payload)
    os.chmod(filename, 0o600)
    return Path(filename)


def _url_ok(url: str) -> bool:
    return isinstance(url, str) and url.startswith(("http://", "https://")) and len(url) <= 4096


def build_app(cfg: Dict[str, Any], solver: Solver) -> web.Application:
    secret = cfg["secret"]
    challenge_manager = ManualChallengeManager(solver, cfg)

    async def challenge_start(request: web.Request) -> web.Response:
        # GLM/Kimi 人工挑战：凭证只能来自浏览器真实 SDK 回调。
        if not _check_secret(request.headers.get("X-API-Key", ""), secret):
            return web.json_response({"success": False, "message": "X-API-Key 校验失败"}, status=401)
        try:
            body = await request.json()
        except Exception:
            return web.json_response({"success": False, "message": "请求体不是合法 JSON"}, status=400)
        platform = str(body.get("platform") or "").strip().lower()
        try:
            session = await challenge_manager.start(
                platform, str(body.get("login_session_id") or ""),
                str(body.get("phone") or ""), str(body.get("phone_code") or ""),
                body.get("timeout"),
            )
        except RuntimeError as exc:
            return web.json_response({"success": False, "message": str(exc)}, status=409)
        except ValueError as exc:
            return web.json_response({"success": False, "message": str(exc)}, status=400)
        return web.json_response({
            "success": True, "session_id": session.session_id, "platform": session.platform,
            "login_session_id": session.login_session_id,
            "status": session.status, "expires_in": max(0, int(session.deadline - time.monotonic())),
        })

    async def challenge_result(request: web.Request) -> web.Response:
        if not _check_secret(request.headers.get("X-API-Key", ""), secret):
            return web.json_response({"success": False, "message": "X-API-Key 校验失败"}, status=401)
        try:
            body = await request.json()
        except Exception:
            return web.json_response({"success": False, "message": "请求体不是合法 JSON"}, status=400)
        try:
            payload = await challenge_manager.result(
                request.match_info["session_id"], str(body.get("platform") or ""),
                str(body.get("login_session_id") or ""), str(body.get("phone") or ""),
            )
        except PermissionError as exc:
            return web.json_response({"success": False, "message": str(exc)}, status=409)
        if payload is None:
            return web.json_response({"success": False, "message": "挑战会话不存在或已消费"}, status=404)
        if payload["status"] == "ok":
            return web.json_response({"success": True, **payload})
        if payload["status"] == "context_gap":
            return web.json_response({"success": False, **payload}, status=422)
        return web.json_response({"success": False, **payload})

    async def challenge_status(request: web.Request) -> web.Response:
        if not _check_secret(request.headers.get("X-API-Key", ""), secret):
            return web.json_response({"success": False, "message": "X-API-Key 校验失败"}, status=401)
        try:
            payload = await challenge_manager.status(
                request.match_info["session_id"], str(request.query.get("platform") or ""),
                str(request.query.get("login_session_id") or ""), str(request.query.get("phone") or ""),
            )
        except PermissionError as exc:
            return web.json_response({"success": False, "message": str(exc)}, status=409)
        if payload is None:
            return web.json_response({"success": False, "message": "挑战会话不存在或已消费"}, status=404)
        return web.json_response({"success": True, **payload})

    async def healthz(_: web.Request) -> web.Response:
        return web.json_response({"ok": True})

    async def solve(request: web.Request) -> web.Response:
        # 契约 A：token 续期链路
        try:
            body = await request.json()
        except Exception:
            return web.json_response({"success": False, "message": "请求体不是合法 JSON"})
        if not _check_secret(str(body.get("secret_key") or ""), secret):
            return web.json_response({"success": False, "message": "secret 校验失败"}, status=401)
        url = body.get("url") or ""
        if not _url_ok(url):
            return web.json_response({"success": False, "message": "缺少有效的验证链接 url"})
        status, cookies, url_expired = await solver.solve(
            url, str(body.get("account_id") or ""), MAX_WAIT_CONTRACT_A
        )
        if status == "ok":
            return web.json_response({"success": True, "data": {"cookies": cookies}})
        data: Dict[str, Any] = {}
        if url_expired:
            data["url_expired"] = True
        return web.json_response({"success": False, "message": f"人工求解未完成（{status}）", "data": data})

    async def risk(request: web.Request) -> web.Response:
        # 契约 B：商品监控（monitor_remote_risk_client）
        if not _check_secret(request.headers.get("X-API-Key", ""), secret):
            return web.json_response({"success": False, "message": "X-API-Key 校验失败"}, status=401)
        try:
            body = await request.json()
        except Exception:
            return web.json_response({"success": False, "message": "请求体不是合法 JSON"})
        if str(body.get("type") or "") != "x5sec_ali":
            return web.json_response({"success": False, "message": "不支持的接口类型"}, status=400)
        url = (body.get("data") or {}).get("url") or ""
        if not _url_ok(url):
            return web.json_response({"success": False, "message": "缺少有效的验证链接 url"})
        status, cookies, _ = await solver.solve(url, "monitor", MAX_WAIT_CONTRACT_B)
        if status != "ok":
            return web.json_response({"success": False, "message": f"人工求解未完成（{status}）", "data": {}})
        return web.json_response({
            "success": True,
            "message": "",
            "data": {
                "x5sec": cookies.get("x5sec", ""),
                "bx-pp": cookies.get("bx-pp", ""),
                "bx_et": cookies.get("bx_et", ""),
            },
        })

    async def browser_notify(request: web.Request) -> web.Response:
        # 服务器浏览器模式人脸通知只推送桌面文本，避免依赖第三方渠道。
        if not _check_secret(request.headers.get("X-API-Key", ""), secret):
            return web.json_response({"success": False, "message": "X-API-Key 校验失败"}, status=401)
        try:
            body = await request.json()
        except Exception:
            return web.json_response({"success": False, "message": "请求体不是合法 JSON"})
        message = str(body.get("message") or "").strip()
        if not message:
            return web.json_response({"success": False, "message": "缺少 message"}, status=400)
        try:
            proc = await asyncio.create_subprocess_exec(
                "notify-send", "-u", "critical", "-a", "xianyu-captcha-helper",
                "闲鱼服务器浏览器人脸", message,
                stdout=asyncio.subprocess.DEVNULL,
                stderr=asyncio.subprocess.DEVNULL,
            )
            await asyncio.wait_for(proc.wait(), timeout=5)
        except Exception as exc:
            log(f"服务器浏览器人脸通知发送失败：{exc}")
            return web.json_response({"success": False, "message": "notified failed"}, status=500)
        return web.json_response({"success": True, "message": "notified", "data": {}})

    async def face_notify(request: web.Request) -> web.Response:
        # 人脸验证仅推送本地提醒；二维码瞬时落盘、通知后删除。
        if not _check_secret(request.headers.get("X-API-Key", ""), secret):
            return web.json_response({"success": False, "message": "X-API-Key 校验失败"}, status=401)
        try:
            body = await request.json()
        except Exception:
            return web.json_response({"success": False, "message": "请求体不是合法 JSON"})
        account_id = str(body.get("account_id") or "")
        try:
            qr_path = save_face_qr(str(body.get("face_qr_url") or ""))
        except Exception as exc:
            log(f"人脸二维码校验失败：{exc}")
            return web.json_response({"success": False, "message": "face_qr_url 无效"}, status=400)
        await solver._notify_face(account_id, str(qr_path))
        qr_path.unlink(missing_ok=True)
        return web.json_response({"success": True, "message": "notified", "data": {}})

    app = web.Application(client_max_size=256 * 1024)
    app.router.add_get("/healthz", healthz)
    app.router.add_post("/challenge/start", challenge_start)
    app.router.add_post("/challenge/sdk-start", challenge_start)
    app.router.add_get("/challenge/{session_id}/status", challenge_status)
    app.router.add_post("/challenge/{session_id}/result", challenge_result)
    app.router.add_post("/solve", solve)
    app.router.add_post("/risk", risk)
    app.router.add_post("/browser-notify", browser_notify)
    app.router.add_post("/face-notify", face_notify)
    return app


class _MockSDKRoute:
    def __init__(self) -> None:
        self.body = ""

    async def fulfill(self, *, status: int = 200, content_type: str = "", body: str = "") -> None:
        self.body = body


class _MockSDKPage:
    def __init__(self, outcome: Dict[str, str]):
        self.outcome = outcome
        self.bindings: Dict[str, Any] = {}
        self.closed = False
        self._route_handler: Any = None

    async def expose_binding(self, name: str, callback: Any) -> None:
        self.bindings[name] = callback

    async def route(self, url: str, handler: Any) -> None:
        self._route_handler = handler

    async def goto(self, url: str, **_: Any) -> None:
        route = _MockSDKRoute()
        await self._route_handler(route)
        await self._deliver(route.body)

    async def set_content(self, html: str, **_: Any) -> None:
        await self._deliver(html)

    async def _deliver(self, html: str) -> None:
        if KIMI_CAPTCHA_ID in html and "validate" in self.outcome:
            await self.bindings["challengeComplete"](None, {"validate": self.outcome["validate"]})
        elif GLM_CAPTCHA_ORGANIZATION in html:
            payload = {key: value for key, value in self.outcome.items() if key in {"rid", "md5"}}
            if payload:
                await self.bindings["challengeComplete"](None, payload)
            else:
                await self.bindings["challengeFail"](None, "missing_md5")

    def is_closed(self) -> bool:
        return self.closed


class _MockSDKContext:
    def __init__(self, outcome: Dict[str, str]):
        self.page = _MockSDKPage(outcome)

    async def new_page(self) -> _MockSDKPage:
        return self.page


class _MockSDKBrowser:
    def __init__(self, outcome: Dict[str, str]):
        self.context = _MockSDKContext(outcome)

    async def new_context(self, **_: Any) -> _MockSDKContext:
        return self.context

    async def close(self) -> None:
        pass


class _MockSDKChromium:
    def __init__(self, outcome: Dict[str, str]):
        self.outcome = outcome

    async def launch(self, **_: Any) -> _MockSDKBrowser:
        return _MockSDKBrowser(self.outcome)


class _MockSDKPlaywright:
    def __init__(self, outcome: Dict[str, str]):
        self.chromium = _MockSDKChromium(outcome)


async def _challenge_selftest(cfg: Dict[str, Any]) -> bool:
    async def finish(manager: ManualChallengeManager, session: ChallengeSession) -> None:
        if session.task:
            await session.task

    kimi_solver = Solver(cfg)
    kimi_solver.pw = _MockSDKPlaywright({"validate": "real-callback-validate"})
    kimi = ManualChallengeManager(kimi_solver, cfg)
    kimi_session = await kimi.start("kimi", "login-kimi", "13800000000", timeout=2)
    try:
        await kimi.start("glm", "busy", "13800000001", "86", timeout=2)
        return False
    except RuntimeError:
        pass
    await finish(kimi, kimi_session)
    try:
        await kimi.result(kimi_session.session_id, "glm", "login-kimi", "13800000000")
        return False
    except PermissionError:
        pass
    kimi_result = await kimi.result(kimi_session.session_id, "kimi", "login-kimi", "13800000000")
    if not kimi_result or kimi_result.get("data") != {"validate": "real-callback-validate"}:
        return False
    if await kimi.result(kimi_session.session_id, "kimi", "login-kimi", "13800000000") is not None:
        return False

    glm_solver = Solver(cfg)
    glm_solver.pw = _MockSDKPlaywright({"rid": "real-rid"})
    glm = ManualChallengeManager(glm_solver, cfg)
    glm_session = await glm.start("glm", "login-glm", "13900000000", "86", timeout=2)
    await finish(glm, glm_session)
    glm_result = await glm.result(glm_session.session_id, "glm", "login-glm", "13900000000")
    if not glm_result or glm_result.get("status") != "context_gap" or "data" in glm_result:
        return False

    expiry_solver = Solver(cfg)
    expiry_solver.pw = _MockSDKPlaywright({})
    expiry = ManualChallengeManager(expiry_solver, cfg)
    expired_session = ChallengeSession(
        session_id="expired", platform="kimi", login_session_id="login-expired",
        phone="13700000000", phone_code="", deadline=time.monotonic() - 1,
    )
    expiry._session = expired_session
    expired_result = await expiry.result("expired", "kimi", "login-expired", "13700000000")
    return bool(expired_result and expired_result.get("status") == "context_gap")


async def _selftest() -> int:
    """无人工自检：headless 打开本地页面（Set-Cookie x5sec=...），验证成功路径全链。"""
    from aiohttp import web as aio_web

    async def index(_: aio_web.Request) -> aio_web.Response:
        resp = aio_web.Response(text="<html><body>selftest</body></html>")
        resp.set_cookie("x5sec", "selftest-value", path="/")
        resp.set_cookie("bx-pp", "pp-value", path="/")
        return resp

    cfg = {
        "secret": "selftest", "bind": "127.0.0.1", "port": 0, "headless": True,
        "notify": False, "post_success_keep_secs": 0, "max_wait": 20,
        "challenge_max_wait": 5, "browser_channel": "",
    }
    if not await _challenge_selftest(cfg):
        log("SELFTEST FAIL：GLM/Kimi SDK 挑战会话契约失败")
        return 1
    solver = Solver(cfg)
    await solver.start()
    try:
        dummy = aio_web.Application()
        dummy.router.add_get("/", index)
        runner = aio_web.AppRunner(dummy)
        await runner.setup()
        site = aio_web.TCPSite(runner, "127.0.0.1", 0)
        await site.start()
        port = runner.addresses[0][1]

        status, cookies, url_expired = await solver.solve(f"http://127.0.0.1:{port}/", "selftest", 20)
        await runner.cleanup()
        if status == "ok" and cookies.get("x5sec") == "selftest-value" and cookies.get("bx-pp") == "pp-value":
            log("SELFTEST PASS：Kimi SDK 回调、GLM 缺 md5、重复/超时/绑定失败及 cookie 求解链全部正常")
            return 0
        log(f"SELFTEST FAIL：status={status} cookies={cookies} url_expired={url_expired}")
        return 1
    finally:
        await solver.stop()


async def _amain(cfg: Dict[str, Any], port_override: Optional[int]) -> None:
    solver = Solver(cfg)
    await solver.start()
    app = build_app(cfg, solver)
    runner = web.AppRunner(app)
    await runner.setup()
    bind = cfg.get("bind", "127.0.0.1")
    port = port_override or int(cfg.get("port", DEFAULT_PORT))
    site = web.TCPSite(runner, bind, port)
    await site.start()
    log(f"xianyu-captcha-helper 监听 {bind}:{port}（/healthz /solve /risk /challenge/sdk-start /challenge/{{id}}/status /challenge/{{id}}/result /browser-notify /face-notify）")
    try:
        while True:
            await asyncio.sleep(3600)
    finally:
        await runner.cleanup()
        await solver.stop()


def main() -> int:
    parser = argparse.ArgumentParser(description="闲鱼本地人工打码助手")
    parser.add_argument("--config", default=str(DEFAULT_CONFIG_PATH))
    parser.add_argument("--port", type=int, default=None)
    parser.add_argument("--secret", default=None, help="覆盖配置中的 secret（测试用）")
    parser.add_argument("--selftest", action="store_true", help="无人工自检（headless）")
    args = parser.parse_args()
    if args.selftest:
        return asyncio.run(_selftest())
    cfg = load_or_create_config(Path(args.config))
    if args.secret:
        cfg["secret"] = args.secret
    if args.port:
        cfg["port"] = args.port
    try:
        asyncio.run(_amain(cfg, args.port))
    except KeyboardInterrupt:
        log("退出")
    return 0


if __name__ == "__main__":
    sys.exit(main())
