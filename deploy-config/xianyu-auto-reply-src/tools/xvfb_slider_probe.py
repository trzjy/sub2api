#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Xvfb 滑块风控探针
=================

目的
----
实证「用物理光标（pyautogui 经 X11/XTest）回放真人轨迹」在 Xvfb 虚拟显示下，
能否通过闲鱼/阿里 baxia 风控（捕获真实 /slide 返回的 result.code == 0/300），
从而验证 self-hosted 手动打码方案在 Linux/Docker 下的基础设施可行性。

与 real_mouse_slider 的关系
--------------------------
- real_mouse 引擎仅用于 Windows：真实 Chrome（channel="chrome"）+ SendInput（win_input），
  且其顶部直接 import windows_foreground / win_input（Windows 专用），无法在 Linux 导入。
- 本探针在 Linux/Docker 做等价验证：Playwright Chromium（headed）+ Xvfb + pyautogui(X11)
  复刻同一套流程：几何映射 → 物理光标逐点回放 → 捕获 result.code。
- 本探针**只验证输入方法**（X11 合成输入是否被视为「物理」）；真实 Chrome 指纹差异不在覆盖范围内
  （方案文档已将「real Chrome 指纹」标注为待实证点）。结论需结合 CDP=300 的已知事实一起读。

回放忠实度
----------
- 坐标映射复刻 common/services/captcha/real_mouse_coordinates.build_geometry_mapper：
  offset = 窗口 screenX/Y + 边框/标题栏修正，to_screen(vx,vy) = (offset+v)*dpr + correction。
- 校准段复刻 calibrate_slider_center：注入 window.__cal 鼠标监听器，物理移动读取页面收到坐标反推 correction。
- 回放段复刻 real_mouse_slider 的「login 分支」：pyautogui.mouseDown → 逐点 moveTo + 按 dt 睡眠 → mouseUp
  （该分支即用 pyautogui 而非 SendInput，且已知在 Windows 下通过）。

用法
----
  # 基础设施冒烟（不需真实 punish URL，验证 Xvfb/pyautogui/headed 浏览器可用）：
  python tools/xvfb_slider_probe.py --smoke

  # 真实滑块风控验证（推荐从 stdin 注入生产 cookie/device_id，探针直接请求新鲜挑战）：
  printf '<json>' | python tools/xvfb_slider_probe.py --input-stdin

环境变量
--------
  XY_TRAIL              指定真人轨迹文件（默认自动选 business 优选样本）
  XY_DISPLAY            显示编号（默认 :99）
  XY_BROWSER_TIMEOUT    等待结果超时（秒，默认 60）
  XY_START_XVFB         "1" 时若 DISPLAY 未设置则本脚本自动拉起 Xvfb（默认 1）
  XY_HEADLESS           设 "1" 强制无头（仅供调试，正常应 headed）

退出码
------
  0  冒烟通过 / 风控 code==0（通过）
  2  风控 code==300（被拒，关键否定结论）
  3  基础设施/加载失败（无法得出风控结论）
  4  参数缺失
"""
from __future__ import annotations

import argparse
import asyncio
import base64
import glob
import hashlib
import json
import os
import re
import subprocess
import sys
import time
from typing import Dict, List, Optional, Tuple

# Xvfb 以 -ac 关闭访问控制后，Xlib 仍会尝试打开 XAUTHORITY 文件；
# 缺文件会导致 pyautogui(python-xlib) 导入即失败（"/root/.Xauthority: No such file"）。
# 故在导入 pyautogui 之前准备一个可读的空 auth 文件。
_xauth = os.environ.get("XAUTHORITY") or "/tmp/.xvfb-probe-xauthority"
try:
    open(_xauth, "a").close()
except Exception:
    pass
os.environ["XAUTHORITY"] = _xauth

# —— pyautogui 惰性导入：X11/XTest 物理光标。Xvfb 下 DISPLAY 就绪即可用。 ——
try:
    import pyautogui

    pyautogui.PAUSE = 0
    pyautogui.FAILSAFE = False
    PYAUTO_OK = True
except Exception as _e:  # noqa: BLE001
    pyautogui = None  # type: ignore
    PYAUTO_OK = False
    _pyauto_err = _e

from playwright.sync_api import sync_playwright

# —— 与 real_mouse_slider 一致的常量/注入脚本 ——
_STEALTH_MINIMAL = """
try { Object.defineProperty(navigator, 'webdriver', { get: () => undefined, configurable: true }); } catch (e) {}
try { delete Object.getPrototypeOf(navigator).webdriver; } catch (e) {}
try { delete window.__playwright; delete window.__pw_manual; delete window.__PW_inspect; } catch (e) {}
"""

# 注入到每个 frame：捕获鼠标事件，用于「视口坐标 -> 屏幕坐标」校准（与 real_mouse 一致）
_CAP_JS = r"""
(() => {
  if (window.__cal) return;
  window.__cal = [];
  window.__probeEvents = [];
  document.addEventListener('mousemove', e => {
    window.__cal.push([e.clientX, e.clientY, e.screenX, e.screenY, e.timeStamp, e.buttons]);
  }, true);
  for (const type of ['pointerdown', 'mousedown', 'pointermove', 'mousemove', 'pointerup', 'mouseup']) {
    document.addEventListener(type, e => {
      if (window.__probeEvents.length < 2048) {
        window.__probeEvents.push([
          e.type, e.clientX, e.clientY, e.screenX, e.screenY,
          e.timeStamp, e.buttons, e.button, e.isTrusted
        ]);
      }
    }, true);
  }
})();
"""

_BROWSER_ARGS = [
    "--no-sandbox",
    "--disable-setuid-sandbox",
    "--disable-dev-shm-usage",
    "--disable-blink-features=AutomationControlled",
    "--disable-infobars",
    "--no-first-run",
    "--no-default-browser-check",
    "--disable-popup-blocking",
    "--force-color-profile=srgb",
    "--lang=zh-CN",
    "--start-maximized",
]

_TRAILS_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "common", "services", "captcha", "human_trails")
_PREFERRED_BUSINESS_TRAIL = "human_trail_pass_1783943859.json"
_LEGACY_BUSINESS_CAPTURE_DISTANCE_PX = 258.0


def log(*a):
    print("[xvfb-probe]", *a, flush=True)


def _request_fresh_challenge(cookies_str: str, device_id: str) -> Tuple[Optional[str], str]:
    """Directly request one fresh token challenge without touching DB/cache state."""
    from common.services.captcha.token_response import (
        extract_token_captcha_url,
        is_token_expired_response,
    )
    from common.services.im_token_api import _merge_cookie_string, request_im_token

    async def _request_once(current_cookies: str):
        return await request_im_token(
            current_cookies,
            device_id,
            api_mode="web",
            timeout_seconds=30,
        )

    result = asyncio.run(_request_once(cookies_str))
    ret_value = result.response_json.get("ret", []) if isinstance(result.response_json, dict) else []
    if isinstance(ret_value, str):
        ret_value = [ret_value]
    ret_labels = [str(item).split("::", 1)[0] for item in ret_value]
    log(f"Token API: http={result.status_code} ret={ret_labels}")

    if (
        is_token_expired_response(result.response_json)
        and result.response_cookies.get("_m_h5_tk")
    ):
        cookies_str = _merge_cookie_string(cookies_str, result.response_cookies)
        log("Token API 明确返回 _m_h5_tk 过期，已在内存合并 Set-Cookie 并重试一次")
        result = asyncio.run(_request_once(cookies_str))
        ret_value = result.response_json.get("ret", []) if isinstance(result.response_json, dict) else []
        if isinstance(ret_value, str):
            ret_value = [ret_value]
        ret_labels = [str(item).split("::", 1)[0] for item in ret_value]
        log(f"Token API retry: http={result.status_code} ret={ret_labels}")

    challenge_url = extract_token_captcha_url(result.response_json)
    if not challenge_url:
        log("Token API 未返回新的验证链接")
        return None, cookies_str
    return challenge_url, cookies_str


def _element_state(frame) -> Optional[dict]:
    try:
        return frame.evaluate(
            """() => {
              const el = document.querySelector('#nc_1_n1z');
              if (!el) return null;
              const rect = el.getBoundingClientRect();
              const style = getComputedStyle(el);
              return {
                x: Math.round(rect.x), y: Math.round(rect.y),
                width: Math.round(rect.width), height: Math.round(rect.height),
                left: style.left, transform: style.transform,
                className: String(el.className || '')
              };
            }"""
        )
    except Exception:
        return None


def _event_summary(frame) -> dict:
    try:
        events = frame.evaluate("() => window.__probeEvents || []") or []
    except Exception:
        events = []
    counts: Dict[str, int] = {}
    pressed_moves = []
    down = None
    up = None
    for event in events:
        if not isinstance(event, list) or len(event) < 9:
            continue
        event_type = str(event[0])
        counts[event_type] = counts.get(event_type, 0) + 1
        compact = {
            "type": event_type,
            "client": [event[1], event[2]],
            "screen": [event[3], event[4]],
            "buttons": event[6],
            "button": event[7],
            "trusted": bool(event[8]),
        }
        if event_type in ("pointerdown", "mousedown") and down is None:
            down = compact
        if event_type in ("pointerup", "mouseup"):
            up = compact
        if event_type in ("pointermove", "mousemove") and event[6] == 1:
            pressed_moves.append(event)
    pressed_range = None
    if pressed_moves:
        xs = [float(event[1]) for event in pressed_moves]
        ys = [float(event[2]) for event in pressed_moves]
        pressed_range = {
            "x": [round(min(xs), 1), round(max(xs), 1)],
            "y": [round(min(ys), 1), round(max(ys), 1)],
        }
    return {
        "total": len(events),
        "counts": counts,
        "down": down,
        "up": up,
        "pressed_move_range": pressed_range,
    }


def ensure_xvfb() -> bool:
    """若 DISPLAY 未设置且允许自动拉起，则启动 Xvfb :99。返回是否已有可用显示。"""
    if os.environ.get("DISPLAY"):
        return True
    if os.environ.get("XY_START_XVFB", "1") != "1":
        return False
    display = os.environ.get("XY_DISPLAY", ":99")
    log(f"DISPLAY 未设置，自动拉起 Xvfb {display} ...")
    proc = subprocess.Popen(
        ["Xvfb", display, "-screen", "0", "1280x800x24", "-ac", "+extension", "XTEST", "-retro"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    # 等待 X socket 就绪
    for _ in range(50):
        if os.path.exists(f"/tmp/.X11-unix/X{display.replace(':', '')}"):
            break
        if proc.poll() is not None:
            log("Xvfb 进程意外退出，stderr 见上")
            return False
        time.sleep(0.1)
    else:
        log("等待 Xvfb socket 超时")
        return False
    os.environ["DISPLAY"] = display
    time.sleep(0.3)
    return True


def extract_drag(trail: List[list]) -> Optional[Tuple[float, float, List[Tuple[float, float, float]]]]:
    """从真人轨迹抽取一条「按下拖动段」。

    返回 (origin_x, origin_y, [(dx, dy, dt_ms), ...])，dx/dy 相对 origin（iframe CSS 坐标），
    dt 相对本段首事件（毫秒）。无有效段返回 None。
    """
    down_idx = None
    ox = oy = ot = None
    for i, e in enumerate(trail):
        if not isinstance(e, list) or len(e) < 5:
            continue
        if e[0] in ("mousedown", "pointerdown") and e[4] == 1:
            ox, oy, ot = float(e[1]), float(e[2]), float(e[3])
            down_idx = i
            break
    if down_idx is None:
        # 退路：没有显式 down，就用第一个 mousemove 作为起点
        for i, e in enumerate(trail):
            if isinstance(e, list) and len(e) >= 5 and e[0] == "mousemove":
                ox, oy, ot = float(e[1]), float(e[2]), float(e[3])
                down_idx = i
                break
    if down_idx is None:
        return None

    seg: List[Tuple[float, float, float]] = []
    prev_t = ot
    for e in trail[down_idx + 1:]:
        if not isinstance(e, list) or len(e) < 5:
            continue
        if e[0] in ("mouseup", "pointerup"):
            # 收尾一点仍计入
            seg.append((float(e[1]) - ox, float(e[2]) - oy, max(0.0, float(e[3]) - prev_t)))
            break
        if e[0] in ("mousemove", "pointermove"):
            # 仅收录按下态（buttons==1）的点；无 buttons 标记时全部收录（容错）
            if e[4] == 1 or len(seg) == 0:
                seg.append((float(e[1]) - ox, float(e[2]) - oy, max(0.0, float(e[3]) - prev_t)))
                prev_t = float(e[3])
    if len(seg) < 5:
        return None
    return ox, oy, seg


def load_trail(scene: str = "business", trail_path: Optional[str] = None) -> Optional[Tuple[str, Tuple[float, float, List]]]:
    if trail_path:
        files = [trail_path]
    else:
        pat = "human_trail_login_*.json" if scene == "login" else "human_trail_pass_*.json"
        files = sorted(glob.glob(os.path.join(_TRAILS_DIR, pat)))
        if scene == "business":
            pref = os.path.join(_TRAILS_DIR, _PREFERRED_BUSINESS_TRAIL)
            if os.path.isfile(pref):
                files = [pref] + [f for f in files if f != pref]
    for f in files:
        try:
            d = json.load(open(f, encoding="utf-8"))
        except Exception as e:
            log(f"加载轨迹失败 {f}: {e}")
            continue
        if d.get("passed") is False:
            continue
        if d.get("slide_code") == 300:
            continue
        drag = extract_drag(d.get("trail", []))
        if drag:
            log(f"选用轨迹: {os.path.basename(f)} (段长={len(drag[2])})")
            return f, drag
    log("未找到可用真人轨迹样本")
    return None


def smoke_test() -> int:
    """基础设施冒烟：Xvfb + pyautogui + headed Chromium 是否可用。"""
    if not ensure_xvfb():
        log("冒烟失败：无可用 DISPLAY 且无法自动拉起 Xvfb")
        return 3
    if not PYAUTO_OK:
        log(f"冒烟失败：pyautogui 不可用: {_pyauto_err}")
        return 3
    screen = pyautogui.size()
    log(f"Xvfb 显示尺寸: {screen.width}x{screen.height}")
    try:
        with sync_playwright() as p:
            ctx = p.chromium.launch_persistent_context(
                "/tmp/xvfb_probe_smoke",
                headless=os.environ.get("XY_HEADLESS") == "1",
                args=_BROWSER_ARGS,
                no_viewport=True,
                locale="zh-CN",
                timezone_id="Asia/Shanghai",
                ignore_https_errors=True,
            )
            ctx.add_init_script(_STEALTH_MINIMAL)
            page = ctx.new_page()
            page.goto("about:blank")
            geo = page.evaluate(
                "() => ({dpr: window.devicePixelRatio, iw: window.innerWidth, ih: window.innerHeight, "
                "sx: window.screenX, sy: window.screenY, ow: window.outerWidth, oh: window.outerHeight})"
            )
            log(f"headed 浏览器几何: {geo}")
            # 物理光标小幅移动，确认 X11 输入可达
            pyautogui.moveTo(int(screen.width / 2), int(screen.height / 2))
            time.sleep(0.1)
            pyautogui.moveRel(5, 5)
            ctx.close()
        log("冒烟通过：Xvfb / pyautogui / headed Chromium 均可用")
        return 0
    except Exception as e:
        log(f"冒烟失败：{e}")
        return 3


def run_real_probe(url: str, cookies_str: str, browser_timeout: int = 60, trail_path: Optional[str] = None) -> int:
    if not ensure_xvfb():
        log("无法获得 DISPLAY")
        return 3
    if not PYAUTO_OK:
        log(f"pyautogui 不可用: {_pyauto_err}")
        return 3

    scene = "login" if "/newlogin/login.do" in url else "business"
    loaded = load_trail(scene, trail_path)
    if not loaded:
        return 3
    _, (_, _, seg) = loaded

    # 解析 cookies
    cookies = []
    base = url.split("?")[0]
    from urllib.parse import urlsplit

    host = urlsplit(url).hostname or ""
    for part in cookies_str.split(";"):
        part = part.strip()
        if not part or "=" not in part:
            continue
        k, v = part.split("=", 1)
        cookies.append({"name": k.strip(), "value": v.strip(), "url": base})

    slide_code: Dict[str, Optional[int]] = {"code": None}
    slide_traffic = {"requests": 0, "responses": 0}
    slide_responses = []
    cdp_slide_request_ids = []

    def _slide_location(value: str) -> Optional[str]:
        try:
            parsed = urlsplit(value)
            if "/slide" not in parsed.path:
                return None
            return f"{parsed.hostname or ''}{parsed.path}"
        except Exception:
            return None

    def _parse_slide_body(body: bytes) -> Optional[int]:
        try:
            text = body.decode("utf-8", errors="replace").strip()
            if not text.startswith("{"):
                start = text.find("{")
                end = text.rfind("}")
                text = text[start:end + 1] if start >= 0 and end > start else ""
            payload = json.loads(text) if text else None
        except Exception as e:
            digest = hashlib.sha256(body).hexdigest()
            log(
                f"/slide 响应解析失败: type={type(e).__name__} "
                f"bytes={len(body)} sha256={digest}"
            )
            return None
        if not isinstance(payload, dict):
            log(f"/slide 响应结构异常: top_type={type(payload).__name__}")
            return None
        result = payload.get("result")
        if not isinstance(result, dict):
            log(f"/slide 响应缺少 result 对象: top_keys={sorted(payload.keys())}")
            return None
        value = result.get("code")
        try:
            return int(value) if value is not None else None
        except (TypeError, ValueError):
            log(f"/slide result.code 类型异常: {type(value).__name__}")
            return None

    def _parse_slide_response(resp) -> Optional[int]:
        try:
            return _parse_slide_body(resp.body())
        except Exception as e:
            log(f"Playwright /slide body 暂不可读: type={type(e).__name__}")
            return None

    def _on_req(req):
        location = _slide_location(req.url)
        if location:
            slide_traffic["requests"] += 1
            log(f"捕获 /slide 请求: method={req.method} location={location}")

    def _on_resp(resp):
        try:
            location = _slide_location(resp.url)
            if location:
                slide_traffic["responses"] += 1
                slide_responses.append(resp)
                content_type = resp.headers.get("content-type", "").split(";", 1)[0]
                log(
                    f"捕获 /slide 响应: status={resp.status} "
                    f"content_type={content_type} location={location}"
                )
                v = _parse_slide_response(resp)
                if v is not None:
                    slide_code["code"] = v
                    log(f"捕获 /slide 返回 result.code = {v}")
        except Exception:
            pass

    def _route_slide(route):
        """Read the real response before a successful slide destroys its iframe."""
        fetched = None
        try:
            fetched = route.fetch()
            body = fetched.body()
            log(
                f"路由捕获 /slide body: status={fetched.status} bytes={len(body)} "
                f"sha256={hashlib.sha256(body).hexdigest()}"
            )
            value = _parse_slide_body(body)
            if value is not None:
                slide_code["code"] = value
                log(f"路由解析 /slide 返回 result.code = {value}")
            route.fulfill(response=fetched)
        except Exception as e:
            log(f"路由读取 /slide body 失败: type={type(e).__name__}")
            try:
                route.fulfill(response=fetched) if fetched is not None else route.continue_()
            except Exception:
                pass

    with sync_playwright() as p:
        ctx = p.chromium.launch_persistent_context(
            "/tmp/xvfb_probe_real",
            headless=os.environ.get("XY_HEADLESS") == "1",
            args=_BROWSER_ARGS,
            no_viewport=True,
            locale="zh-CN",
            timezone_id="Asia/Shanghai",
            ignore_https_errors=True,
            extra_http_headers={"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8"},
        )
        ctx.add_init_script(_STEALTH_MINIMAL)
        ctx.add_init_script(_CAP_JS)
        ctx.route(re.compile(r"/slide(?:\?|$)"), _route_slide)
        ctx.on("request", _on_req)
        ctx.on("response", _on_resp)
        try:
            ctx.add_cookies(cookies)
        except Exception as e:
            log(f"注入 cookies 失败（继续尝试）: {type(e).__name__}")

        page = ctx.new_page()
        cdp = ctx.new_cdp_session(page)
        cdp.send("Network.enable")

        def _on_cdp_response(params):
            response = params.get("response") or {}
            if _slide_location(str(response.get("url") or "")):
                request_id = params.get("requestId")
                if request_id:
                    cdp_slide_request_ids.append(request_id)

        cdp.on("Network.responseReceived", _on_cdp_response)
        url_digest = hashlib.sha256(url.encode("utf-8")).hexdigest()
        log(f"打开 punish 链接: host={host} length={len(url)} sha256={url_digest}")
        try:
            page.goto(url, wait_until="domcontentloaded", timeout=30000)
        except Exception as e:
            log(f"打开链接异常（继续等待滑块）: {type(e).__name__}")

        # 等待滑块 iframe 与按钮
        frame = None
        button = None
        track = None
        for _ in range(60):
            for fr in page.frames:
                try:
                    el = fr.query_selector("#nc_1_n1z")
                    track_el = fr.query_selector("#nc_1_n1t") or fr.query_selector(".nc_scale")
                except Exception:
                    el = None
                    track_el = None
                if el and track_el:
                    frame = fr
                    button = el
                    track = track_el
                    break
            if button:
                break
            time.sleep(0.5)
        if not button:
            log("未找到滑块按钮 #nc_1_n1z（链接可能已失效或非滑块场景）")
            ctx.close()
            return 3

        button_box = button.bounding_box()
        if not button_box:
            log("无法读取当前滑块按钮位置")
            ctx.close()
            return 3
        mx = float(button_box["x"]) + float(button_box["width"]) / 2.0
        my = float(button_box["y"]) + float(button_box["height"]) / 2.0

        try:
            distance_value = frame.evaluate(
                """() => {
                  const button = document.querySelector('#nc_1_n1z');
                  const track = document.querySelector('#nc_1_n1t')
                    || document.querySelector('.nc_scale');
                  if (!button || !track) return null;
                  return track.getBoundingClientRect().width
                    - button.getBoundingClientRect().width;
                }"""
            )
            distance = float(distance_value or 0.0)
        except Exception:
            distance = 0.0
        if distance <= 0:
            track_box = track.bounding_box() if track else None
            if track_box:
                distance = max(0.0, float(track_box["width"]) - float(button_box["width"]))
        if distance <= 0:
            log("无法计算当前滑轨距离")
            ctx.close()
            return 3
        scale = distance / _LEGACY_BUSINESS_CAPTURE_DISTANCE_PX
        replay_seg = [(dx * scale, dy, dt) for dx, dy, dt in seg]
        log(
            f"当前按钮中心=({mx:.0f},{my:.0f})，滑轨距离={distance:.1f}px，"
            f"轨迹X缩放={scale:.3f}"
        )

        # 几何映射（复刻 build_geometry_mapper）
        geo = page.evaluate(
            "() => ({screenX: window.screenX, screenY: window.screenY, "
            "outerWidth: window.outerWidth, outerHeight: window.outerHeight, "
            "innerWidth: window.innerWidth, innerHeight: window.innerHeight, "
            "devicePixelRatio: window.devicePixelRatio})"
        )
        dpr = float(geo.get("devicePixelRatio") or 1.0)
        border = max(0.0, (float(geo["outerWidth"]) - float(geo["innerWidth"])) / 2.0)
        top_chrome = max(0.0, (float(geo["outerHeight"]) - float(geo["innerHeight"])) - border)
        offset_x = float(geo["screenX"]) + border
        offset_y = float(geo["screenY"]) + top_chrome
        corr_x = 0.0
        corr_y = 0.0

        def to_screen(vx: float, vy: float) -> Tuple[int, int]:
            return int(round((offset_x + vx) * dpr + corr_x)), int(round((offset_y + vy) * dpr + corr_y))

        log(f"几何: dpr={dpr} offset=({offset_x:.0f},{offset_y:.0f}) border={border:.0f} top={top_chrome:.0f}")

        # 物理光标校准（复刻 calibrate_slider_center 的 pyautogui 路径）
        local_center_value = button.evaluate(
            """element => {
              const rect = element.getBoundingClientRect();
              return [rect.left + rect.width / 2, rect.top + rect.height / 2];
            }"""
        )
        local_center = (float(local_center_value[0]), float(local_center_value[1]))
        pred = to_screen(mx, my)
        for target in (frame, page):
            try:
                target.evaluate("() => { window.__cal = []; }")
            except Exception:
                pass
        pyautogui.moveTo(pred[0], pred[1])
        time.sleep(0.18)
        try:
            cal = frame.evaluate("() => window.__cal || []") or []
        except Exception:
            cal = []
        target_center = local_center
        source = "iframe"
        if not cal:
            try:
                cal = page.evaluate("() => window.__cal || []") or []
            except Exception:
                cal = []
            target_center = (mx, my)
            source = "page"
        if cal:
            obs = cal[-1]
            # obs = [clientX, clientY, screenX, screenY, ts, buttons]
            err_x = target_center[0] - float(obs[0])
            err_y = target_center[1] - float(obs[1])
            corr_x += err_x * dpr
            corr_y += err_y * dpr
            log(
                f"校准[{source}]: 观测client=({obs[0]:.0f},{obs[1]:.0f}) "
                f"修正=({corr_x:.0f},{corr_y:.0f})"
            )
        else:
            log("校准：页面未收到物理鼠标事件（X11 输入未达窗口，但继续尝试回放）")

        # 回放（复刻 real_mouse 的 login 分支：mouseDown → 逐点 moveTo + dt 睡眠 → mouseUp）
        slide_code["code"] = None
        sx, sy = to_screen(mx, my)
        log(f"起点屏幕坐标: ({sx},{sy})，段点数={len(replay_seg)}")
        before_state = _element_state(frame)
        try:
            frame.evaluate("() => { window.__probeEvents = []; }")
        except Exception:
            pass
        pyautogui.moveTo(sx, sy)
        time.sleep(0.12)
        pyautogui.mouseDown()
        time.sleep(0.1)
        import random

        for i, (dx, dy, dt) in enumerate(replay_seg):
            tx, ty = to_screen(mx + dx, my + dy)
            pyautogui.moveTo(tx, ty)
            if dt >= 3.0:
                time.sleep(max(0.0, (dt / 1000.0) * random.uniform(0.85, 1.15)))
        time.sleep(0.08)
        pyautogui.mouseUp()
        page.wait_for_timeout(200)
        log(f"拖动事件摘要: {_event_summary(frame)}")
        log(f"按钮状态: before={before_state} after={_element_state(frame)}")

        # 等待结果
        start = time.time()
        deadline = min(8.0, max(3.0, browser_timeout - (time.time() - start)))
        waited = 0.0
        while waited < deadline:
            page.wait_for_timeout(500)
            waited += 0.5
            if slide_code["code"] == 0:
                break
            if slide_code["code"] == 300:
                break

        if slide_code["code"] is None:
            for response in slide_responses:
                value = _parse_slide_response(response)
                if value is not None:
                    slide_code["code"] = value
                    log(f"延迟解析 /slide 返回 result.code = {value}")
                    break
        if slide_code["code"] is None:
            for request_id in cdp_slide_request_ids:
                try:
                    captured = cdp.send("Network.getResponseBody", {"requestId": request_id})
                    encoded_body = str(captured.get("body") or "")
                    body = (
                        base64.b64decode(encoded_body)
                        if captured.get("base64Encoded")
                        else encoded_body.encode("utf-8")
                    )
                    log(
                        f"CDP 捕获 /slide body: bytes={len(body)} "
                        f"sha256={hashlib.sha256(body).hexdigest()}"
                    )
                    value = _parse_slide_body(body)
                except Exception as e:
                    log(f"CDP /slide body 暂不可读: type={type(e).__name__}")
                    continue
                if value is not None:
                    slide_code["code"] = value
                    log(f"CDP 解析 /slide 返回 result.code = {value}")
                    break

        code = slide_code["code"]
        if code == 0:
            log("RESULT: result.code == 0 → 通过（物理光标回放未被 baxia 拦截）")
            ctx.close()
            return 0
        if code == 300:
            log("RESULT: result.code == 300 → 被拒（该次滑动未通过 Baxia 校验）")
            ctx.close()
            return 2
        log(
            f"RESULT: 未捕获明确 code（最终={code}，"
            f"/slide requests={slide_traffic['requests']} responses={slide_traffic['responses']}）；"
            "可能链接失效/非滑块/超时"
        )
        ctx.close()
        return 3


def main(argv: Optional[List[str]] = None) -> int:
    ap = argparse.ArgumentParser(description="Xvfb 滑块风控探针")
    ap.add_argument("--smoke", action="store_true", help="仅做基础设施冒烟（不需真实 URL）")
    ap.add_argument(
        "--input-stdin",
        action="store_true",
        help="从 stdin 读取含 cookies 及 url 或 device_id 的 JSON",
    )
    ap.add_argument("--trail", default=None, help="指定真人轨迹文件")
    ap.add_argument("--timeout", type=int, default=int(os.environ.get("XY_BROWSER_TIMEOUT", "60")), help="等待结果超时秒")
    args = ap.parse_args(argv)

    if args.smoke:
        return smoke_test()

    if not args.input_stdin:
        log("真实验证仅接受 --input-stdin，避免凭据进入命令参数或环境变量")
        return 4

    stdin_input = {}
    try:
        stdin_input = json.load(sys.stdin)
    except Exception as e:
        log(f"读取 stdin JSON 失败: {type(e).__name__}")
        return 4
    url = stdin_input.get("url")
    cookies_str = stdin_input.get("cookies")
    device_id = stdin_input.get("device_id")
    if not url and cookies_str and device_id:
        try:
            url, cookies_str = _request_fresh_challenge(cookies_str, str(device_id))
        except Exception as e:
            log(f"Token API 请求新鲜验证链接失败: {type(e).__name__}")
            return 3
    if not url or not cookies_str:
        log("真实验证的 stdin JSON 需要 cookies 及 url 或 device_id")
        return 4
    return run_real_probe(url, cookies_str, args.timeout, args.trail)


if __name__ == "__main__":
    sys.exit(main())
