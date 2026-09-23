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
- 契约 C（GLM 同会话发码挑战，2026-09-22 会话边界收敛裁定）：
    POST /challenge/sdk-start body 新增可选顶层键 {"x_timestamp","x_nonce","x_sign"}
    （Lane G 的 webZhipuComputeSign 生成下发；未下发时 helper 按同一已取证算法
    本地计算）。
    GLM 流程：浏览器导航 chatglm.cn 真实 origin → 等待匿名会话建立（chatglm_token
    guest Cookie + JWT payload 的 device_id claim）→ 同一浏览器上下文注入官方数美 SDK
    人工滑块 → 通过后**同源同出口** fetch POST /chatglm/user-api/user/login_captcha
    （body 六键 {phone, phone_code:"+86", pic_captcha_id, tm, fr, distinct_id}，
    Cookie 由浏览器自动携带，不手工组 Cookie 头、不伪造）→ 成功返回
    data={rid, phone_code, send_status, send_body_status, send_message, cookies,
    device_id}。任一环节失败（会话未建立/滑块失败/发码非 2xx 或 body.status 非 0）
    → context_gap，不造成功。Cookie/JWT/device_id/rid 仅内存传递，绝不落日志。
    Kimi 流程零改动。

人工流程：请求到达 → 桌面通知 → 弹出有头 Chromium 加载验证链接 → 人工拖滑块 →
检测到 x5* cookie（与 orchestrator._has_x5sec 同口径：名称以 x5 开头或含 x5sec）→
进一步验证等待门（2026-09-23：风控升级后滑块通过时页面可能仍要求手机号登录，
命中进一步验证文案则通知并保持页面等人工完成，凭证以最新快照回传；无进一步验证
则维持原停留语义）→ 按契约返回 → 浏览器停留数秒供确认后关闭。

失败语义：超时 / 忙 / 浏览器被关闭 / 页面异常 → 返回失败，Worker 编排自动回退
本机真实鼠标与 Playwright 引擎，不劣于现状。

超时上限：契约 A 的 Worker 读超时下限为 300s（remote_timeout.get_remote_solve_timeout），
本端 deadline 固定压在 285s 内；契约 B（商品监控）总超时 120s，deadline 压在 110s 内。

隐私：不请求、不存储账号 Cookie；pass_cookies 开启时请求体携带的 Cookie 仅存在于
请求内存对象中。日志不打印任何 cookie 值与完整验证链接。GLM 同会话发码产生的
chatglm 匿名会话 Cookie / device_id / rid 仅存在于挑战会话内存对象中，随结果一次性
回传后即丢弃；日志只记键名与状态码，绝不记 Cookie/JWT/rid/device_id 值。

配置：~/.config/xianyu-captcha-helper/config.json（首次运行自动生成 32 位 secret）。
"""
from __future__ import annotations

import argparse
import asyncio
import hashlib
import hmac
import json
import os
import secrets
import sys
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Optional

from aiohttp import web
from playwright.async_api import async_playwright

DEFAULT_CONFIG_PATH = Path.home() / ".config" / "xianyu-captcha-helper" / "config.json"
DEFAULT_PORT = 18089
# 下列参数来自官方线上 bundle 的既有取证，不从请求接收，也不允许配置成任意脚本 URL。
KIMI_CAPTCHA_SDK_URL = "https://cstaticdun.126.net/load.min.js"
KIMI_CAPTCHA_ID = "2752f01d87dc45948de1a1e0ac2b7160"
GLM_CAPTCHA_SDK_URL = "https://chatglm.cn/smcp/smcp.min.js"
GLM_CAPTCHA_ORGANIZATION = "599zinRadlRxTLrOkTR9"
# --- GLM 同会话发码（契约 C，2026-09-22 会话边界收敛裁定）---
# GLM_ORIGIN 官方真实站点 origin：GLM 流程必须导航到该 origin 建立真实匿名会话，
# 不得再用本地空白页/路由注入页承载（本地源无站点 Cookie/device_id → rid 与发码
# 请求为两个客户端上下文，上游 400）。取 rid 与发码均在该 origin 内完成。
GLM_ORIGIN = "https://chatglm.cn"
# GLM_SEND_SMS_PATH 发码端点（2026-09-22 21:40 生产抓包取证，与后端
# webZhipuSendSMSCodeEndpoint 同一常量）：POST，body 六键，成功 ⇔ HTTP 2xx 且
# body.status==0（历史白名单 code==0/ret==0/success==true 回退兼容）。
GLM_SEND_SMS_PATH = "/chatglm/user-api/user/login_captcha"
# GLM_SIGN_SALT 签名盐值：与后端 webZhipuSignSalt 同源（2026-09-17 官方 main.js
# 逆向 + 真实抓包验证）。公开常量，严禁将任何 Cookie/Token/登录态内容写入本常量
# 或周边注释。
GLM_SIGN_SALT = "8a1317a7468aa3ad86e997d08f3f31cb"
# 挑战页必须运行在**真实 http 源**下：about:blank / data: 等不透明源会被浏览器拒绝
# 读取 document.cookie（"Access is denied for this document"），导致易盾/数美 SDK 初始化
# 抛异常、页面卡在“正在加载官方验证组件…”。该地址仅作为页面源，HTML 由 Playwright 路由
# 拦截注入，不实际访问该路径（端口无需与监听端口一致）。
CHALLENGE_PAGE_URL = "http://127.0.0.1:18089/__challenge_page__"
# 与 Worker 读超时对齐：契约 A ≥300s、契约 B =120s，各留网络余量。
MAX_WAIT_CONTRACT_A = 285
MAX_WAIT_CONTRACT_B = 110
EXPIRED_MARKER = "页面访问出现了问题"
# 进一步验证判定标记（2026-09-23 用户裁定：风控升级后滑块通过后页面可能仍要求
# 手机号登录，通过时刻的 x5sec 快照不等于终态）。命中任一文案即视为"还需要进一步
# 验证"：保持页面打开等人工完成，而不是按 post_success_keep_secs 直接关浏览器。
# 页面真实文案目前为 context_gap（无 DOM/截图取证），此表按用户描述的"手机号登录"
# 语义给保守电话特征集；实际页面用词不同时，改配置 further_verify_markers 即可，
# 不需要改代码。刻意不含"验证码""安全验证"等宽泛词——滑块挑战页自身文案会误命中。
DEFAULT_FURTHER_VERIFY_MARKERS = (
    "手机号登录",
    "手机号登陆",
    "短信验证码",
    "短信登录",
    "请输入手机号",
    "获取验证码",
    "验证手机号",
)


def log(msg: str) -> None:
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}", flush=True)


def _zhipu_sign_timestamp(now_ms: int) -> str:
    """官方 x-timestamp 变换（与后端 webZhipuSignTimestamp 同一算法，勿改）：
    now 为 13 位毫秒串；t = sum(digits) - digits[len-2]；
    返回 now[0:len-2] + str(t%10) + now[len-1]。纯函数，自检覆盖黄金用例。"""
    now = str(now_ms)
    if len(now) < 3:
        return now
    digits = [int(c) for c in now]
    t = sum(digits) - digits[-2]
    return now[:-2] + str(t % 10) + now[-1]


def _zhipu_sign_from(x_timestamp: str, x_nonce: str) -> str:
    """x-sign = md5(xTimestamp + "-" + xNonce + "-" + salt)，32 位小写 hex
    （与后端 webZhipuSignFrom 同一算法，勿改）。纯函数，自检覆盖黄金用例。"""
    raw = x_timestamp + "-" + x_nonce + "-" + GLM_SIGN_SALT
    return hashlib.md5(raw.encode()).hexdigest()


def _zhipu_sign_triplet() -> Dict[str, str]:
    """签名三件套（x-timestamp / x-nonce / x-sign），与后端 webZhipuComputeSign
    同一算法。仅当 sdk-start 未下发 sign 参数时本地兜底生成（任务卡裁定：以下发
    为准，禁止发明第二套算法——本函数是同一取证算法的移植，非第二套）。"""
    x_timestamp = _zhipu_sign_timestamp(int(time.time() * 1000))
    x_nonce = uuid.uuid4().hex
    return {"timestamp": x_timestamp, "nonce": x_nonce, "sign": _zhipu_sign_from(x_timestamp, x_nonce)}


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
        "further_verify_wait": True,
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

    async def _notify(self, account_id: str, deadline: int, message: str = "") -> None:
        if not self.cfg.get("notify", True):
            return
        if not message:
            message = f"账号 {account_id or '?'} 需要过滑块，请在弹出的浏览器中完成（限时 {deadline}s）"
        try:
            proc = await asyncio.create_subprocess_exec(
                "notify-send", "-u", "critical", "-a", "xianyu-captcha-helper",
                "闲鱼人工验证",
                message,
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

    async def _wait_further_verification(
        self,
        page: Any,
        cookies_fn: Any,
        account_id: str,
        markers: tuple,
        loop_start: float,
        deadline: int,
    ) -> Dict[str, str]:
        """滑块通过后的"进一步验证"等待门（2026-09-23 用户裁定）。

        风控等级上升后，滑块通过时页面可能仍要求进一步验证（如手机号登录）。
        通过时刻的 x5sec 快照不等于终态：此时直接按 keep_secs 关浏览器，会把
        还没做完验证的人工页面关掉。本方法在命中通过凭证后被调用：

        - 页面文案命中任一 marker → 桌面通知一次，保持页面打开等人工完成
        - 人工完成（标记消失）→ 返回等待期间捕获的最新 x5* 凭证快照（登录后
          凭证可能轮换，以最新为准）
        - 人工手动关页面 → 视为结束，按已捕获快照返回
        - 达到求解 deadline → 按已捕获快照返回（通过凭证已到手，绝不因等待
          把 ok 降级成 fail——最坏只是回传变晚，不劣于旧行为）

        三个终态出口（标记消失/deadline/页面关闭）统一经 _final_snapshot 最后
        读一次当前凭证，捕获收口瞬间的轮换（外审 R2）；页面内容读取失败（跳转
        窗口）绝不视为"标记消失"，保留等待状态下一轮再查。

        Args:
            cookies_fn: 异步 callable，返回 Playwright 原始 cookie 列表
                （context.cookies() 的协程结果）；门内 await 后按 x5*/bx* 口径过滤。

        Returns:
            等待期间捕获到的凭证 cookie dict；无新凭证（或无进一步验证、页面
            提前关闭）时为空 dict，调用方沿用通过时刻的快照。
        """

        async def _read_fresh() -> Dict[str, str]:
            """异步读取并过滤当前 cookie 快照；异常向上抛由调用方按既定口径处理。"""
            return self._x5_cookies(await cookies_fn())

        async def _final_snapshot(reason: str) -> Dict[str, str]:
            """收口统一出口：最后尽力读一次当前凭证，捕获收口瞬间的轮换。"""
            nonlocal latest
            try:
                fresh = await _read_fresh()
            except Exception as exc:
                log(f"等待门收口前读取 cookie 失败（{reason}）account={account_id}：{exc}")
                return latest
            if fresh and self._has_pass_signal(fresh):
                latest = fresh
            return latest

        latest: Dict[str, str] = {}
        notified = False
        while True:
            elapsed = time.monotonic() - loop_start
            if elapsed >= deadline:
                log(f"进一步验证等待达到求解 deadline account={account_id}，按已捕获凭证返回")
                return await _final_snapshot("deadline")
            try:
                if page.is_closed():
                    log(f"页面在进一步验证等待期间被关闭 account={account_id}")
                    return await _final_snapshot("页面关闭")
            except Exception:
                pass
            try:
                content = await page.content()
            except Exception as exc:
                # 跳转瞬间 content() 可能抛错：这绝不等于"标记消失"，不能在
                # 最需要等待的跳转窗口提前收口（外审 R2）——保留等待状态下一轮再查。
                log(f"等待门读取页面内容失败（跳转窗口，继续等待）account={account_id}：{exc}")
                await asyncio.sleep(1.0)
                continue
            hit = next((m for m in markers if m and m in content), "")
            if not hit:
                if notified:
                    log(f"进一步验证已完成/消失 account={account_id}")
                return await _final_snapshot("标记消失")
            if not notified:
                notified = True
                remain = max(1, int(deadline - elapsed))
                log(
                    f"检测到进一步验证（疑似手机号登录）account={account_id} "
                    f"marker={hit}，保持页面等待人工完成（剩余 {remain}s）"
                )
                await self._notify(
                    account_id,
                    remain,
                    message=(
                        f"账号 {account_id or '?'} 滑块已通过，但页面仍要求进一步验证"
                        f"（{hit}），请在浏览器中完成；完成后页面自动关闭（剩余 {remain}s）"
                    ),
                )
            try:
                fresh = await _read_fresh()
            except Exception as exc:
                log(f"进一步验证等待期间读取 cookie 失败 account={account_id}：{exc}")
                fresh = {}
            if fresh and self._has_pass_signal(fresh):
                latest = fresh
            await asyncio.sleep(1.0)

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
            further_wait = bool(self.cfg.get("further_verify_wait", True))
            markers = tuple(
                str(m) for m in (self.cfg.get("further_verify_markers") or DEFAULT_FURTHER_VERIFY_MARKERS)
            )
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
                        # 进一步验证等待门（仅有人工在场时有意义）：风控升级后滑块通过
                        # 时页面可能仍要求手机号登录，此时等人工完成再取最新凭证回传。
                        # 等待无凭证轮换时沿用通过时刻快照；deadline 内未完成也不把
                        # ok 降级为 fail（凭证已到手，最坏只是回传变晚）。
                        if further_wait and not headless:
                            try:
                                held = await self._wait_further_verification(
                                    page,
                                    lambda: context.cookies(),
                                    account_id,
                                    markers,
                                    loop_start,
                                    deadline,
                                )
                            except Exception as gate_exc:
                                # 等待门自身异常绝不把已到手的 ok 降级为 fail
                                log(f"进一步验证等待门异常（按通过时刻快照返回）account={account_id}：{gate_exc}")
                                held = {}
                            if held:
                                cookies = held
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


# ---------------------------------------------------------------------------
# GLM 同会话挑战注入（契约 C）：以下 JS 由 helper 注入到 chatglm.cn 真实 origin
# 的官方登录页 DOM 中运行（不导航本地页、不替换文档、不伪造挑战值、不硬编码令牌）。
# ---------------------------------------------------------------------------

# GLM_MOUNT_JS 在官方登录页上挂载数美滑块组件（同源 SDK + 同源容器，官方页面
# 自身 Cookie/localStorage/device 状态全部保留）。该 JS 由 page.evaluate 执行，
# 通过回传 challengeComplete/challengeFail（page.expose_binding 提供的同源绑定），
# 不改写页面文档。
GLM_MOUNT_JS = """
(containerId) => {
  if (document.getElementById('glm-helper-host')) {
    return { mounted: true, reused: true };
  }
  const host = document.createElement('div');
  host.id = 'glm-helper-host';
  host.setAttribute('style',
    'position:fixed;top:0;left:0;right:0;bottom:0;z-index:2147483647;' +
    'background:rgba(255,255,255,0.98);display:flex;flex-direction:column;' +
    'align-items:center;justify-content:flex-start;padding:24px 12px;' +
    'box-sizing:border-box;overflow-y:auto;font-family:system-ui,sans-serif;');
  const title = document.createElement('h3');
  title.textContent = '请在此窗口完成人工验证';
  title.setAttribute('style', 'margin:0 0 8px;color:#111;');
  const status = document.createElement('p');
  status.id = 'glm-helper-status';
  status.textContent = '正在加载官方验证组件…';
  status.setAttribute('style', 'margin:0 0 12px;color:#444;font-size:14px;');
  const box = document.createElement('div');
  box.id = 'glm-captcha';
  box.setAttribute('style', 'width:100%;max-width:480px;min-height:420px;');
  host.appendChild(title); host.appendChild(status); host.appendChild(box);
  document.body.appendChild(host);
  window.__glmHelperFail = (message) => {
    try { status.textContent = message || '官方验证组件未完成'; } catch (e) {}
    window.challengeFail(message || 'sdk_error');
  };
  const script = document.createElement('script');
  script.src = %SDK_URL%;
  script.onload = () => {
    if (typeof window.initSMCaptcha !== 'function') return window.__glmHelperFail('数美 SDK 未就绪');
    // 2026-09-21 双重实证（chatglm.cn 线上 smcp.min.js 行为 + 官方 main bundle 集成
    // 代码逆向）：新版 API 为 initSMCaptcha(options, instanceCallback)，实例经第二
    // 个参数回调交付；instance.onSuccess(data)，data 至少含 {rid, pass}。
    try {
      window.initSMCaptcha({
        organization: %ORGANIZATION%, appendTo: '#glm-captcha',
        product: 'embed', width: '100%'
      }, (instance) => {
        if (!instance || typeof instance.onSuccess !== 'function') {
          return window.__glmHelperFail('数美组件实例未交付');
        }
        instance.onSuccess((data) => {
          if (!data || typeof data.rid !== 'string' || !data.rid) {
            return window.__glmHelperFail('数美回调缺少 rid');
          }
          if (data.pass === false) return window.__glmHelperFail('数美验证未通过');
          try { status.textContent = '滑块通过，正在发送验证码…'; } catch (e) {}
          // 只回传 rid：md5 正常滑块流不带（2026-09-22 取证），不猜测、不伪造。
          window.challengeComplete({ rid: data.rid });
        });
        if (typeof instance.onError === 'function') {
          instance.onError((e) => window.__glmHelperFail('数美验证失败: ' + String(e && e.msg || e).slice(0, 80)));
        }
      });
    } catch (e) { return window.__glmHelperFail('数美组件初始化异常: ' + String(e.message).slice(0, 80)); }
    // 2026-09-23 Playwright 探针实证（chatglm.cn 真实页 headless 注入同款挂载 JS）：
    // 官方数美 SDK 渲染滑块节点（#glm-captcha .shumei_captcha）需要 ~3s（t=2s 无
    // 节点、t=3s 节点出现）。原固定 3s 一次性兜底与渲染赛跑，慢几百毫秒即误杀
    // （2026-09-23 10:30:32 生产日志实证）；原 6s"实例未交付"兜底同理可能过早，
    // 已移除——实例交付以 onSuccess 回调为准，渲染等待已覆盖 SDK 加载慢的场景；
    // 若实例回调始终不交付，由 Python 侧既有挑战 deadline 失败关闭，不新增第二套
    // 超时。现改为 500ms 轮询：节点出现即清除轮询进入成功路径（等 onSuccess 回调）；
    // 自 script.onload 起累计 RENDER_TIMEOUT_MS（生产 20s，_run_glm 注入）仍无
    // 节点才失败关闭。
    const renderWaitStart = Date.now();
    const renderPoll = setInterval(() => {
      if (document.querySelector('#glm-captcha .shumei_captcha')) {
        clearInterval(renderPoll);
        return;
      }
      if (Date.now() - renderWaitStart >= %RENDER_TIMEOUT_MS%) {
        clearInterval(renderPoll);
        window.__glmHelperFail('数美组件渲染失败（容器无滑块节点，等待 ' + Math.round(%RENDER_TIMEOUT_MS% / 1000) + 's）');
      }
    }, 500);
  };
  script.onerror = () => window.__glmHelperFail('数美 SDK 加载失败');
  document.head.appendChild(script);
  return { mounted: true, reused: false };
}
"""

# GLM_SEND_SMS_JS 同会话同源发码（在 chatglm.cn 页面上下文 evaluate 执行）。
# 证据链（全部既有取证，不发明）：端点/body 六键 = 2026-09-22 21:40 生产抓包；
# 请求头 = 2026-09-22 官网发码抓包（匿名会话 Cookie + x-device-id 携带）与后端
# applyZhipuFingerprintHeaders 同构（app-name/x-app-*/x-lang）；x-device-id 取自
# 页面匿名会话 chatglm_token JWT payload 的 device_id claim（与后端
# webZhipuDeviceIDFromToken 同口径）；x-request-id 每请求随机（与后端
# webZhipuUUIDHex 同语义）；Origin/Referer/User-Agent 为浏览器管制头，同源 fetch
# 自动取真实页面值（不伪造）；签名三件套由后端经 sdk-start 下发
# （webZhipuComputeSign 生成），JS 侧不实现第二套算法。Cookie 由浏览器对同源
# 请求自动携带，不手工组 Cookie 头。
# 返回 {status, bodyStatus, message, device_id}：message 已截断脱敏；device_id
# 仅随 result 内存回传（Lane G 存 challenge session 供登录步复用），绝不落日志。
GLM_SEND_SMS_JS = """
async ({path, body, sign}) => {
  const clampText = (v, n) => {
    let s = '';
    try { s = String(v == null ? '' : v); } catch (e) { s = '<unprintable>'; }
    return s.slice(0, n);
  };
  const result = { status: 0, bodyStatus: null, message: '', device_id: '' };
  try {
    result.device_id = (() => {
      try {
        const m = document.cookie.match(/(?:^|;\\s*)chatglm_token=([^;]+)/);
        if (!m) return '';
        const part = decodeURIComponent(m[1]).split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
        const json = JSON.parse(atob(part + '='.repeat((4 - part.length % 4) % 4)));
        return typeof json.device_id === 'string' ? json.device_id : '';
      } catch (e) { return ''; }
    })();
    const headers = {
      'Content-Type': 'application/json',
      'Accept': 'application/json',
      'app-name': 'chatglm',
      'x-app-platform': 'pc',
      'x-app-version': '0.0.1',
      'x-app-fr': 'default',
      'x-lang': 'zh',
      'x-request-id': crypto.randomUUID().replace(/-/g, '')
    };
    if (sign && sign.timestamp && sign.nonce && sign.sign) {
      headers['x-timestamp'] = String(sign.timestamp);
      headers['x-nonce'] = String(sign.nonce);
      headers['x-sign'] = String(sign.sign);
    }
    if (result.device_id) {
      headers['x-device-id'] = result.device_id;
    }
    const resp = await fetch(path, { method: 'POST', headers, body: JSON.stringify(body) });
    result.status = resp.status;
    if (resp.status >= 200 && resp.status < 300) {
      let top = null;
      try { top = await resp.json(); } catch (e) { result.message = 'response_not_json'; }
      if (top && typeof top === 'object') {
        const n = (v) => { const x = typeof v === 'string' ? Number(v) : v; return (typeof x === 'number' && isFinite(x)) ? x : null; };
        const st = n(top.status); const cd = n(top.code); const rt = n(top.ret);
        if (st !== null) result.bodyStatus = st;
        else if (cd !== null) result.bodyStatus = cd;
        else if (rt !== null) result.bodyStatus = rt;
        else if (top.success === true) result.bodyStatus = 0;
        else if (top.success === false) result.bodyStatus = -1;
        const msg = [top.message, top.msg, top.detail].find((v) => typeof v === 'string' && v);
        result.message = clampText(msg || '', 80);
      }
    } else {
      result.message = 'http_' + resp.status;
    }
  } catch (e) {
    result.status = -1;
    result.message = clampText(e && e.name || 'fetch_error', 40);
  }
  return result;
}
"""


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
    # sign Lane G 经 sdk-start body 顶层键 x_timestamp/x_nonce/x_sign 下发的签名
    # 三件套（规范化后内部表示 {timestamp, nonce, sign}）；
    # None 表示未下发，GLM 发码时按同一取证算法本地生成）。
    sign: Optional[Dict[str, str]] = None
    # send_context GLM 同会话发码中间产物（仅内存）：滑块通过后由 _run 填充
    # {cookies(原始 cookie 对象列表), cookie_string(document.cookie), device_id}，
    # 供 _send_glm_sms_code 组请求、并经 _glm_send_result 并入 result 回传。
    # 绝不写入日志。
    send_context: Dict[str, Any] = field(default_factory=dict)


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
        """返回仅加载已取证官方 SDK 的本地页面；不会导航到 SDK 脚本 URL。

        GLM 流程不再使用本页承载（2026-09-22 会话边界裁定：rid 必须产自
        chatglm.cn 真实 origin 的同浏览器上下文，见 GLM_MOUNT_JS 注释与
        _run_glm 的注入路径）；本页仅继续服务 kimi（易盾，无站点会话依赖）。"""
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
        # GLM：本地页不再加载数美 SDK（滑块已改在 chatglm.cn 真实 origin 上注入，
        # 见 _run_glm）。此分支仅为路由兜底占位——正常流程不会导航到本页。
        return common + """
<p id="status">GLM 挑战请在 chatglm.cn 页面中完成</p>
<script>
window.challengeFail('glm_challenge_misrouted_to_local_page');
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
                    phone_code: str = "", timeout: Optional[int] = None,
                    sign: Optional[Dict[str, str]] = None) -> ChallengeSession:
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
        # sign（可选）：Lane G 经 sdk-start body 顶层键 x_timestamp/x_nonce/x_sign
        # 下发的签名三件套（webZhipuComputeSign 生成，Go 侧 omitempty 省键）。
        # 规范化为内部 {timestamp, nonce, sign} 表示；三键任一缺失或长度越界 →
        # 按未下发处理（GLM 发码时本地按同一取证算法生成）。值不落日志。
        if isinstance(sign, dict):
            candidate = {
                "timestamp": str(sign.get("x_timestamp") or "").strip(),
                "nonce": str(sign.get("x_nonce") or "").strip(),
                "sign": str(sign.get("x_sign") or "").strip(),
            }
            if all(candidate.values()) and all(len(v) <= 64 for v in candidate.values()):
                sign = candidate
            else:
                sign = None
        else:
            sign = None
        async with self._lock:
            await self._release_expired_locked()
            if self._session is not None:
                raise RuntimeError("已有人工挑战占用单槽位")
            wait = self.max_wait if timeout is None else max(1, min(int(timeout), self.max_wait))
            session = ChallengeSession(
                session_id=secrets.token_urlsafe(24), platform=platform,
                login_session_id=login_session_id, phone=phone, phone_code=phone_code,
                deadline=time.monotonic() + wait, sign=sign,
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

    # ------------------------------------------------------------------
    # GLM 同会话发码（契约 C，2026-09-22 会话边界收敛裁定）
    # ------------------------------------------------------------------

    @staticmethod
    async def _session_cookies(context: Any) -> List[Dict[str, Any]]:
        """读取 chatglm.cn 域下的浏览器 Cookie 对象列表（原始结构仅内存使用）。"""
        try:
            raw = context.cookies(GLM_ORIGIN)
        except TypeError:
            raw = context.cookies(GLM_ORIGIN)  # pragma: no cover - 同形态重试兜底
        if asyncio.iscoroutine(raw) or hasattr(raw, "__await__"):
            raw = await raw
        return [c for c in (raw or []) if isinstance(c, dict)]

    @staticmethod
    def _cookie_header(cookies: List[Dict[str, Any]]) -> str:
        """把浏览器 Cookie 对象列表组装为 Cookie 头串（仅内存/回传，绝不落日志）。

        只收 chatglm.cn 域的会话键（含 chatglm_token guest 等匿名会话），剔除
        探针类键。值不参与任何日志输出。"""
        probe = {"bx-cookie-test"}
        parts = []
        for c in cookies:
            name = str(c.get("name", ""))
            if not name or name.lower() in probe:
                continue
            parts.append(name + "=" + str(c.get("value", "")))
        return "; ".join(parts)

    @classmethod
    def _glm_send_result(cls, session: ChallengeSession, send: Dict[str, Any]) -> None:
        """把同会话发码结果并入 session.result（失败关闭语义由调用方判定）。

        回传键（任务卡裁定）：rid（已由 challengeComplete 填充）、phone_code、
        send_status（上游 HTTP 状态）、send_body_status（响应体 status 键值，
        发码专用判定回退 code/ret/success，与后端 zhipuSendBizSuccess 同口径）、
        send_message（≤80 字符脱敏）、cookies（chatglm 匿名会话 Cookie 串）、
        device_id（匿名会话 chatglm_token JWT payload device_id claim）。
        Cookie/JWT/device_id 值绝不写日志。"""
        result = dict(session.result)
        result["phone_code"] = session.phone_code
        result["send_status"] = str(int(send.get("status") or 0))
        body_status = send.get("bodyStatus")
        result["send_body_status"] = "" if body_status is None else str(body_status)
        result["send_message"] = str(send.get("message") or "")[:80]
        result["cookies"] = cls._cookie_header(session.send_context.get("cookies") or [])
        result["device_id"] = str(send.get("device_id") or "")
        session.result = result

    async def _run_glm(self, session: ChallengeSession) -> None:
        """GLM 同会话链路：真实 origin 导航 → 匿名会话建立 → 注入数美滑块 →
        rid 回调 → 同浏览器上下文同源 fetch 发码。任一环节失败 → context_gap，
        不造成功。"""
        browser = None
        done = asyncio.Event()
        callback_result: Dict[str, str] = {}
        callback_failure = ""

        async def challenge_complete(_source: Any, payload: Any) -> None:
            if not isinstance(payload, dict):
                return
            # 只记键名不记值：实锤数美 onSuccess 载荷字段集。
            log(f"SDK 挑战回调字段 platform=glm keys={sorted(payload.keys())}")
            values = {
                field_name: str(payload.get(field_name) or "").strip()
                for field_name in ("rid",)
                if isinstance(payload.get(field_name), str) and str(payload.get(field_name)).strip()
            }
            callback_result.clear()
            callback_result.update(values)
            done.set()

        async def challenge_fail(_source: Any, reason: Any = None) -> None:
            nonlocal callback_failure
            callback_failure = str(reason or "sdk_error")[:80]
            done.set()

        def _fail(message: str) -> None:
            session.context_gap = True
            session.status = "context_gap"
            session.message = message
            # 失败原因不含任何凭据值（SDK 文案/状态码/超时等），记日志供诊断。
            log(f"GLM 同会话挑战失败：{message[:120]}")

        try:
            if self.solver.pw is None:
                raise RuntimeError("求解器未初始化")
            headless = bool(self.solver.cfg.get("headless", False))
            channel = self.solver.cfg.get("browser_channel") or None
            browser = await self.solver.pw.chromium.launch(
                headless=headless, channel=channel,
                args=["--window-size=560,760", "--disable-blink-features=AutomationControlled"],
            )
            context = await browser.new_context(locale="zh-CN", no_viewport=True)
            page = await context.new_page()
            await page.expose_binding("challengeComplete", challenge_complete)
            await page.expose_binding("challengeFail", challenge_fail)

            # 1) 导航到 chatglm.cn 真实 origin（官方首页）。滑块组件按 organization
            #    取证在站点任一页面可挂载，注入脚本不依赖具体路径；人工无需在页内
            #    再导航（导航会丢失注入的宿主容器）。
            session.status = "running"
            log(f"已打开 GLM 同会话人工挑战 origin={_host_of(GLM_ORIGIN)} deadline={max(0, int(session.deadline - time.monotonic()))}s")
            await page.goto(GLM_ORIGIN + "/", wait_until="domcontentloaded", timeout=30_000)

            # 2) 等待匿名会话建立：chatglm.cn 域下出现 Cookie（页面自身写入的
            #    匿名会话 Cookie，如 chatglm_token guest）。device_id 在滑块通过
            #    后的发码步再解析（同一会话内取值）。
            session_established = False
            while time.monotonic() < session.deadline and not done.is_set():
                if page.is_closed():
                    callback_failure = "browser_closed"
                    break
                if not session_established and await self._session_cookies(context):
                    session_established = True
                    log("GLM 匿名会话已建立（Cookie 键数不落日志）")
                if session_established:
                    break
                await asyncio.sleep(0.25)
            if callback_failure and not done.is_set():
                _fail(f"GLM 页面会话未建立：{callback_failure}")
                return
            if not session_established:
                _fail("GLM 匿名会话未建立（无站点 Cookie）；未猜测或伪造结果")
                return

            # 3) 同一浏览器上下文注入数美滑块（同源 SDK，页面自带会话，不伪造
            #    挑战值）。注入脚本挂载固定容器，等人工拖动。
            mount_js = (
                GLM_MOUNT_JS
                .replace("%SDK_URL%", json.dumps(GLM_CAPTCHA_SDK_URL))
                .replace("%ORGANIZATION%", json.dumps(GLM_CAPTCHA_ORGANIZATION))
                # 渲染轮询上限：生产 20s（2026-09-23 探针实证真实渲染 ~3s，20s 留足
                # 慢网余量；自检渲染失败用例注入更短阈值保持快速）。
                .replace("%RENDER_TIMEOUT_MS%", "20000")
            )
            mount_state = await page.evaluate(mount_js, "glm-helper-host")
            if not isinstance(mount_state, dict) or not mount_state.get("mounted"):
                _fail("GLM 滑块组件挂载失败；未猜测或伪造结果")
                return
            while time.monotonic() < session.deadline and not done.is_set():
                if page.is_closed():
                    callback_failure = "browser_closed"
                    break
                await asyncio.sleep(0.25)
            if not (done.is_set() and "rid" in callback_result):
                _fail(
                    f"GLM 滑块未完成：{callback_failure or '超时'}；"
                    "官方 SDK 回调缺少 rid，未猜测或伪造结果"
                )
                return
            session.result = dict(callback_result)
            session.send_context = {"cookies": await self._session_cookies(context)}
            log("GLM 滑块通过，同会话发码开始")

            # 4) 同源同出口发码：page.evaluate 在 chatglm.cn 页面内 fetch
            #    /chatglm/user-api/user/login_captcha（body 六键，与 2026-09-22
            #    抓包对齐）。签名三件套优先用后端 sdk-start 下发的 session.sign
            #    （webZhipuComputeSign 生成）；未下发时按同一取证算法本地生成
            #    （移植实现，非第二套算法）。device_id 取自同一匿名会话 JWT。
            #    phone_code 归一口径与后端 zhipuNormalizePhoneCode 同构（"86" →
            #    "+86"；抓包 body.phone_code 带加号）。
            sign = session.sign or _zhipu_sign_triplet()
            send = await page.evaluate(
                GLM_SEND_SMS_JS,
                {
                    "path": GLM_SEND_SMS_PATH,
                    "body": {
                        "phone": session.phone,
                        "phone_code": session.phone_code if session.phone_code.startswith("+") else "+" + session.phone_code,
                        "pic_captcha_id": callback_result["rid"],
                        "tm": "pc",
                        "fr": "default",
                        "distinct_id": "",
                    },
                    "sign": sign,
                },
            )
            if not isinstance(send, dict):
                _fail("GLM 同会话发码未获得可判定结果（evaluate 返回形状异常）")
                return
            self._glm_send_result(session, send)
            send_status = send.get("status")
            body_status = send.get("bodyStatus")
            http_ok = isinstance(send_status, int) and 200 <= send_status < 300
            body_ok = body_status == 0
            # 日志只记状态码与键名，绝不记 Cookie/JWT/rid/device_id 值。
            log(
                "GLM 同会话发码完成 "
                f"send_status={send_status} send_body_status={body_status} "
                f"message_present={bool(send.get('message'))} "
                f"has_cookies={bool(session.send_context.get('cookies'))} "
                f"device_id_present={bool(send.get('device_id'))}"
            )
            if not (http_ok and body_ok):
                # 失败关闭：非 2xx 或 body status 非 0 → context_gap（结果仍回传
                # 诊断键，由后端最小脱敏诊断消费，不造成功）。
                _fail(
                    f"GLM 同会话发码失败（send_status={send_status} "
                    f"send_body_status={body_status}）；按失败关闭协议返回"
                )
                return
            session.status = "ok"
            session.message = "人工挑战完成（同会话发码成功）"
        except asyncio.CancelledError:
            _fail("人工挑战已过期")
        except Exception as exc:
            _fail(f"人工挑战未完成：{type(exc).__name__}")
            log(f"人工挑战异常 platform=glm：{type(exc).__name__}")
        finally:
            if browser:
                try:
                    await browser.close()
                except Exception:
                    pass

    async def _run(self, session: ChallengeSession) -> None:
        if session.platform == "glm":
            await self._run_glm(session)
            return
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
            # 只记键名不记值：实锤易盾 onSuccess 载荷字段集。
            log(f"SDK 挑战回调字段 platform=kimi keys={sorted(payload.keys())}")
            values = {
                field_name: str(payload.get(field_name) or "").strip()
                for field_name in ("validate",)
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

            # kimi：用真实源承载内联页面（路由拦截注入 HTML，不实际访问远端）：
            # 不透明源下易盾 SDK 读 document.cookie 会被拒绝，导致组件初始化失败。
            # kimi 无站点会话依赖（GLM 已改走 _run_glm 真实 origin 路径）。
            await page.route(CHALLENGE_PAGE_URL, _serve_challenge)
            await page.goto(CHALLENGE_PAGE_URL, wait_until="domcontentloaded", timeout=30_000)
            session.status = "running"
            log(f"已打开 SDK 人工挑战 platform=kimi deadline={max(0, int(session.deadline - time.monotonic()))}s")
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
            # kimi 仅需 validate（E0 取证：loginWithSMS 请求体不带 captcha）。
            if done.is_set() and "validate" in callback_result:
                session.result = dict(callback_result)
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
                # GLM 发码失败的诊断键（send_status/send_body_status/send_message）
                # 已在 _glm_send_result 写入 session.result：失败关闭时仍随
                # context_gap 载荷带出，供后端最小脱敏诊断；成功凭证键（rid 等）
                # 一并带出但后端只按 status==ok 消费。不含 Cookie/JWT 值以外的
                # 敏感新增——cookies/device_id 本就在 result 内。
                if session.result:
                    payload["data"] = dict(session.result)
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
        # Lane G 下发的签名三件套在 body 顶层（x_timestamp/x_nonce/x_sign，Go 侧
        # omitempty 省键）；透传原始 dict，由 start() 统一校验规范化。
        sign_body = {
            "x_timestamp": body.get("x_timestamp"),
            "x_nonce": body.get("x_nonce"),
            "x_sign": body.get("x_sign"),
        }
        try:
            session = await challenge_manager.start(
                platform, str(body.get("login_session_id") or ""),
                str(body.get("phone") or ""), str(body.get("phone_code") or ""),
                body.get("timeout"), sign_body,
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
    """kimi 本地挑战页 mock（kimi 流程零改动，行为与既有自检一致）。"""

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

    async def _deliver(self, html: str) -> None:
        if KIMI_CAPTCHA_ID in html and "validate" in self.outcome:
            await self.bindings["challengeComplete"](None, {"validate": self.outcome["validate"]})

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


class _MockGLMSendServer:
    """GLM 发码后端 mock：校验发码请求的端点/头/六键 body（全部假数据），返回
    可编程的状态码与 body。落盘计数仅供自检断言，值不含任何凭据。"""

    def __init__(self, status: int, payload: str):
        self.status = status
        self.payload = payload
        self.requests: List[Dict[str, Any]] = []

    async def handle(self, request: Any) -> web.Response:
        try:
            body = await request.json()
        except Exception:
            body = {}
        headers = getattr(request, "headers", {}) or {}
        self.requests.append({
            "path": request.path,
            "has_sign_headers": bool(headers.get("x-sign")) and bool(headers.get("x-nonce")) and bool(headers.get("x-timestamp")),
            "has_device_id": bool(headers.get("x-device-id")),
            "body_keys": sorted(body.keys()),
            "phone_code": body.get("phone_code", ""),
            "pic_captcha_id": body.get("pic_captcha_id", ""),
        })
        return web.json_response(json.loads(self.payload), status=self.status)


class _MockGLMPage:
    """GLM 同会话链路 mock：
    - context.cookies(origin) 返回假匿名会话 Cookie（假数据，键名对齐取证）；
    - evaluate(mount_js) 记录挂载调用并触发 rid 回调；
    - evaluate(send_js, args) 用 mock JS 假体记录请求载荷并调 mock 后端，
      断言 helper 传入的端点/body/sign 形状。"""

    def __init__(self, outcome: Dict[str, str], send_server: _MockGLMSendServer,
                 cookie_header: str, send_status: Optional[Any] = None,
                 send_body_status: Optional[Any] = None):
        self.outcome = outcome
        self.send_server = send_server
        self.cookie_header = cookie_header
        self.send_status = send_status
        self.send_body_status = send_body_status
        self.bindings: Dict[str, Any] = {}
        self.closed = False
        self.mounted = False
        self.goto_urls: List[str] = []
        self.last_send_args: Dict[str, Any] = {}

    async def expose_binding(self, name: str, callback: Any) -> None:
        self.bindings[name] = callback

    async def goto(self, url: str, **_: Any) -> None:
        self.goto_urls.append(url)

    def is_closed(self) -> bool:
        return self.closed

    async def evaluate(self, script: str, arg: Any = None) -> Any:
        if "glm-helper-host" in script:
            self.mounted = True
            rid = self.outcome.get("rid", "")
            if rid:
                await self.bindings["challengeComplete"](None, {"rid": rid})
            else:
                await self.bindings["challengeFail"](None, "slider_failed")
            return {"mounted": True, "reused": False}
        # 发码 evaluate：script 为 GLM_SEND_SMS_JS，arg = {path, body, sign}。
        self.last_send_args = dict(arg or {})
        req = self.last_send_args
        # mock 假体模拟真实 JS 行为：device_id 取自 outcome（假值），仅当存在时
        # 才视为带 x-device-id 头。
        req = dict(req)
        req["device_id"] = self.outcome.get("device_id", "")
        await self.send_server.handle(_MockSendRequest(req))
        if self.send_status is None:
            return None  # evaluate 返回形状异常路径
        return {
            "status": self.send_status,
            "bodyStatus": self.send_body_status,
            "message": "mock",
            "device_id": self.outcome.get("device_id", ""),
        }


class _MockSendRequest:
    """_MockGLMSendServer.handle 的最小请求假体（aiohttp.Request 鸭子类型子集）。"""

    def __init__(self, args: Dict[str, Any]):
        self.path = str(args.get("path", ""))
        self._body = dict(args.get("body") or {})
        self.headers = _MockHeaders({
            "x-sign": "present" if (args.get("sign") or {}).get("sign") else "",
            "x-nonce": "present" if (args.get("sign") or {}).get("nonce") else "",
            "x-timestamp": "present" if (args.get("sign") or {}).get("timestamp") else "",
            "x-device-id": "present" if args.get("device_id") else "",
        })

    async def json(self) -> Dict[str, Any]:
        return dict(self._body)


class _MockHeaders:
    def __init__(self, mapping: Dict[str, str]):
        self._m = {k.lower(): v for k, v in mapping.items()}

    def get(self, key: str, default: str = "") -> str:
        return self._m.get(str(key).lower(), default)


class _MockGLMContext:
    def __init__(self, page: _MockGLMPage, cookie_header: str):
        self.page = page
        self.cookie_header = cookie_header

    async def cookies(self, origin: str) -> List[Dict[str, Any]]:
        if _host_of(GLM_ORIGIN) not in origin:
            return []
        out = []
        for pair in self.cookie_header.split("; "):
            if not pair:
                continue
            name, _, value = pair.partition("=")
            out.append({"name": name, "value": value})
        return out

    async def new_page(self) -> _MockGLMPage:
        return self.page


class _MockGLMBrowser:
    def __init__(self, page: _MockGLMPage, cookie_header: str):
        self.page = page
        self.cookie_header = cookie_header

    async def new_context(self, **_: Any) -> _MockGLMContext:
        return _MockGLMContext(self.page, self.cookie_header)

    async def close(self) -> None:
        self.page.closed = True


class _MockGLMChromium:
    def __init__(self, page: _MockGLMPage, cookie_header: str):
        self.page = page
        self.cookie_header = cookie_header

    async def launch(self, **_: Any) -> _MockGLMBrowser:
        return _MockGLMBrowser(self.page, self.cookie_header)


class _MockGLMPlaywright:
    def __init__(self, page: _MockGLMPage, cookie_header: str):
        self.chromium = _MockGLMChromium(page, cookie_header)


async def _challenge_selftest(cfg: Dict[str, Any]) -> bool:
    async def finish(manager: ManualChallengeManager, session: ChallengeSession) -> None:
        if session.task:
            await session.task

    # ---- kimi 既有契约（零改动）：回调 / 单槽位 / 绑定 / 一次性消费 ----
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

    # ---- GLM 成功路径：会话建立 → 滑块 → 同会话发码 status==0 ----
    # （全部假数据：假 rid、假 Cookie 值、假 device_id、假手机号，零凭据。）
    def make_glm(outcome: Dict[str, str], send_status: Any, send_body_status: Any,
                 payload: str, cookie_header: str, sign: Optional[Dict[str, str]]):
        server = _MockGLMSendServer(200, payload)
        page = _MockGLMPage(outcome, server, cookie_header,
                            send_status=send_status, send_body_status=send_body_status)
        solver = Solver(cfg)
        solver.pw = _MockGLMPlaywright(page, cookie_header)
        manager = ManualChallengeManager(solver, cfg)
        return manager, page, server

    success_payload = json.dumps({"status": 0, "message": "mock-send-ok", "result": None})
    glm, glm_page, glm_server = make_glm(
        {"rid": "fake-rid", "device_id": "fake-device-id"}, 200, 0, success_payload,
        "chatglm_token=fake.jwt.value; chatglm_test_probe=1",
        {"timestamp": "1789648837735", "nonce": "68c8bc6d488c4869961703710a72eb33",
         "sign": "3a19f494b85cd19deb788467b154e76e"},
    )
    glm_session = await glm.start("glm", "login-glm", "13900000000", "86", timeout=3,
                                  sign={"x_timestamp": "1789648837735",
                                        "x_nonce": "68c8bc6d488c4869961703710a72eb33",
                                        "x_sign": "3a19f494b85cd19deb788467b154e76e"})
    await finish(glm, glm_session)
    glm_result = await glm.result(glm_session.session_id, "glm", "login-glm", "13900000000")
    if not glm_result or glm_result.get("status") != "ok":
        return False
    data = glm_result.get("data") or {}
    # 发码请求契约：端点、六键 body、签名三件套与 device_id 头（形状断言，值全假）。
    if len(glm_server.requests) != 1:
        return False
    req = glm_server.requests[0]
    if req["path"] != GLM_SEND_SMS_PATH or not req["has_sign_headers"] or not req["has_device_id"]:
        return False
    if req["body_keys"] != ["distinct_id", "fr", "phone", "phone_code", "pic_captcha_id", "tm"]:
        return False
    if req["phone_code"] != "+86" or req["pic_captcha_id"] != "fake-rid":
        return False
    if glm_page.last_send_args.get("sign", {}).get("sign") != "3a19f494b85cd19deb788467b154e76e":
        return False  # 下发 sign 必须原样透传，不得本地重算
    # result 回传扩展键：rid / phone_code / send_status / send_body_status /
    # send_message / cookies / device_id。
    if data.get("rid") != "fake-rid" or data.get("phone_code") != "86":
        return False
    if data.get("send_status") != "200" or data.get("send_body_status") != "0":
        return False
    if "send_message" not in data or "send_body_status" not in data:
        return False
    if data.get("cookies") != "chatglm_token=fake.jwt.value; chatglm_test_probe=1":
        return False
    if data.get("device_id") != "fake-device-id":
        return False

    # ---- GLM 发码失败关闭：HTTP 200 但 body.status 非 0 → context_gap ----
    glm2, _, glm2_server = make_glm(
        {"rid": "fake-rid", "device_id": "fake-device-id"}, 200, 500,
        json.dumps({"status": 500, "message": "mock-send-biz-fail"}), "chatglm_token=fake.jwt.value", None,
    )
    glm2_session = await glm2.start("glm", "login-glm2", "13900000001", "86", timeout=3)
    await finish(glm2, glm2_session)
    glm2_result = await glm2.result(glm2_session.session_id, "glm", "login-glm2", "13900000001")
    if not glm2_result or glm2_result.get("status") != "context_gap":
        return False
    d2 = glm2_result.get("data") or {}
    if d2.get("send_status") != "200" or d2.get("send_body_status") != "500":
        return False  # 失败关闭但诊断键仍回传
    if len(glm2_server.requests) != 1:
        return False
    # 未下发 sign → 本地按同一取证算法兜底生成（仍必须带三件套头）。
    if not glm2_server.requests[0]["has_sign_headers"]:
        return False

    # ---- GLM 发码失败关闭：非 2xx → context_gap ----
    server3 = _MockGLMSendServer(400, json.dumps({"status": 400, "message": "mock-bad-request"}))
    page3 = _MockGLMPage({"rid": "fake-rid", "device_id": "fake-device-id"}, server3,
                         "chatglm_token=fake.jwt.value", send_status=400, send_body_status=400)
    glm3_solver = Solver(cfg)
    glm3_solver.pw = _MockGLMPlaywright(page3, "chatglm_token=fake.jwt.value")
    glm3 = ManualChallengeManager(glm3_solver, cfg)
    glm3_session = await glm3.start("glm", "login-glm3", "13900000002", "86", timeout=3)
    await finish(glm3, glm3_session)
    glm3_result = await glm3.result(glm3_session.session_id, "glm", "login-glm3", "13900000002")
    if not glm3_result or glm3_result.get("status") != "context_gap":
        return False
    if (glm3_result.get("data") or {}).get("send_status") != "400":
        return False

    # ---- GLM 滑块失败关闭：无 rid 回调 → context_gap，不发码 ----
    glm4, _, _ = make_glm(
        {"device_id": "fake-device-id"}, 200, 0, success_payload, "chatglm_token=fake.jwt.value", None,
    )
    glm4_session = await glm4.start("glm", "login-glm4", "13900000003", "86", timeout=3)
    await finish(glm4, glm4_session)
    glm4_result = await glm4.result(glm4_session.session_id, "glm", "login-glm4", "13900000003")
    if not glm4_result or glm4_result.get("status") != "context_gap" or "data" in glm4_result:
        return False

    # ---- GLM 签名纯函数黄金用例（与后端 webZhipuSignTimestamp/webZhipuSignFrom 同一算法）----
    if _zhipu_sign_timestamp(1789648837735) != "1789648837735":
        return False
    if _zhipu_sign_timestamp(1700000000001) != "1700000000091":
        return False
    if _zhipu_sign_from("1789648837735", "68c8bc6d488c4869961703710a72eb33") != "3a19f494b85cd19deb788467b154e76e":
        return False

    # ---- 过期会话：context_gap ----
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


# GLM_MOUNT_JS 真实浏览器自检用例的 mock 数美 SDK（全部假数据，路由本地下发，
# 零外网依赖）：initSMCaptcha 在 window.__mockRenderDelayMs 后向 appendTo 容器
# 渲染 .shumei_captcha 节点（window.__mockRenderNode === false 时永不渲染且不
# 交付实例，模拟"SDK 加载慢/渲染卡死"）；节点渲染后交付实例并在 300ms 后回调
# onSuccess({rid, pass:true})——模拟人工拖动通过。
_GLM_MOCK_SMCP_JS = """
window.initSMCaptcha = (options, instanceCallback) => {
  setTimeout(() => {
    const host = document.querySelector((options && options.appendTo) || '#glm-captcha');
    if (!host || window.__mockRenderNode === false) return;
    const node = document.createElement('div');
    node.className = 'shumei_captcha';
    host.appendChild(node);
    instanceCallback({
      onSuccess: (fn) => { setTimeout(() => fn({ rid: 'fake-browser-rid', pass: true }), 300); },
      onError: (fn) => { window.__mockOnError = fn; }
    });
  }, window.__mockRenderDelayMs || 0);
};
"""


async def _glm_mount_browser_selftest(pw: Any) -> bool:
    """GLM_MOUNT_JS 真实浏览器用例（headless + 全本地路由，零外网依赖）。

    - 延迟渲染回归（2026-09-23 探针实证的真实页行为）：mock SDK 在挂载后 ~4s 才
      渲染滑块节点（真实页 ~3s，取 4s 保证旧固定 3s 兜底必误杀）。断言轮询不再
      误杀、challengeComplete 收到 rid，且真实 GLM_SEND_SMS_JS 在同页发码成功
      （mock 后端 status==0；发码请求带签名三件套与 x-device-id 头、rid 原样透传）。
    - 渲染失败关闭：mock SDK 永不渲染节点，注入短渲染阈值（1500ms）验证按阈值
      失败关闭（生产阈值为 20s，见 _run_glm 的 %RENDER_TIMEOUT_MS% 替换）。
    """
    import base64

    jwt_payload = base64.urlsafe_b64encode(
        json.dumps({"device_id": "fake-device-id"}).encode()
    ).decode().rstrip("=")
    fake_token = "fake-header." + jwt_payload + ".fake-signature"
    api_hits: List[Dict[str, Any]] = []

    async def _route(route: Any) -> None:
        url = str(route.request.url).split("?")[0]
        if url == GLM_CAPTCHA_SDK_URL:
            await route.fulfill(status=200, content_type="application/javascript",
                                body=_GLM_MOCK_SMCP_JS)
            return
        if url == GLM_ORIGIN + GLM_SEND_SMS_PATH:
            headers = route.request.headers
            pic = ""
            try:
                pic = str((json.loads(route.request.post_data or "{}") or {}).get("pic_captcha_id", ""))
            except Exception:
                pass
            api_hits.append({
                "has_sign_headers": bool(headers.get("x-sign")) and bool(headers.get("x-nonce")) and bool(headers.get("x-timestamp")),
                "has_device_id": bool(headers.get("x-device-id")),
                "pic_captcha_id": pic,
            })
            await route.fulfill(status=200, content_type="application/json",
                                body=json.dumps({"status": 0, "message": "mock-send-ok", "result": None}))
            return
        await route.fulfill(status=200, content_type="text/html; charset=utf-8",
                            body="<html><body>glm-mount-selftest</body></html>")

    def _mount_js(render_timeout_ms: str) -> str:
        return (GLM_MOUNT_JS
                .replace("%SDK_URL%", json.dumps(GLM_CAPTCHA_SDK_URL))
                .replace("%ORGANIZATION%", json.dumps(GLM_CAPTCHA_ORGANIZATION))
                .replace("%RENDER_TIMEOUT_MS%", render_timeout_ms))

    browser = None
    try:
        browser = await pw.chromium.launch(
            headless=True, args=["--disable-blink-features=AutomationControlled"])

        # ---- 用例 1：延迟渲染（4s 出节点）不被误杀，取 rid 后同页真实发码成功 ----
        context = await browser.new_context(locale="zh-CN")
        await context.add_cookies([{"name": "chatglm_token", "value": fake_token, "url": GLM_ORIGIN + "/"}])
        await context.route(GLM_ORIGIN + "/**", _route)
        page = await context.new_page()
        done: asyncio.Event = asyncio.Event()
        rid_holder: Dict[str, str] = {}
        fail_holder: Dict[str, str] = {}

        async def complete(_source: Any, payload: Any) -> None:
            if isinstance(payload, dict) and isinstance(payload.get("rid"), str):
                rid_holder["rid"] = payload["rid"]
            done.set()

        async def fail(_source: Any, reason: Any = None) -> None:
            fail_holder["reason"] = str(reason or "")[:80]
            done.set()

        await page.expose_binding("challengeComplete", complete)
        await page.expose_binding("challengeFail", fail)
        await page.add_init_script("window.__mockRenderDelayMs = 4000; window.__mockRenderNode = true;")
        await page.goto(GLM_ORIGIN + "/", wait_until="domcontentloaded", timeout=30_000)
        state = await page.evaluate(_mount_js("20000"), "glm-helper-host")
        if not (isinstance(state, dict) and state.get("mounted")):
            return False
        try:
            await asyncio.wait_for(done.wait(), timeout=12)
        except asyncio.TimeoutError:
            return False
        if fail_holder or rid_holder.get("rid") != "fake-browser-rid":
            return False
        send = await page.evaluate(GLM_SEND_SMS_JS, {
            "path": GLM_SEND_SMS_PATH,
            "body": {"phone": "13900000000", "phone_code": "+86",
                     "pic_captcha_id": rid_holder["rid"], "tm": "pc", "fr": "default",
                     "distinct_id": ""},
            "sign": _zhipu_sign_triplet(),
        })
        if not (isinstance(send, dict) and send.get("status") == 200 and send.get("bodyStatus") == 0
                and send.get("device_id") == "fake-device-id"):
            return False
        if len(api_hits) != 1:
            return False
        if not api_hits[0]["has_sign_headers"] or not api_hits[0]["has_device_id"]:
            return False
        if api_hits[0]["pic_captcha_id"] != "fake-browser-rid":
            return False
        await context.close()

        # ---- 用例 2：节点始终不出现 → 按注入阈值（1500ms，生产为 20s）失败关闭 ----
        context2 = await browser.new_context(locale="zh-CN")
        await context2.route(GLM_ORIGIN + "/**", _route)
        page2 = await context2.new_page()
        done2: asyncio.Event = asyncio.Event()
        rid2: Dict[str, str] = {}
        fail2_holder: Dict[str, str] = {}

        async def complete2(_source: Any, payload: Any) -> None:
            if isinstance(payload, dict) and isinstance(payload.get("rid"), str):
                rid2["rid"] = payload["rid"]
            done2.set()

        async def fail2(_source: Any, reason: Any = None) -> None:
            # 注意：回调函数名 fail2 与结果字典不得同名（函数名会在自身作用域内
            # 遮蔽外层字典），故字典命名 fail2_holder。
            fail2_holder["reason"] = str(reason or "")[:80]
            done2.set()

        await page2.expose_binding("challengeComplete", complete2)
        await page2.expose_binding("challengeFail", fail2)
        await page2.add_init_script("window.__mockRenderDelayMs = 200; window.__mockRenderNode = false;")
        await page2.goto(GLM_ORIGIN + "/", wait_until="domcontentloaded", timeout=30_000)
        state2 = await page2.evaluate(_mount_js("1500"), "glm-helper-host")
        if not (isinstance(state2, dict) and state2.get("mounted")):
            return False
        try:
            await asyncio.wait_for(done2.wait(), timeout=8)
        except asyncio.TimeoutError:
            return False
        if rid2 or not fail2_holder.get("reason", "").startswith("数美组件渲染失败"):
            return False
        await context2.close()
        return True
    except Exception:
        return False
    finally:
        if browser:
            try:
                await browser.close()
            except Exception:
                pass


class _GateStubPage:
    """进一步验证等待门自检桩：按轮次回放页面内容，可模拟人工关页。"""

    def __init__(self, contents: List[str]):
        self._contents = list(contents)
        self.closed = False

    def is_closed(self) -> bool:
        return self.closed

    def close(self) -> None:
        self.closed = True

    async def content(self) -> str:
        if len(self._contents) > 1:
            return self._contents.pop(0)
        return self._contents[0]


async def _further_verification_gate_selftest(solver: "Solver") -> bool:
    """进一步验证等待门自检：标记消失取收口轮换凭证 / 持续标记按 deadline 收口 /
    人工关页即返回，三条分支全部失败关闭断言。桩为 Playwright 真实形态
    （async callable 返回 name/value 列表，外审 R2-P1 回归）。"""
    ok = True

    # 分支 1：标记一轮后消失（人工完成手机号登录）→ 收口前最后读一次 cookie，
    # 收口瞬间轮换的新 x5sec 必须被捕获（外审 R2-P4 回归）
    page = _GateStubPage(["请输入手机号 + 短信验证码登录", "闲鱼首页"])
    snapshots = iter([
        [{"name": "x5sec", "value": "rotated-1"}, {"name": "bx-pp", "value": "p"}],
        [{"name": "x5sec", "value": "rotated-2"}, {"name": "bx-pp", "value": "p2"}],
    ])

    async def cookies_fn_clear() -> list:
        return next(snapshots)

    latest = await solver._wait_further_verification(
        page, cookies_fn_clear, "selftest",
        DEFAULT_FURTHER_VERIFY_MARKERS, time.monotonic(), 20,
    )
    if latest.get("x5sec") != "rotated-2":
        log(f"GATE SELFTEST FAIL：收口瞬间轮换的凭证应被捕获，实际={latest}")
        ok = False

    # 分支 2：标记始终存在 → 按 deadline 收口并统一最后读一次（捕获收口前
    # 最后一秒的轮换），不悬挂、不降级
    page = _GateStubPage(["手机号登录"])
    snapshots = iter([
        [{"name": "x5sec", "value": "kept-1"}],
        [{"name": "x5sec", "value": "kept-1"}],
        [{"name": "x5sec", "value": "kept-final"}],
        [{"name": "x5sec", "value": "kept-final"}],
    ])

    async def cookies_fn_kept() -> list:
        return next(snapshots)

    latest = await solver._wait_further_verification(
        page, cookies_fn_kept, "selftest",
        DEFAULT_FURTHER_VERIFY_MARKERS, time.monotonic(), 2,
    )
    if latest.get("x5sec") != "kept-final":
        log(f"GATE SELFTEST FAIL：deadline 收口应带收口前最后一次读取的凭证，实际={latest}")
        ok = False

    # 分支 3：等待期间人工关闭页面 → 立即收口并统一最后读一次（页面关了但
    # context 仍可读时捕获关页前落盘的凭证）
    page = _GateStubPage(["获取验证码", "获取验证码"])
    snapshots = iter([
        [{"name": "x5sec", "value": "closed-1"}],
        [{"name": "x5sec", "value": "closed-final"}],
    ])

    async def cookies_fn_close() -> list:
        page.close()
        return next(snapshots)

    latest = await solver._wait_further_verification(
        page, cookies_fn_close, "selftest",
        DEFAULT_FURTHER_VERIFY_MARKERS, time.monotonic(), 20,
    )
    if latest.get("x5sec") != "closed-final":
        log(f"GATE SELFTEST FAIL：人工关页收口应带最后一次读取的凭证，实际={latest}")
        ok = False

    # 分支 4：页面内容持续读取失败（跳转窗口）→ 绝不视为"标记消失"提前收口，
    # 必须坚持等到 deadline 才按统一出口返回（外审 R2-P2 回归）
    class _BrokenContentPage:
        def is_closed(self) -> bool:
            return False

        async def content(self) -> str:
            raise RuntimeError("Execution context was destroyed (跳转窗口)")

    read_count = {"n": 0}

    async def cookies_fn_broken() -> list:
        read_count["n"] += 1
        return [{"name": "x5sec", "value": "broken-final"}]

    gate_start = time.monotonic()
    latest = await solver._wait_further_verification(
        _BrokenContentPage(), cookies_fn_broken, "selftest",
        DEFAULT_FURTHER_VERIFY_MARKERS, gate_start, 2,
    )
    waited = time.monotonic() - gate_start
    if waited < 2.0:
        log(f"GATE SELFTEST FAIL：content 读失败不得提前收口（实际仅等待 {waited:.1f}s）")
        ok = False
    if latest.get("x5sec") != "broken-final" or read_count["n"] < 1:
        log(f"GATE SELFTEST FAIL：deadline 统一出口应带最后读取的凭证，实际={latest}")
        ok = False

    if ok:
        log("GATE SELFTEST PASS：进一步验证等待门四分支（标记消失取收口轮换凭证/"
            "持续标记 deadline 收口/人工关页即返回/跳转窗口内容读失败不提前收口）全部符合预期")
    return ok


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
            if not await _further_verification_gate_selftest(solver):
                log("SELFTEST FAIL：进一步验证等待门用例失败")
                return 1
            if not await _glm_mount_browser_selftest(solver.pw):
                log("SELFTEST FAIL：GLM 滑块挂载 JS 轮询用例失败（延迟渲染不被误杀 / 渲染失败关闭）")
                return 1
            log("SELFTEST PASS：kimi SDK 回调、GLM 同会话发码成功/失败关闭（body 非 0、非 2xx、滑块失败）、"
                "GLM 挂载 JS 轮询（4s 延迟渲染不被误杀、注入短阈值的渲染失败关闭）、"
                "进一步验证等待门四分支（标记消失取收口轮换凭证/持续标记 deadline 收口/人工关页取最终快照/内容读失败不提前收口）、"
                "签名黄金用例、单槽位/绑定/过期及 cookie 求解链全部正常")
            return 0
        log(f"SELFTEST FAIL：status={status} cookie_names={sorted(cookies)} url_expired={url_expired}")
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
