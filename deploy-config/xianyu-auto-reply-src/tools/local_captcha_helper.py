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
from pathlib import Path
from typing import Any, Dict, Optional

from aiohttp import web
from playwright.async_api import async_playwright

DEFAULT_CONFIG_PATH = Path.home() / ".config" / "xianyu-captcha-helper" / "config.json"
DEFAULT_PORT = 18089
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
                    args=["--window-size=520,700", "--disable-blink-features=AutomationControlled"],
                )
                context = await browser.new_context(locale="zh-CN")
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
    app.router.add_post("/solve", solve)
    app.router.add_post("/risk", risk)
    app.router.add_post("/browser-notify", browser_notify)
    app.router.add_post("/face-notify", face_notify)
    return app


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
        "notify": False, "post_success_keep_secs": 0, "max_wait": 20, "browser_channel": "",
    }
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
            log("SELFTEST PASS：浏览器启动/加载/cookie 轮询命中/成功契约 全部正常")
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
    log(f"xianyu-captcha-helper 监听 {bind}:{port}（/healthz /solve /risk /browser-notify /face-notify）")
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
