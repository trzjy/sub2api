# 首页友链服务端注入方案（爬虫可见真实 `<a>` 标签）

> 2026-09-24 · 状态：已部署生产并通过技术验收（终审 R8 approve；生产实证通过），仅剩评测站重爬判定 · 归口：docs/ 按主题立文档
> 外审消化记录见 §11（方案审 R1-R6 计 11 项发现全采纳；实现终审 R7 block→回修→R8 approve）。

## 1. 背景与问题

站点为纯 SPA（纯客户端渲染）：`frontend/index.html` 只有 `<div id="app"></div>`，
首页 footer 友情链接由 Vue 在浏览器渲染（`frontend/src/views/HomeView.vue:89-94` 紧凑分支、
`497-505` 默认分支），服务端返回的 HTML 源码中不存在真实 `<a>` 标签。

外部评测站 apipingce.top 的"来源链接"校验爬虫不执行 JS，只扫 HTML 源码，导致收录申请
一直报"来源链接暂未识别"。

目标：apipingce 爬虫在我们**首页**（`/`）服务端返回的 HTML 源码里直接看到
`<a href="https://apipingce.top/" target="_blank" rel="noopener noreferrer">AI API中转站评测</a>`
真实链接（真实 DOM 节点，非 JS 配置数据），同时不破坏现有后台友链管理、不影响正常用户。

**语义边界（R1 修正）**：注入范围严格限定首页响应；登录、后台、SPA 子路由、404 等
非首页响应**不得**包含该 hidden nav 块——全站隐藏外链超出"首页 footer"语义，且构成
对搜索引擎的隐性 cloaking 风险。

## 2. 调研结论（2026-09-24 实证）

1. **纯 CSR 确认**：`frontend/index.html` 仅 `<div id="app"></div>` + module script；
   `frontend/src/main.ts` 浏览器端 `app.mount('#app')`。无 SSR/预渲染。
2. **后端已有服务端注入链**：`backend/internal/web/embed_on.go`（build tag `embed`）的
   `FrontendServer` 服务 index.html，`injectSettings`（`embed_on.go:204`）已注入：
   `__APP_CONFIG__` 脚本（`</head>` 前）、站点标题、favicon。带 `HTMLCache` 缓存，
   设置更新回调（`server/router.go:86-89` `SetOnUpdateCallback` → `InvalidateCache`）自动失效重建。
3. **缓存与路由的关键事实（R1 确认）**：`serveIndexHTML`（`embed_on.go:145-166`）对
   **所有**无静态文件的 SPA 路由（`/`、`/dashboard`、`/users/123`、404 兜底……）返回
   **同一份**缓存 HTML（`HTMLCache` 单条目）。因此注入若进入该共享产物，会扩散到全站。
4. **数据已在注入 payload 中**：`SettingService.GetPublicSettingsForInjection`
   （`service/setting_public.go:707`）已含 `FooterLinks`（`safeRawJSONArray` 透传，
   JSON 数组 `[{"id","name","url","sort_order"}]`）。
5. **后台保存已强校验（SSOT 确认）**：`handler/admin/setting_handler_update.go:1408-1450`
   校验 name 非空/≤50 字符、URL 非空/≤2048 字符且 `config.ValidateAbsoluteHTTPURL`
   （绝对 http/https、拒绝 fragment），上限 20 条、id 自动生成且唯一。
   注意：该路径校验 `strings.TrimSpace(link.URL)` 但**未回写** trim 后的值——
   库中 URL 可能带首尾空白，注入侧必须自行 trim。
6. **前端排序口径**：`HomeView.vue:569-571` `sortedFooterLinks` = `sort_order` 升序。
7. **生产部署走 embed 构建**：`deploy/Dockerfile:76` `-tags embed` 构建，
   `deploy/Caddyfile:34` `reverse_proxy localhost:8080`——生产首页由 Go 进程伺服，
   注入点生效。

## 3. 方案选型

- **方案 A（index.html 写死链接）**：否。写死不随后台配置变化，形成第二事实源，
  违反合同 §2 方案唯一（后台 footer_links 已是友链 SSOT）。
- **方案 B（服务端渲染注入，首页变体）**：✅ 采用。复用既有注入链渲染 footer_links
  为真实 `<a>`；`HTMLCache` 扩展为双变体（基础变体 + 首页变体），首页变体仅在首页
  路由伺服。既有机制（缓存、失效回调、payload）全部复用，唯一必要扩展是缓存条目
  从一份变体变为两份——既有单变体缓存无法表达"仅首页"语义，该扩展是闭合缺口
  的最小机制（合同 §5）。
- **方案 C（独立中间件/模板注入）**：否。与 B 达成同一结果但新增平行注入路径，
  违反合同 §5（引入新机制前必须先证明既有机制无法闭合——既有机制完全能闭合）。
- **否决的子方案（R1 消化）**：每请求解析 settings/注入——`HTMLCache` 不缓存
  settingsJSON，每首页请求需重新 `GetPublicSettingsForInjection`（设置获取回主链路）
  且可能与缓存产物不一致；违反"缓存一次性渲染"的既有设计。按 sort_order 变体
  粒度再拆缓存键——只有首页/非首页两种语义，两份变体已闭合，不引入通用变体框架。

## 4. 实现设计

改动仅三个文件（`embed_on.go` + `html_cache.go` + `embed_test.go`，共享签名，
单执行单元），前端零改动。

### 4.1 `backend/internal/web/html_cache.go`（双变体缓存）

`HTMLCache` 新增 `homeHTML []byte`、`homeETag string` 两个字段，并补缓存提交代校验
（R2 修正，闭合失效-写回竞态）：

- `Set(html, homeHTML, settingsJSON []byte)` 改为 `SetIfCurrent(html, homeHTML,
  settingsJSON []byte, wantVersion uint64) bool`：持锁校验 `c.settingsVersion ==
  wantVersion` 才写入两份变体与两 ETag，否则**不写任何内容**并返回 false；
  `wantVersion` 由调用方在**读取设置之前**经新增的 `Version() uint64` 捕获。
  这闭合既有 miss 路径的竞态：请求 A 捕获 v0 → 读取旧 settings → 管理员更新触发
  `Invalidate()`（version→v1）→ 请求 A `SetIfCurrent` 失败，旧快照不再写回缓存
  （该请求以自身渲染结果响应一次，不污染缓存；下一请求从新设置重建）。
  原 `Set` 删除，不留平行写入口（合同 §4 旧链归零）。
- `GetHome() *CachedHTML`：返回首页变体（`homeHTML` 为 nil 时返回 nil，与 `Get`
  的 stale 语义一致）。
- `Invalidate()`：同时清空两份变体（原子失效，无新增失效点）。
- `generateETag(settingsJSON []byte, variant string)`：现有实现加 variant 后缀参数。

不引入通用"变体键"框架——语义上只有首页/非首页两种（§3 否决项）。

### 4.2 `backend/internal/web/embed_on.go`

1. `serveIndexHTML` 判定首页：`isHome := c.Request.URL.Path == "/"`
   （R3 修正：仅原始根路径。事实口径——前端路由 `/home` 渲染 HomeView、
   `/` 仅重定向到 `/home`（`router/index.ts:34-36,191-192`）、
   `/index.html` 与其余任意路径命中 `NotFound`（`router/index.ts:815-817`）；
   若把 `/index.html` 归入首页变体，该 URL 在前端实际渲染 404 页，等于向
   NotFound 注入 hidden nav，违反 §1 语义边界。`/home` 用户在 Vue footer
   已可见地看到同一批链接，无需服务端变体；外站爬虫只抓 `/`）。
   缓存命中按 `isHome` 取 `GetHome()`/`Get()`；
   缓存未命中时先 `version := s.cache.Version()` 捕获代，再取设置；
   `rendered := s.injectSettings(settingsJSON)` 后计算
   `homeRendered := injectFooterLinks(rendered, settingsJSON)`，
   `cache.SetIfCurrent(rendered, homeRendered, settingsJSON, version)`；
   按 `isHome` 取变体伺服；若 `SetIfCurrent` 失败（取设置期间发生失效）取到 nil
   变体，则直接以本次渲染结果响应、**不写缓存**（复用既有 `cached == nil` 不设
   ETag 分支），下一请求从新设置重建（R2 修正，见 §4.1 与 §11 R2）。
2. `injectSettings` **保持原样**（产出非首页变体；title/favicon 注入对两变体同效，
   首页变体由其派生）。
3. 新增 `injectFooterLinks(html, settingsJSON []byte) []byte`（R4 修正：**只解析、
   稳定排序、HTML 转义，零过滤**——与 Vue 消费端同一语义）：
   - 从 settingsJSON 解析 `footer_links`（匿名 struct：`name`/`url`/`sort_order`；
     `id` 服务端无需输出）；解析失败或为空数组 → 原样返回（镜像 `ParseFooterLinks`
     坏 JSON 静默→空的既有语义与 Vue no-op 渲染）。
   - 按 `sort_order` 升序稳定排序（`sort.SliceStable`），镜像前端
     `sortedFooterLinks` 口径。
   - **不做任何条目过滤**（R4 修正）：不校验 URL scheme/fragment、不 trim、不跳过
     空名——既有唯一权威 `docs/footer-links-plan.md` 终审已明确拒绝公开读侧重校验
     （"公开下发前未重校验持久化值"属被拒绝语义，镜像 custom_endpoints 公开契约；
     HomeView `sortedFooterLinks` 注释明言 "No filtering/purification/visibility
     rules"），SSR 过滤会与 Vue 全量渲染形成同一 SSOT 的平行语义。历史/坏数据
     条目 SSR 与 Vue 渲染行为完全一致；若需改变损坏值语义，须先获用户裁定并
     同步所有消费端（超出本任务范围）。
   - 逐条渲染 `<a href="ESCAPED_URL" target="_blank" rel="noopener noreferrer">ESCAPED_NAME</a>`
     （`htmlpkg.EscapeString`，与 Vue 模板 `:href`/`{{ }}` 输出同一转义语义）。
   - 数组非空即注入 `<nav id="footer-links" hidden>` 包裹全部 `<a>`；注入首个
     `</body>` 前（`bytes.Replace`——settingsJSON 由 Go `json.Marshal` 产生，
     HTML 默认转义，不可能含字面 `</body>`，与既有 `</head>` 注入同一安全前提）。
   - 输出形如：
     ```html
     <nav id="footer-links" hidden><a href="https://apipingce.top/" target="_blank" rel="noopener noreferrer">AI API中转站评测</a></nav></body>
     ```
4. ~~设置读取/序列化失败路径失败关闭~~（R5 撤销 R4 处置）：`serveIndexHTML`
   的设置获取/序列化失败回退（200 baseHTML）**保持既有行为完全不变**（全部路径、
   含首页）。权威链：用户任务原文明确要求"不要影响正常用户的浏览体验"（合同 §1
   用户决定 > 抽象规则），503 失败关闭会在设置抖动时阻断根路径正常用户，与该
   边界直接冲突；既有测试 `fallback_on_settings_error`（`embed_test.go:406`）
   已锁定 200 回退行为；`docs/footer-links-plan.md` 终审先例亦以可用性为由拒绝
   公开读侧失败关闭。R4 审查意见属证据，不构成对该可用性语义变更的授权
   （合同 §1：证据永不创建需求）。R4 指出的"爬虫无法区分服务故障与配置缺失"
   登记为观察项（§11 R5），是否改变留待用户裁定。

### 4.3 `hidden` 的取舍（用户体验与可见性）

- 真实用户已在 Vue 渲染 footer 中**可见地**看到同一批链接（人类访客与人工审核均可见）。
- 服务端注入块面向**不执行 JS 的爬虫**，且仅存在于首页响应，用 HTML `hidden`
  属性对用户隐藏：
  - 不产生未挂载闪动（相比注入进 `#app` 内等待 Vue mount 覆盖的方案）；
  - 不用内联 `style="display:none"`，不受 CSP `style-src` 约束；
  - 从可访问性树移除，无障碍用户不受影响；
  - 不与 Vue footer 产生可见重复；不会出现在任何非首页页面。
- 爬虫只扫 HTML 源码找 `<a href>`，`hidden` 不影响源码识别。

### 4.4 `backend/internal/web/embed_test.go`

镜像既有风格新增：

- `TestInjectFooterLinks`：
  - 正常渲染：`<a href="..." target="_blank" rel="noopener noreferrer">name</a>` 且位于 `</body>` 前；
  - 按 sort_order 升序输出；
  - name/URL HTML 转义（`<script>`、`&` 等）；
  - footer_links 缺失/空数组/非法 JSON → 原样返回；
  - 经 `server.injectSettings`（真实 dist baseHTML）的端到端断言。
- **首页/非首页变体测试**（R1/R3 新增）：`serveIndexHTML` 对 `/` 返回含 nav 的首页变体、
  对 `/dashboard` 等**以及 `/index.html`（R3 负向用例：该 URL 前端实际渲染
  NotFound，不得含 nav）**返回不含 nav 的基础变体（用 `httptest` + mock provider）。
- **`HTMLCache` 双变体与代校验测试**（R1/R2 新增）：`SetIfCurrent` 成功写入后
  `Get`/`GetHome` 返回各自内容与 ETag（home 带 `-fl` 后缀）；`Invalidate` 后两者
  均为 nil；**代校验竞态用例**：捕获 v0 → `Invalidate`（v1）→ `SetIfCurrent(..., v0)`
  返回 false 且两变体仍为 nil，`SetIfCurrent(..., v1)` 返回 true 且内容可见；
  **mid-flight 失效端到端用例**：mock provider 取设置过程中触发
  `server.InvalidateCache()`，断言该请求正常响应但缓存保持空（`Get`/`GetHome`
  均为 nil），旧快照未复活。
- **零过滤渲染测试**（R4 修正，取代 R1 的过滤用例）：URL 含 `javascript:`、
  fragment、首尾空白、name 为空的条目**照常渲染**且 name/URL 均被 HTML 转义
  （与 Vue 全量渲染同一语义；URL/转义断言覆盖属性逃逸向量）。
- **失败关闭测试**（R4 新增，**R5 撤销**）：不再新增——既有
  `fallback_on_settings_error`（`embed_test.go:406`）锁定的 200 回退行为保持
  不变且必须继续通过，本任务对该路径零改动。

### 4.5 边界与不变量

- 缓存：两份变体同一次渲染派生、同一条目存储、同一次 `Invalidate` 清空；
  设置更新回调（`router.go:86-89`）无需任何改动即覆盖两变体。失效-写回竞态由
  `SetIfCurrent` 代校验闭合（R2 修正，§4.1）：取设置期间发生的失效使提交失败，
  旧快照不进缓存、只响应当次请求；代价是竞态丢失的那一次渲染不进缓存（下次
  请求重建），属于正确性优先的有界取舍，非降级兜底。
- ETag：两变体各自 ETag 派生自同一 baseHTMLHash + settingsJSON 哈希，首页变体加
  `-fl` 后缀；footer_links 变化 → settingsJSON 变化 → 两变体 ETag 同时变化，已闭合。
  首页与非首页内容相同（无有效友链）时 ETag 不同但内容相同——ETag 只需在同一 URL
  上时间一致，跨 URL 不要求相等，语义正确。
- CSP：注入块为纯 HTML 链接（无脚本/内联样式），nonce 机制不受影响。
- 非 embed 构建：`embed_off.go` 无此路径，不受影响。

## 5. 安全设计（XSS）

- **零过滤 + 全转义**（R4 修正后的最终口径）：SSR 不对持久化值做任何
  scheme/host/fragment/非空判定（读侧重校验属被既有权威拒绝的语义，
  `docs/footer-links-plan.md` 终审记录），安全性完全由输出转义保证：
  - URL：`htmlpkg.EscapeString` 转义后输出，杜绝属性逃逸（引号/尖括号/`&`）；
    与 Vue `:href="link.url"` 的浏览器侧转义同一暴露面、同一语义。
  - name：`htmlpkg.EscapeString` 转义，杜绝标签注入。
  - 写入口已强校验（`config.ValidateAbsoluteHTTPURL` 等，§2.5）是唯一数据质量
    边界；历史/坏数据条目 SSR 与 Vue 渲染行为一致，若需改变该语义须先获用户
    裁定并同步所有消费端。
- `id` 不输出（前端仅作 `:key`）。

## 6. 影响分析（消费者闭包）

| 消费方 | 判定 |
|---|---|
| `serveIndexHTML` 首页路径（仅 `/`） | 受影响（即本改动目标）：响应新增 hidden nav 块 |
| `serveIndexHTML` 非首页路径（`/index.html`、`/dashboard`、`/users/123`、404 兜底等全部其余路径） | 受影响但语义不变：改取基础变体，内容与改动前**逐字节相同**（`injectSettings` 未动）；新增负向测试锁定（含 `/index.html`，R3） |
| `HTMLCache` / 两变体 ETag / `InvalidateCache` | 受影响（本改动扩展）：双变体同次失效；提交代校验 `SetIfCurrent` 闭合既有失效-写回竞态（R2，惠及整条注入链）；回调机制零改动 |
| `serveIndexHTML` 设置获取失败回退（返回 baseHTML） | 不受影响（R5 撤销 R4 处置）：既有 200 回退保持不变（全部路径含首页），既有测试锁定 |
| legacy `ServeEmbeddedFrontend`（`cmd/server/main.go:108` setup 向导、`router.go:82` 注入服务创建失败回退） | 已证明不受影响：两条路径本就无 `__APP_CONFIG__` 注入，且 setup 阶段无 footer_links 配置语义 |
| 前端 `HomeView.vue` / `appStore.footerLinks` | 不受影响：数据源与渲染逻辑零改动，仅首页 HTML 源码多一个 hidden 块 |
| 后台设置 CRUD / 审计（`setting_handler_audit.go:458`） | 不受影响：设置链零改动 |
| CSP/SecurityHeaders 中间件 | 不受影响：无脚本、无内联样式 |
| 非 embed 构建 / API 路由 | 不受影响 |

## 7. 旧链归零

- 注入部分：纯新增注入步骤，无替换/迁移/停用。
- 缓存部分（R2 修正）：`HTMLCache.Set` 被 `SetIfCurrent` 取代，旧 `Set` 删除，
  不留平行写入口；调用方仅 `embed_on.go` miss 路径一处，替换即归零。

## 8. 验收清单

1. `go build -tags embed ./internal/web/` 通过（backend 目录）。
2. `go test -tags embed ./internal/web/ -count=1` 全部通过（含新增测试，含首页/非首页负向用例）。
3. `go vet -tags embed ./internal/web/` 通过。
4. 单元级证据：`/` 响应产物含
   `<a href="https://apipingce.top/" target="_blank" rel="noopener noreferrer">`；
   `/dashboard` 响应产物不含 `id="footer-links"`。
5. 生产实测（部署后，属活体验收，**完成门禁**；命令 R5 修正为严格判定）：
   a. 管理后台录入 apipingce 链接后：
      `curl -fsS https://<域名>/ | grep -F '<a href="https://apipingce.top/" target="_blank" rel="noopener noreferrer">'`
      （`-f` 使 4xx/5xx 直接失败，`-F` 固定串断言首页命中）；
      `curl -fsS https://<域名>/dashboard | ! grep -q 'id="footer-links"'`
      （零命中必须"成功"，且 4xx/5xx 不再被误判为"无注入"）；
   b. **外站实际识别**：在 apipingce.top 触发来源链接复核，以"来源链接已识别"
   的页面/响应截图或原文作为原始问题（"来源链接暂未识别"）闭合的唯一完成证据
   （R1 采纳：curl 命中只能证明源码含链接，不能证明外站校验器接受 hidden 链接）。
6. 证据落盘：按 `docs/evidence-filing-standard.md` 落盘
   `/home/zjy/.sub2api-acceptance/footer-links-ssr-20260924/`（R1 修正：目录格式
   `<项目标识>-<yyyymmdd>`），最低集 `00-deploy.md` + `99-final-report.md`，
   附加件含首页/`/dashboard` 源码抓取原文与 footer_links 配置变更前后留档。
   本会话环境无生产 DB/域名，第 5 项登记为"需部署环境补验"；**在获得 5.b 证据前，
   本任务对外报告状态为"待外部验收"，不得宣称原始问题已解决**（合同 §7）。

## 9. 预闭环自审（方案阶段，供外审对抗性挑战）

1. **执行遗漏核对**：用户需求三要素（爬虫源码可见真实 `<a>`、不破坏后台友链管理、
   不影响正常用户体验）→ §3/§4.3/§8 逐条对应；验收命令与验证方式已定义。
2. **全局一致性**：无平行第二实现（唯一注入路径，首页变体由同一渲染链派生）；
   R4 修正后 SSR 与 Vue 对同一 SSOT 的渲染语义一致——零过滤、全转义（读侧
   URL 校验已被既有权威拒绝，`config.ValidateAbsoluteHTTPURL` 仅存在于写入口）；
   无第二
   事实源（数据仍以 footer_links 设置为 SSOT）；失效逻辑闭合
   （复用既有 InvalidateCache 回调，双变体同次失效；`SetIfCurrent` 代校验闭合
   取设置-写缓存窗口的旧快照复活竞态，R2 修正）；旧入口（legacy
   `ServeEmbeddedFrontend`、非 embed 构建）不产生孤儿数据；无 DTO 契约变更。
3. **禁叠补丁自检**：本改动是"在既有注入机制上补齐爬虫可见性"，不是为 CSR 问题
   打补丁的正解替代（引入 SSR/预渲染才是机制级变更，对单一爬虫校验需求属过度设计）。
   外审请挑战：`SetIfCurrent` 失败时"响应当次但不缓存"是否构成兜底（不是——
   这是并发竞态下的正确性取舍，非对错误的掩盖）、`</body>` 首次替换的前提
  （json.Marshal HTML 转义）是否成立。

## 10. 派发计划

单一执行单元（`embed_on.go` + `html_cache.go` + `embed_test.go` 共享签名，
文件+测试对，无其他可并行单位——前端零改动，拆"实现/测试"会共享 embed_on.go
接口签名，不满足无共享状态前提）：

- 派发单 1：`embed_on.go`（实现）+ `html_cache.go`（双变体缓存）+ `embed_test.go`（测试）。
- 执行模型：codebuddy hy3（额度耗尽切 `custom-local:deepseek-v4.1-flash`）。
- 验证命令白名单：`go build -tags embed ./internal/web/`、
  `go test -tags embed ./internal/web/ -count=1`、`go vet -tags embed ./internal/web/`。

## 11. 外审消化记录

### R1（2026-09-24，gpt-5.6-sol，working-tree 方案审，结论 block）

| 发现 | 分类 | 裁定 | 依据与回修落点 |
|---|---|---|---|
| 注入进共享缓存 HTML 会扩散到全部 SPA 路由（现有测试覆盖 `/dashboard` 等），超出"首页 footer"语义 | redline_out_of_scope | **采纳** | §1 语义边界、§3 方案 B 修订、§4.1 双变体缓存、§4.4 负向测试、§6 影响表。未照单全收其"/home"建议：前端路由首页即 `/`，外站爬虫只抓 `/`，不扩展（§4.2.1） |
| curl\|grep 只证明源码含链接，不证明 apipingce 校验器接受 hidden 链接，须以外站实际识别为完成证据 | must_fix | **采纳** | §8.5.b 完成门禁、§8.6 "待外部验收"状态纪律（合同 §7） |
| 证据目录 `footer-links-ssr-2026-09-24/` 违反硬性标准 `<yyyymmdd>` | must_fix | **采纳** | §8.6 改 `footer-links-ssr-20260924/`，补 `00-deploy.md`/`99-final-report.md`/配置前后留档 |
| `safeFooterLinkURL` 形成第二套 URL 契约（漏 fragment 校验）；返回原值会保留库中首尾空白 | executor_cleanup | **采纳** | §4.2.4 复用 `config.ValidateAbsoluteHTTPURL` + 返回 trim 值；§4.4 新增 fragment/空白用例。实证支持：admin 写入路径校验 trim 值但未回写（§2.5） |

无拒绝项；四项均为真实边界内问题，无过度设计建议需按合同 §6 拒绝。

### R2（2026-09-24，gpt-5.6-sol，working-tree 方案复审，结论 1×P1 must_fix）

| 发现 | 分类 | 裁定 | 依据与回修落点 |
|---|---|---|---|
| 失效后旧设置快照可竞态写回缓存：请求 A 未命中→读旧设置→管理员更新触发 `Invalidate()`→请求 A 仍 `Set()`，旧快照复活至下次失效；仅渲染/派生/写入持锁不覆盖"读设置→写缓存"窗口 | P1 must_fix | **采纳** | §4.1 `Set`→`SetIfCurrent` 代校验（复用既有 `settingsVersion` 字段作代，零新机制）+ 新增 `Version()`；§4.2.1 miss 路径先捕代再取设置；§4.4 代校验竞态用例 + mid-flight 失效端到端用例；§4.5/§9 同步更新。实证：该竞态在既有代码已存在（现 miss 路径同为 Get→fetch→Set 无代校验），方案原"失效逻辑已闭合"系过度声明，本次一并修复，惠及整条注入链 |

无拒绝项；采纳时未采用其"由缓存层提供原子 get-or-render"的备选表述——`SetIfCurrent`
代校验即其建议"捕获 generation、仅允许未变化者提交"的最小实现，不引入 get-or-render
新接口形态（合同 §5 最小机制）。

### R3（2026-09-24，gpt-5.6-sol，working-tree 方案复审，结论 1×P2 redline_out_of_scope）

| 发现 | 分类 | 裁定 | 依据与回修落点 |
|---|---|---|---|
| `/index.html` 被归入首页变体，但前端路由事实是 `/home` 渲染 HomeView、`/` 仅重定向、`/index.html` 命中 NotFound——该判定会向 404 页注入 hidden nav，违反 §1"404 不得包含"边界；且方案中"/home 不存在"的事实描述有误 | redline_out_of_scope | **采纳** | 实证确认（`router/index.ts:34-36,191-192,815-817`）。§4.2.1 `isHome` 收窄为仅 `/`；纠正 `/home` 事实口径（R1 时误述，本轮回写）；§4.4 新增 `/index.html` 负向用例；§6 首行/次行同步 |

无拒绝项。

### R5（2026-09-24，gpt-5.6-sol，working-tree 方案复审，结论 block：2×P1 + 1×P2）

| 发现 | 分类 | 裁定 | 依据与回修落点 |
|---|---|---|---|
| 503 失败关闭未经权威授权：`/` 从既有 200 baseHTML 回退改 503 直接阻断根路径正常用户，与本方案"不影响正常用户"边界（§1）冲突；既有测试已锁定 200 行为；R4 意见属证据、不能单独授权可用性语义变更 | P1 redline_out_of_scope | **采纳，撤销 R4-F2 处置** | 实证确认（`embed_test.go:406` 锁定 200 回退）。§4.2.4 回退路径恢复既有行为（全部路径含首页）；§4.4 撤销 503 测试；§6 回退行恢复"不受影响"。权威链：用户任务原文"不要影响正常用户的浏览体验"（合同 §1 用户决定）+ 既有测试锁定 + footer-links-plan.md 终审可用性先例。R4 的"爬虫无法区分故障与缺配"降级为观察项留待用户裁定 |
| `docs/footer-links-plan.md` 被多处引用为既有唯一权威但未入版本控制（docs/* 忽略且未跟踪），其他工作树无法复核零过滤与用户裁定依据 | P1 must_fix | **采纳** | `.gitignore` 增加 `!docs/footer-links-plan.md` 白名单（与本案方案文档同批入库） |
| 验收命令不严格：`curl -s` 遇 4xx/5xx 不失败；`grep -c` 零命中时退出码 1（脚本中意外中止）且手工执行会把错误页误判为"无注入" | P2 must_fix | **采纳** | §8.5.a 改 `curl -fsS` + `grep -F` 固定串断言首页命中 + `! grep -q 'id="footer-links"'` 零命中断言（与 footer-links-plan.md 终审采纳过的同类修正一致） |

无拒绝项。R4-F2（失败关闭）的处置由本轮 F1 撤销，R4 该发现降级为观察项：
"设置服务故障期间，首页爬虫获得 200 无友链页，无法与配置缺失区分"——现状行为，
是否改变留待用户裁定。

### R4（2026-09-24，gpt-5.6-sol，working-tree 方案复审，结论 block：1×P1 + 1×P2）

| 发现 | 分类 | 裁定 | 依据与回修落点 |
|---|---|---|---|
| SSR 读侧过滤与既有权威冲突：历史/坏数据条目会被 SSR 跳过而 Vue 消费端全量渲染，同一 SSOT 平行语义；既有唯一权威 `docs/footer-links-plan.md` 终审已明确拒绝"公开下发前重校验持久化值"（镜像 custom_endpoints 公开契约 + 用户裁定） | P1 redline_out_of_scope | **采纳，取代 R1-F4 处置** | 实证确认（footer-links-plan.md 终审记录 + HomeView `sortedFooterLinks` "No filtering" 注释 + `:href="link.url"` 原值渲染）。§4.2.3 改为只解析/稳定排序/转义、零过滤；删除 `safeFooterLinkURL`（R1-F4 的"复用校验器"方案随之被取代——其"第二套 URL 契约"担忧以 SSR 无任何 URL 契约的方式更彻底闭合）；§4.4 过滤用例改为零过滤渲染用例；§5 重写为"零过滤+全转义"口径 |
| 首页设置读取/序列化失败时 200 baseHTML 回退：爬虫把服务故障误判为配置缺失，无法区分，违反合同 §5 失败关闭 | P2 must_fix | **采纳，后被 R5-F1 撤销** | 原处置为首页 503 失败关闭；R5-F1 证实该变更未经权威授权且违反"不影响正常用户"边界（见 R5 记录），已回退为既有 200 回退不变 |

无拒绝项。

### R6（2026-09-24，gpt-5.6-sol，working-tree 方案复审，结论 approve·no blocking findings）

R5 回修后复审通过：未发现 must_fix、越界实现、假实现、平行权威路径或安全边界缺口；
收敛建议（按方案已定义的定向测试、生产实测与证据落盘门禁执行）与 §8 既有门禁一致，
无需回修。方案审定稿冻结，进入派发执行（§10）。companion 脚本对本轮输出标记
"incomplete"系格式识别误报（输出为完整结论段，评审全程 14 分钟、exit 0、非中断）。

### 实现终审 R7（2026-09-25，gpt-5.6-sol，自包含 diff packet，结论 block·1 项 must_fix）

执行完成后的实现终审。companion/corealgos 通道故障（Codex CLI 0.155.1 对该中转自始至终
未声明本地工具目录，上游模型尝试自身服务端工具 `run_officejs` 全被 CLI 以 "unsupported
call" 拒绝——全天 rollout 实证；当日所有"成功"审查实为纯 packet 审查），按用户既有裁定
改道 `api.shaulaapi.com/v1` 直连同模型（记忆 external-review-direct-route）。

| 发现 | 分类 | 处置 | 依据 |
|---|---|---|---|
| `serveIndexHTML` 忽略 `SetIfCurrent` 返回值：commit 失败后无条件回读变体，可能取到并发请求 B 写入的新缓存条目，A 以自身渲染体 + B 的 ETag 响应，body/ETag 错配可致客户端缓存错误正文 | P1 must_fix | **采纳** | 与锁定语义 §5"commit 失败不得附带 ETag"直接冲突。修复：仅 commit 成功时回读变体并整体伺服该条目的 Content+ETag（同源自洽）；失败/回读遇 Invalidate 竞态则伺服本次渲染、不写缓存、无 ETag。新增竞态测试 `TestFrontendServer_CommitLostServesOwnRenderWithoutETag` |

### 实现终审 R8（2026-09-25，gpt-5.6-sol，回修后收敛审，结论 approve·no blocking findings）

R7 回修后复审通过：CAS/ETag 修复验证无误；无越界实现、无语义偏离、无假实现；
residual risk 仅剩生产环境实测（§8.5.b 已登记，需部署环境补验）。证据：
`~/.codex-companion/footer-links-ssr-direct-review/`（payload/response R7+R8）。
