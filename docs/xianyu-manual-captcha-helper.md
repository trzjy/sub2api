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

- 程序：`deploy-config/xianyu-auto-reply-src/tools/local_captcha_helper.py`（aiohttp + Playwright，单槽位人工求解：请求→notify-send critical 弹窗→有头 Chrome（channel=chrome，系统 148）加载 punish 链接→1s 轮询 cookie→命中后进"进一步验证等待门"→按契约回传→停留数秒关闭；日志不落 cookie 值与完整链接）。
  - **进一步验证等待门（2026-09-23）**：风控等级上升后，滑块通过时页面可能仍要求手机号登录。命中通过凭证后按页面文案判定（默认标记：手机号登录/手机号登陆/短信验证码/短信登录/请输入手机号/获取验证码/验证手机号；配置键 `further_verify_markers` 可覆盖，`further_verify_wait: false` 可整体停用）：命中→再弹 notify-send 并保持页面等人工完成，完成（标记消失）/人工关页/到达 285s deadline 三者任一即收口，收口时统一最后读一次当前凭证（覆盖收口瞬间的 cookie 轮换），凭证以等待期间最新 x5* 快照回传，绝不在等待后把 ok 降级为 fail；页面内容读取失败（跳转窗口）不视为完成、继续等待。headless 模式不启用。手机号登录页真实文案尚无 DOM 取证，若实际页面用词不在标记表内，改配置即可。
- 配置：`~/.config/xianyu-captcha-helper/config.json`（0600，首次运行自动生成 32 位 secret；`browser_channel: "chrome"`；既有配置文件无 `further_verify_wait` 键时默认启用等待门）。
- 服务：`~/.config/systemd/user/xianyu-captcha-helper.service`（DISPLAY=:0）+ `xianyu-captcha-tunnel.service`（`ssh -N -R 127.0.0.1:18089:127.0.0.1:18089 yiyutu-server`，ServerAlive 保活，ExitOnForwardFailure）。
- 自检：`python3 tools/local_captcha_helper.py --selftest`——headless 验证 cookie 求解链 + kimi SDK 回调 + GLM 同会话发码（mock 后端，成功/失败关闭全分支）+ 签名黄金用例 + 进一步验证等待门四分支（2026-09-23 PASS，随等待门落地与外审整改更新）。

## 5. Worker 侧配置（2026-09-16 已由执行会话落库完成）

全局配置存 `xy_system_settings`，管理界面入口为 Worker 管理端（8089）→ 验证码配置页
（`GET/PUT /captcha/remote-config`，仅管理员）。当前库内状态：

1. `captcha.remote_service_url = http://<网桥网关IP>:18089/solve`（已写入 `http://172.19.0.1:18089/solve`）。
2. `captcha.remote_secret_key` = 本地 `config.json` 中的 secret（已写入，SHA256 与工作站逐字节核验一致）。
3. `captcha.remote_pass_cookies = false`（已写入）：人工模式不需要传账号 Cookie，链接过期由 Worker 刷新 URL 重试（`url_expired` 路径）。
4. `captcha.block_remote_calls` 保持默认 `true`：该开关**只门禁 backend-web 的入站过滑块接口**（`POST /captcha/slider-solve` 模式B，captcha.py:136/:540），不影响 Worker 出站调用本地 helper——本拓扑没有入站调用方，保持关闭是更小攻击面的正确状态。（更正：早期版本手册曾写"需改为 false"，系误读，已依代码证据修正。）
5. 配置为**每次调用实时读库**（cookie_token_manager.py:607、flow.py:47），改后无需重启 Worker 容器。
6. 端到端验证：`POST /captcha/slider-solve/test` 带真实 punish 链接（需管理员登录态），或等真实续期挑战自然触发。

### 5b. 契约 B（商品监控）配置——按用户隔离，admin 已启用

商品监控的远程过风控是**个人设置**（`xy_user_settings`），不是全局 captcha 配置：
键 `monitor.remote_risk_url` / `monitor.remote_risk_secret`（个人设置页填写，
校验逻辑 common/services/monitor_remote_risk_config.py）。2026-09-16 已按用户裁定为唯一后台用户
`admin`（user_id=1）写入 url=`http://172.19.0.1:18089/risk` + 与契约 A 相同的 secret
（helper 三路由共用一个 secret）；Worker 容器实测负路径 401、有效鉴权可达 helper。
其他后台用户需分别配置；监控任务真实风控触发待自然发生。

## 5c. GLM/Kimi 本地人工 SDK 挑战入口

helper 复用既有本地有头 Playwright 进程打开官方验证码组件；管理页不加载第三方 SDK，也不允许管理员手填凭证：

- 启动：`python3 deploy-config/xianyu-auto-reply-src/tools/local_captcha_helper.py`（默认监听 `127.0.0.1:18089`；配置项 `challenge_max_wait` 默认 180 秒，最大 300 秒）。
- 健康：`GET /healthz`，无需鉴权，仅返回 `{"ok":true}`。
- 创建：`POST /challenge/sdk-start`，`X-API-Key: <secret>`，body `{"platform":"glm|kimi","login_session_id":"...","phone":"...","phone_code":"86","timeout":180[, "x_timestamp":"...","x_nonce":"...","x_sign":"..."]}`；旧 `/challenge/start` 是同一路由别名。平台、登录会话和手机号共同绑定短期会话，单槽位占用时返回 409。可选顶层键 `x_timestamp/x_nonce/x_sign` 为签名三件套参数（Lane G 的 `webZhipuComputeSign` 生成下发，Go 侧 omitempty 省键；GLM 发码时优先使用，未下发时 helper 按同一取证算法本地生成，非第二套算法）。
- 查询：`GET /challenge/{session_id}/status?platform=...&login_session_id=...&phone=...`，带 `X-API-Key`；绑定不匹配返回 409。
- 一次性取结果：`POST /challenge/{session_id}/result`，带 `X-API-Key`，body 含同一组 `platform/login_session_id/phone`。Kimi 成功只返回 `data={validate}`。GLM 成功返回 `data={rid, phone_code, send_status, send_body_status, send_message, cookies, device_id}`（send_status=上游 HTTP 状态、send_body_status=响应体 status 键值、send_message≤80 字符脱敏、cookies=chatglm 匿名会话 Cookie 串、device_id=匿名会话 chatglm_token JWT payload 的 device_id claim）。终态读取后立即消费，重复读取 404。

### 5c-1. GLM 同会话发码链路（2026-09-22 会话边界收敛裁定）

背景：旧实现把数美 SDK 装在本地空白路由页上取 rid，浏览器从未访问 chatglm.cn，没有匿名会话 Cookie 与 device-id——rid 与后端发码请求是两个客户端上下文，上游对发码请求返回 HTTP 400。按用户裁定收敛为同会话链路：

1. **真实 origin 导航**：helper 浏览器导航到 `https://chatglm.cn/` 官方页面（不再用本地路由注入页承载 GLM；kimi 易盾无站点会话依赖，仍走本地页，零改动）。
2. **匿名会话建立**：等待 chatglm.cn 域下出现站点自身写入的匿名会话 Cookie（如 chatglm_token guest）；未建立即失败关闭。
3. **同一浏览器上下文过滑块**：在官方页面 DOM 注入固定容器（不替换文档、不伪造挑战值），加载站点同源数美 SDK（`chatglm.cn/smcp/smcp.min.js`，`initSMCaptcha(options, instanceCallback)` 官方实例回调 API，2026-09-21 双重实证），人工拖动，仅采信真实 `instance.onSuccess` 回调的 `rid`（md5 正常滑块流不带，不猜测）。
4. **同会话同源发码**：滑块通过后在同一页面 `page.evaluate` 执行同源 fetch `POST /chatglm/user-api/user/login_captcha`（端点与六键 body `{phone, phone_code:"+86", pic_captcha_id, tm:"pc", fr:"default", distinct_id:""}` 均为 2026-09-22 21:40 生产抓包取证）。Cookie 由浏览器自动携带（不手工组 Cookie 头）；`x-device-id` 取自同一匿名会话 chatglm_token JWT payload 的 device_id claim（与后端 `webZhipuDeviceIDFromToken` 同口径）；签名三件套优先用 sdk-start 下发的 `sign` 参数（后端 `webZhipuComputeSign` 生成），未下发时 helper 内按同一算法移植实现本地生成（黄金用例与后端单测同值对齐）；`x-request-id` 每请求随机；Origin/Referer/User-Agent 为浏览器管制头自动取真实页面值。
5. **成功判定与失败关闭**（与后端 `zhipuSendBizSuccess` 同口径）：成功 ⇔ HTTP 2xx 且响应体 status==0（回退 code==0/ret==0/success==true 历史白名单）。页面未建立会话、滑块失败、发码非 2xx、body status 非 0 → 全部 `context_gap`（HTTP 422），结果附 `send_status/send_body_status/send_message` 最小脱敏诊断键，绝不造成功。

### 5c-2. 隐私边界（GLM 同会话链路）

- chatglm 匿名会话 Cookie、device_id、rid 仅存在于挑战会话内存对象，随 result 一次性回传后即丢弃；helper 不落盘、不转发第三方。
- 日志只记键名与状态码（如 `send_status=200 device_id_present=True`），绝不打印 Cookie/JWT/rid/device_id 值；发码 `send_message` 截断 80 字符且只取 message/msg/detail 文案键。
- 手机号不落 helper 日志；`login_session_id` 绑定校验用 `hmac.compare_digest`。
- 登录步（phone_login）是否复用同会话 Cookie/x-device-id 由后端 Lane G 消费 result 内 `cookies/device_id` 实现；登录步是否强制同会话为 context_gap，按活体验收判定。
- 真实发码验收只用另行授权的测试手机号，证据按 `docs/evidence-filing-standard.md` 脱敏落盘。

- SDK 页面（kimi）：不把脚本 URL 当网页导航。helper 用本地路由注入页建立最小有头页面后加载已取证 SDK：Kimi `initNECaptcha` 使用已取证 `captchaId`、`mode:"embed"`、`apiVersion:2`。
- 失败关闭：GLM 滑块回调缺 `rid`、SDK 加载失败、弹窗关闭、超时均返回 HTTP 422 `context_gap`，不得把 `token`、`validate` 或 `pass` 当 rid。kimi 回调缺 `validate` 同样 422。

## 6. 验收清单

| 项 | 口径 | 状态 |
|---|---|---|
| 本地端自检 | --selftest PASS（成功路径全链） | ✅ 2026-09-16 |
| 隧道连通 | 服务器 `curl 127.0.0.1:18089/healthz` → 200 | ✅ 2026-09-16 |
| 容器连通 | Worker 容器内访问 `http://172.19.0.1:18089/healthz` → 200（ufw 需放行 172.19.0.0/16→18089/tcp，已加） | ✅ 2026-09-16 |
| 契约鉴权负路径 | 错 secret / 缺 X-API-Key → 401（经容器全链实测） | ✅ 2026-09-16 |
| Worker 全局配置落库 | url+secret+pass_cookies=false 写入 xy_system_settings，SHA256 核验一致；实时读库无需重启 | ✅ 2026-09-16 |
| 契约端到端 | Worker `/captcha/slider-solve/test` 或真实续期挑战触发本地弹窗+人工通过+回传成功 | ✅ 2026-09-16 模拟端到端 |
| **真实挑战实测** | 真实 Baxia 挑战经人工本地打码后，凭证被 Worker 接受并恢复续期（x5sec IP 绑定风险只能实测排除） | ✅ 2026-09-16 真实挑战通过：`x5sec` 回传后 Worker 判定远程成功、刷新 Token 并重连 WebSocket |
| 回退不劣化 | 人不在电脑前：本地超时/失败 → Worker 编排回退本机引擎，续期链路行为与现状一致 | ✅ 2026-09-16 已实测远程失败后继续进入本机引擎；验证对象是回退入口与失败语义，不要求回退引擎本次通过风控 |
| GLM 同会话发码自检 | --selftest 含 GLM mock 全分支（成功 / body 非 0 关闭 / 非 2xx 关闭 / 滑块失败关闭）+ 签名黄金用例 | ✅ 2026-09-23（Lane H 协议落地；真实发码活体验收待另行授权测试手机号） |

验收证据：`/home/zjy/.sub2api-acceptance/xianyu-captcha-helper-20260916/`（00-deploy + 01-connectivity + 99-final-report）。

## 7. 风险与说明

- **人脸验证**：协议密码登录触发的 `face_qr_url` 已透传主站账号页 UI；本地 `/face-notify` 是桌面提醒，只通知账号与入口，不承担主站二维码展示。服务器浏览器模式当前依赖账号通知渠道，尚未配置通道时本地无法收到。
- **IP 绑定**：本地（家宽）解出的 x5sec 已由服务器真实挑战验证可用。
- **时效**：人工 10~60s，Worker 端 300s 读超时足够；人不在 → 超时回退，不阻塞不劣化。
- **隐私**：默认不传账号 Cookie；开启 pass_cookies 时 Cookie 经隧道到达本地仅存于内存。日志不落 cookie 值/完整验证链接。
- **网桥网关 IP 动态风险**：docker 网络重建可能改变网关 IP 导致 remote_url 失效；遗留项——后续在 compose 为 `xianyu-internal` 固定子网（需容器重启窗口），触发条件：网关 IP 变更导致打码不通。
- **生产运维留痕**：服务器侧安装（socat + 中继单元）与连通性验证按 evidence-filing-standard 落盘 `.sub2api-acceptance/xianyu-captcha-helper-20260916/`。
