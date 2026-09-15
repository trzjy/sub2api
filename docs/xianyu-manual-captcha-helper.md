# 闲鱼风控本地人工打码助手（xianyu-captcha-helper）方案与运维手册

- 状态：**在役方向**（2026-09-16 用户裁定：回归自研 Worker，风控痛点用本地人工打码解决；闲管家路线已放弃，见 xianyu-worker-retirement-plan.md 状态行）。
- 原则：**Worker 侧零改动**。Worker 内置的「远程过滑块」协议天生为慢速人工求解设计（读超时下限 300s），本地端只是实现同一协议的求解器。

## 1. 背景与决策

自研 Worker 的 token 续期/采集链路会被阿里 Baxia 滑块风控拦截；服务端自动化过滑块（xvfb 物理鼠标探针）通过率不稳定（见 .sub2api-acceptance/xianyu-xvfb-probe-20260915：code=0/300 波动）。用户裁定不更换软件，把「过验证」变成人工参与的一步：挑战发生时推到用户本地工作站，真人拖滑块，凭证回传 Worker 继续工作。

## 2. 拓扑

```
Worker 容器(yiyutu-server)                 本地工作站(zjy@GL502VML)
┌─────────────────────────┐   ssh -R    ┌──────────────────────────┐
│ orchestrator 远程策略 ─────▶ 127.0.0.1 ──▶ sshd ──▶ 反向隧道 ──▶ 127.0.0.1:18089 │
│ (remote_solver.py)        │   :18089    │        xianyu-captcha-helper             │
│                           │             │  notify-send 弹窗 → 有头 Chrome 人工滑块  │
│ socat(网桥网关IP:18089)────┘             │  → 返回 x5*/bx* cookie                   │
└─────────────────────────┘               └──────────────────────────┘
```

- sshd `GatewayPorts=no` → 隧道只绑服务器回环；Worker 容器经 docker 网桥网关 IP 访问主机，由服务器上的 socat 中继（只绑内网桥接口，不暴露公网）转到回环端口。
- 服务编排失败语义：远程失败/超时/忙 → 自动回退本机真实鼠标与 Playwright 引擎，**不劣于现状**。

## 3. 协议契约（本地端实现，Worker 侧已有）

### 契约 A：token 续期链路
- Worker 调用方：`common/services/captcha/remote_solver.py` / `orchestrator._call_remote_solve`（编排链：远程→真实鼠标→Playwright→DrissionPage 兜底；挂载于 `websocket/app/services/xianyu/cookie_token_manager.py` 与 `websocket/app/api/routes/internal.py`）。
- 请求：`POST /solve` body `{secret_key, account_id, url(punish 链接), browser_timeout[, cookies, device_id]}`。
- 成功：`{"success": true, "data": {"cookies": {x5*/bx*: ...}}}`；失败：`{"success": false, "message"}`；链接过期：`data.url_expired=true`（Worker 刷新 URL 重试）。
- Worker 读超时 `max(300, browser_timeout+180)` 秒（`captcha/remote_timeout.py`），本地端 deadline 固定 285s 内。

### 契约 B：商品监控
- Worker 调用方：`common/services/monitor_remote_risk_client.py`（总超时 120s）。
- 请求：`POST /risk`，header `X-API-Key: <secret>`，body `{"type":"x5sec_ali","data":{"url":...}}`。
- 响应：`{"success", "message", "data": {"x5sec", "bx-pp", "bx_et"}}`。
- 注意：bx* 家族 cookie 不以 x5 开头，本地端返回集合必须包含 `x5*`+`bx*`（自检已覆盖）。

## 4. 本地端实现与安装（工作站）

- 程序：`deploy-config/xianyu-auto-reply-src/tools/local_captcha_helper.py`（aiohttp + Playwright，单槽位人工求解：请求→notify-send critical 弹窗→有头 Chrome（channel=chrome，系统 148）加载 punish 链接→1s 轮询 cookie→命中即回→停留 8s 关闭；日志不落 cookie 值与完整链接）。
- 配置：`~/.config/xianyu-captcha-helper/config.json`（0600，首次运行自动生成 32 位 secret；`browser_channel: "chrome"`）。
- 服务：`~/.config/systemd/user/xianyu-captcha-helper.service`（DISPLAY=:0）+ `xianyu-captcha-tunnel.service`（`ssh -N -R 127.0.0.1:18089:127.0.0.1:18089 yiyutu-server`，ServerAlive 保活，ExitOnForwardFailure）。
- 自检：`python3 tools/local_captcha_helper.py --selftest`——headless 起本地 Set-Cookie 页面，验证浏览器→轮询→成功契约全链（2026-09-16 PASS）。

## 5. Worker 侧配置（管理界面，一次性）

1. Worker 管理端（8089）→ 验证码/滑块配置页（`GET/PUT /captcha/remote-config`，仅管理员）。
2. 填入：`url = http://<网桥网关IP>:18089/solve`、`secret_key = 本地 config.json 中的 secret`；`block_remote_calls` 改为 `false`（默认 true）。
3. `pass_cookies`（传递 Cookie）建议默认关：人工模式不需要传账号 Cookie，链接过期由 Worker 刷新 URL 重试（`url_expired` 路径）。
4. 用 `POST /captcha/slider-solve/test` 做一次带真实 punish 链接的端到端测试。

## 6. 验收清单

| 项 | 口径 | 状态 |
|---|---|---|
| 本地端自检 | --selftest PASS（成功路径全链） | ✅ 2026-09-16 |
| 隧道连通 | 服务器 `curl 127.0.0.1:18089/healthz` → 200 | ✅ 2026-09-16 |
| 容器连通 | Worker 容器内访问 `http://172.19.0.1:18089/healthz` → 200（ufw 需放行 172.19.0.0/16→18089/tcp，已加） | ✅ 2026-09-16 |
| 契约鉴权负路径 | 错 secret / 缺 X-API-Key → 401（经容器全链实测） | ✅ 2026-09-16 |
| 契约端到端 | Worker `/captcha/slider-solve/test` 或真实续期挑战触发本地弹窗+人工通过+回传成功 | 待验（硬性项） |
| **真实挑战实测** | 真实 Baxia 挑战经人工本地打码后，凭证被 Worker 接受并恢复续期（x5sec IP 绑定风险只能实测排除） | **待验（硬性项，不可用代码推断替代）** |
| 回退不劣化 | 人不在电脑前：本地超时/失败 → Worker 编排回退本机引擎，续期链路行为与现状一致 | 待验（随真实挑战一并观察） |

验收证据：`/home/zjy/.sub2api-acceptance/xianyu-captcha-helper-20260916/`（00-deploy + 01-connectivity + 99-final-report）。

## 7. 风险与说明

- **IP 绑定**：本地（家宽）解出的 x5sec 由服务器使用。该模式是 Worker 协议原生支持的标准玩法，但有效性必须真实挑战实测；若实测失败，回退方案是服务器侧 xvfb 探针继续调优或回到闲管家路线（历史底稿留存）。
- **时效**：人工 10~60s，Worker 端 300s 读超时足够；人不在 → 超时回退，不阻塞不劣化。
- **隐私**：默认不传账号 Cookie；开启 pass_cookies 时 Cookie 经隧道到达本地仅存于内存。日志不落 cookie 值/完整验证链接。
- **网桥网关 IP 动态风险**：docker 网络重建可能改变网关 IP 导致 remote_url 失效；遗留项——后续在 compose 为 `xianyu-internal` 固定子网（需容器重启窗口），触发条件：网关 IP 变更导致打码不通。
- **生产运维留痕**：服务器侧安装（socat + 中继单元）与连通性验证按 evidence-filing-standard 落盘 `.sub2api-acceptance/xianyu-captcha-helper-20260916/`。
