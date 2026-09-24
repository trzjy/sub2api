# 方案：首页友情链接（footer_links）后台可管理

> 2026-09-24 立项。用户裁定背景：导航站/评测站收录普遍要求先挂对方回链（收录前置条件），
> 友链功能是收录流程的运营基础设施；同时把 HomeView 硬编码的评测站外链迁入配置，旧链归零。
> 定位：推广运营配置功能（换链/上链不发版），非 SEO 权重杠杆（互惠友链在搜索引擎已折价，
> 本站 CSR SPA 无索引基础，此处不做过度承诺）。
>
> **v2（2026-09-24 方案审回修）**：外审 5 条发现 5/5 采纳——补 service 层持久化/读回链路锚点、
> HomeView 双 footer 分支全覆盖、id 校验范式（自动生成/字符集/唯一性）、移除 adminSettingsStore
> 平行状态、补迁移门禁。消化留痕见 §7。
>
> **v3（2026-09-24 方案审二轮回修）**：第二轮外审 5 条（3 P1/2 P2）5/5 采纳——纠正 v2 两处
> 锚点标错（setting_handler.go 实为公开 handler；写链是 handler 侧旧值回填+始终持久化，非 service
> 层 nil 跳过）、补齐公开注入三件套（service/dto PublicSettings + PublicSettingsInjectionPayload）、
> admin API 类型写入正文、移除 admin 编辑区测试豁免。见 §7 v2 轮留痕。
>
> **v6（2026-09-24 方案审五轮回修）**：第五轮外审 4 条全采纳：①B2 派发单 L34 仍残留条件提交
> 表述（v5 替换串行宽失配静默未生效，外审逮住工具软肋）——彻底清除；②KeyUsageView 存在同款
> footer（L394"same pattern as HomeView"）——按用户裁定"首页 footer"范围**登记排除**
> （合同 §3 消费者闭包逐一登记）；③部署检查 grep -c 无零值断言——改 `! grep -q` 非零失败
> 断言并分离源码/部署产物两类检查；④服务端生成 ID 回填缺实现规则——补"保存成功后响应回填
> form.footer_links"实现语义。见 §7 v5 轮留痕。
>
> **v5（2026-09-24 方案审四轮回修）**：第四轮外审 8 条（4 P1/4 P2）：7 条采纳（公开构造两段
> 分离——L301 为字符串字段 map 取值、注入构造才用 safeRawJSONArray；请求类型真实归属
> admin/setting_handler_update.go L24；提交语义去矛盾——SettingsView 整组提交无条件携带，
> 指针仅为 API 直调省略保留；前端 computed 仅排序无过滤；B1 白名单覆盖 schema 测试；
> B2 白名单补 i18n/app store/rg 扫描；部署检查具体化）。1 条经**用户裁定**闭合：坏 JSON
> 解析语义=镜像既有（→[]），授权依据见 §1/§7。见 §7 v4 轮留痕。
>
> **v4（2026-09-24 方案审三轮回修）**：第三轮外审 3 P1 + 2 P2：4 条采纳（saveSettings payload
> 逐字段构造点、前端 PublicSettings 类型与 app.ts 默认对象、公开构造映射锚点、旧链清理全库化+
> 部署后检查命令）；1 条部分采纳（并发保存覆盖语义记录为全站设置模型既定行为，拒绝为本设置单独
> 加版本校验的新机制，拒绝依据见 §6/§7）。见 §7 v3 轮留痕。

## 1. 契约（唯一权威，双端对齐）

设置键：`footer_links`（JSON 数组字符串，模式同 `custom_endpoints`/`custom_menu_items`）

```json
[
  { "id": "apipingce", "name": "AI API中转站评测", "url": "https://apipingce.top/", "sort_order": 1 }
]
```

| 字段 | 类型 | 约束 |
|---|---|---|
| id | string | 缺省时服务端自动生成（同 menu `generateMenuItemID`）；显式给出时 ≤32 字符且仅 `a-zA-Z0-9-_`（同 `menuItemIDPattern`）；**数组内唯一**（`seen` map 去重拒绝） |
| name | string | 必填，≤50 字符（锚文本） |
| url | string | 必填，绝对 http(s) URL（`config.ValidateAbsoluteHTTPURL`），≤2048 字符 |
| sort_order | int | 升序展示 |

上限 **20 条**；无 md: 模式、无可见性字段（footer 全员公开）、无 CSP 注入（纯 `<a>` 锚链，非 iframe）、
无 nofollow 开关（方案外字段，默认 dofollow 互惠链语义）。
公开下发：未配置时下 `[]`，前端不渲染友链区（非空才渲染，同浮窗契约风格）。
**坏 JSON 解析语义（2026-09-24 用户裁定）**：解析失败→返回 `[]`，与 custom_menu_items/
custom_endpoints 既有已接受契约一致。授权依据：该值唯一合法写入路径为带校验的 admin API，
坏值仅可能来自手工改库；失败关闭会使单条设置损坏打爆整个公开设置接口（首页全挂），爆炸半径更大。
ParseFooterLinks 的"坏 JSON→空切片"测试即损坏值语义测试。

## 2. 后端改动（Lane B1，文件+测试对）

镜像 custom_endpoints 完整消费者闭包（**v3 修正：写链为 handler 侧旧值回填 + service 层始终持久化，
非 nil 跳过；管理员读回/更新响应在 admin handler，setting_handler.go 是公开 handler**）：

1. `backend/internal/service/domain_constants.go` ~L420：`SettingKeyFooterLinks = "footer_links"`
2. `backend/internal/handler/dto/settings.go`：`FooterLink` struct + `ParseFooterLinks(raw)`
   （坏 JSON→空切片，§1 用户裁定）+ `PublicSettings` 响应 DTO（L364，L403 附近字段区）增
   `FooterLinks`。**PUT 请求绑定类型是 `admin/setting_handler_update.go` L24 的
   `UpdateSettingsRequest`（非 dto 文件）——L172 附近增 `FooterLinks *[]dto.FooterLink
   json:"footer_links"`**（指针：API 直调省略时保留既有；SettingsView 整组提交恒携带）
3. `backend/internal/service/settings_view.go` L14 service 层 `SystemSettings`：增 `FooterLinks` 字段（JSON tag 同契约）
4. `backend/internal/service/setting_parse.go`：L80 默认值 `"[]"` + L375 管理端映射
5. `backend/internal/handler/admin/setting_handler_update.go`：
   - L1363 范式：`footerLinksJSON := previousSettings.FooterLinks`（**旧值回填**，请求未携带时保留既有配置），
     请求携带时校验+序列化后赋值
   - L1643 范式：service `SystemSettings` 结构体赋值（`FooterLinks: footerLinksJSON`，service 层始终持久化）
   - L1269 校验块附近：上限 20/name/url/契约 §1 id 规则（自动生成 + 字符集 + seen 唯一性）
   - L2293 范式：更新响应读回映射 `dto.ParseFooterLinks(updatedSettings.FooterLinks)`
6. `backend/internal/handler/admin/setting_handler.go` L266 范式：管理员 GET 响应映射
   `dto.ParseFooterLinks(...)`
7. `backend/internal/service/setting_update.go` L376 范式：`updates[SettingKeyFooterLinks] = settings.FooterLinks`
8. `backend/internal/service/setting_public.go`（**两段构造分离，v4 审 P1**）：
   公开键注册（L198 附近）；**service `PublicSettings`（字符串字段、源为 settings map）的构造块
   （L301 `return &PublicSettings{...}`）增 `FooterLinks: settings[SettingKeyFooterLinks]`**
   （同相邻 `CustomEndpoints: settings[SettingKeyCustomEndpoints]` 行，此处无 safeRawJSONArray/
   无 `.FooterLinks` 字段访问，照抄会编译错）；**`PublicSettingsInjectionPayload`（L571，
   json.RawMessage 字段）增 `FooterLinks` + 其构造映射（~L702，与 CustomEndpoints 同款同源）增
   `FooterLinks: safeRawJSONArray(settings.FooterLinks)`**（缺失则 `window.__APP_CONFIG__` 不含
   友链，app store 误标已加载、首屏不发 XHR 永不展示）
9. `backend/internal/handler/setting_handler.go` L86（公开 handler）：公开响应解析映射
10. `backend/internal/handler/admin/setting_handler_audit.go` L453：changed 登记
11. `backend/internal/handler/dto/public_settings_injection_schema_test.go`：注入 schema 断言同步
12. 后端定向测试：ParseFooterLinks 往返/空/坏 JSON；handler 校验（超限/空名/坏 URL/**重复 id 拒绝**/
    非法字符 id 拒绝/合法写入）；**部分更新保留语义**（请求省略 footer_links → 既有配置不被清空）；
    **写读回环**（写→管理员 GET 读回含服务端生成 id 一致，范式同 setting_service_platform_quota_test.go）；
    公开注入（公开响应与注入 payload 均含 footer_links）

## 3. 前端改动（Lane B2，文件+测试对）

**v2 修正：HomeView 有两个 footer 渲染分支（L88 compact / L465 默认页），两分支消费同一份排序后的
footer_links；v2 移除 adminSettings.ts 改动点（SettingsView 表单直连 adminAPI.settings，不走该 store）。**

1. `frontend/src/types/index.ts`：`FooterLink { id: string; name: string; url: string; sort_order: number }`；
   **`PublicSettings` 接口（~L248，现仅 custom_menu_items/custom_endpoints）增
   `footer_links: FooterLink[]`**
2. `frontend/src/api/admin/settings.ts`（**v3 写入正文**）：`SystemSettings` 响应类型与
   `UpdateSettingsRequest` 请求类型均增 `footer_links`（SettingsView 的 payload 显式声明为
   `UpdateSettingsRequest`、GET 返回该文件 `SystemSettings`，缺一则保存触发类型错误/读回无契约）
3. `frontend/src/stores/app.ts`：公开设置解析处透出 `footerLinks`（模式同 supportQrcodeUrl）；
   **注入/默认 settings 对象（~L357，现显式列出 `custom_menu_items: []`/`custom_endpoints: []`）增
   `footer_links: []`**——注入与 XHR 两条到达路径行为保持一致
4. `frontend/src/views/HomeView.vue` **两个分支**：
   - L88 compact 分支：**删除硬编码 apipingce.top**，改为渲染 `appStore.footerLinks`（升序、`·` 分隔、
     `target="_blank" rel="noopener noreferrer"`），空数组不渲染友链内容、原 © 行保持
   - L465 默认页分支：在 © 行与 docs/GitHub 链接处**追加同一份**友链渲染（同排序/同 rel 策略），空数组零 DOM 变化
   - 渲染逻辑抽为组件内共用 computed/片段，不新建全局组件层（净增复杂度最小）
5. `frontend/src/views/admin/SettingsView.vue` 通用设置 tab 新增「友情链接」编辑区
   （镜像 L6679 CustomEndpoints 区：行内 名称/URL/排序 + 增删/上移下移；表单直连
   `adminAPI.settings.getSettings()/updateSettings()` 现有 loadSettings/saveSettings 链路，L11192/L11977）
   - **提交语义（v4 审去矛盾）**：SettingsView 整组表单提交，saveSettings payload（~L11671
     逐字段构造区）**无条件携带 `footer_links: form.footer_links`**（未编辑友链时也携带——
     验收测试须断言此点）；后端 `*` 指针仅为 API 直调方省略保留，前端无任何条件提交逻辑
   - **服务端生成 ID 回填规则（v5 审，实现语义非仅测试点）**：saveSettings 成功后，以更新响应
     中的 footer_links（含服务端生成 id）**回写 `form.footer_links`**（或等价地重新
     loadSettings），保证本地新增行获得稳定 id——否则后续删除/排序/再保存会携带空 id
     重复生成，破坏数组项稳定身份
6. i18n `zh/en`：`admin.settings.footerLinks.*`（键名对齐 customEndpoints 区块风格）
7. **消费者闭包登记（v5 审）**：全站 footer 消费者=HomeView 两分支 + **KeyUsageView L394
   （"same pattern as HomeView"）**。**KeyUsageView 按用户裁定范围排除**（裁定=「首页 footer
   友情链接」，导航站互查仅看首页；排除登记于本条，B2 禁区不得触碰该文件，后端公开下发不限——
   字段全量可用，未来若需扩展仅改前端消费点）。
8. 前端定向测试（**v3：无豁免**）：
   - HomeView **两种首页模式** footer spec（compact/默认：非空渲染顺序/rel/锚文本、空数组不渲染、
     原 © 行与 docs/GitHub 不受影响）
   - `SettingsView.spec.ts`（既有，66 处 getSettings/updateSettings 交互先例）定向用例必写：
     增删行/上移下移/读回绑定/保存 payload 含 footer_links（**含未编辑友链时仍携带的断言**）/
     **保存后本地行 id 非空（服务端生成 id 回填生效）**

## 4. 旧链归零与迁移门禁

- 硬编码出站链接：HomeView.vue L89 `apipingce.top` 随本任务删除。
- 清理验证范围（v3 审采纳）：**全库 grep `apipingce`**（backend/frontend/tests/docs/deploy 脚本、
  非仅 frontend/src）；部署后叠加运行时检查（见下方命令）。
- 数据迁移（部署后强制步骤）：生产后台通用设置 → 友情链接，录入
  「AI API中转站评测 / https://apipingce.top/」。
  - **负责人**：主会话（验收环节）；**截止**：部署当日
  - **门禁**：录入 + 首页活体验证证据落盘前，不得宣称任务完成（evidence-filing-standard 硬性）；
    部署后检查命令（可复现断言，输出落证据目录）：
    ```bash
    BASE_URL=https://corealgos.com
    # 1) 公开设置含评测站友链（jq -e：无匹配即非零失败）
    curl -fsS "$BASE_URL/api/v1/settings/public" | jq -e '.data.footer_links[] | select(.url=="https://apipingce.top/")'
    # 2) 部署产物检查：线上 bundle 无硬编码残留（! grep -q：命中即失败，零命中才通过）
    BUNDLE=$(curl -fsS "$BASE_URL" | grep -oE '/assets/index-[^"]+\.js' | head -1)
    ! curl -fsS "$BASE_URL$BUNDLE" | grep -q "apipingce" && echo "bundle 零残留" || { echo "FAIL: bundle 仍含硬编码"; exit 1; }
    # 3) 源码残留检查（与 2 分立：本机仓库运行时代码零命中，docs/.omc/*.md 证据豁免）
    rg -n "apipingce" /mnt/data/sub2api --glob '!docs/**' --glob '!.omc/**' --glob '!*.md' && { echo "FAIL: 源码残留"; exit 1; } || echo "源码零残留"
    ```
  - **回滚**：录入失败/渲染异常 → `.env` 回退上一镜像 tag 重建容器；配置变更前后状态截图留证

## 5. 验收标准（Done when）

1. 后端：五段+service 层读写回环生效（写→GET 读回→公开下发一致），校验块拦截超限/坏 URL/重复 id；定向测试绿
2. 前端：admin 可增删改排并保存；**两种首页模式** footer 均按 sort_order 渲染、空配置零 DOM 变化；定向测试绿 + vue-tsc 零错 + check:i18n 通过
3. 公共契约变更 → 收敛后一轮全量对账（go test ./... + 前端 build）按合同 §5③ 跑一次
4. 生产部署 + 后台录入评测站链接 + 双分支首页活体验证 + 证据落盘（§4 门禁）
5. 自审闭环：平行路径、DTO 前后端字段对齐、i18n 双语完整、旧链 grep 归零

## 6. 风险与边界

- 纯展示层 + 设置链路，不涉资金/状态机/认证；公开 payload 增字段属向后兼容追加
- **并发保存覆盖语义（v3 审 P2，记录为既定行为）**：设置更新为整组表单提交、last-write-wins，
  两管理员并发编辑时后保存者以旧表单覆盖新友链——这是**全站所有设置项（含 custom_menu_items/
  custom_endpoints）的既有统一模型**，非本任务引入的新风险。**拒绝**为本设置单独加并发版本校验：
  合同 §2（单一设置偏离全站模型=引入平行语义）+ §5（新机制须先证明既有机制无法闭合，此处既有
  语义本就闭合、风险等级为低频运营配置）。若未来全站设置需要乐观锁，作独立任务统一评审。
- 公开暴露面：footer_links 仅 name/url，无敏感字段；URL 经绝对 http(s) 校验
- 使用纪律（运营侧，写入 admin 区描述文案）：数量克制（建议 ≤15）、只换同业相关站、
  谨慎链向低质站（footer 全站出链链到被惩站点有反向拖累风险）

## 7. 外审消化留痕（2026-09-24，gpt-5.6-sol 方案审，5/5 采纳零拒绝）

| 发现 | 裁定 | 闭合证据 |
|---|---|---|
| P1 持久化/读回链路缺口 | 采纳 | §2 补 settings_view.go L14 / setting_update.go L376 / setting_handler.go L86 三锚点 + 写读回环测试 |
| P1 HomeView 双 footer 分支（"无第二渲染点"断言错误） | 采纳 | 已核实 L88 compact + L465 默认页两分支；§3 改为双分支同源渲染 + 双模式测试 |
| P2 id 校验缺口 | 采纳 | §1 契约补自动生成/≤32/字符集/seen 唯一性；§2 校验块 + 重复 id 拒绝测试 |
| P2 adminSettingsStore 平行状态（redline） | 采纳 | 已核实 SettingsView 表单直连 adminAPI（L11196/L11977）；§3 移除 adminSettings.ts 改动点 |
| P2 迁移门禁缺失 | 采纳 | §4 补负责人/截止/回滚/门禁（同合同 §4 临时迁移例外登记要求） |

### v2 审第二轮（gpt-5.6-sol，结论 block，5/5 采纳零拒绝）

| 发现 | 裁定 | 闭合证据 |
|---|---|---|
| P1 写链 nil 跳过语义错误（实为 handler 旧值回填 + service 始终持久化） | 采纳 | 已核实 admin/setting_handler_update.go L1363 `customEndpointsJSON := previousSettings.CustomEndpoints` 范式；§2-5 改为旧值回填 + L1643 结构体赋值，删"nil 跳过"表述 |
| P1 管理员 GET/更新响应映射位置标错（setting_handler.go 实为公开 handler） | 采纳 | 已核实 setting_handler.go L39 = GetPublicSettings；§2-6/9 分列 admin/setting_handler.go L266（GET）、L2293（更新响应）、setting_handler.go L86（公开） |
| P1 公开注入三件套缺失（service/dto PublicSettings + PublicSettingsInjectionPayload，缺失则首屏注入不含友链且不再发 XHR） | 采纳 | 已核实 setting_public.go L571 PublicSettingsInjectionPayload；§2-8 补字段+构造赋值；§2-11 补注入 schema 测试（dto/public_settings_injection_schema_test.go）；§2-12 补公开注入测试 |
| P2 api/admin/settings.ts 摘要采纳但正文未列 | 采纳 | §3-2 明确列出 SystemSettings/UpdateSettingsRequest 两处类型（payload 显式类型，缺则编译错/读回无契约） |
| P2 admin 编辑区测试豁免（先例断言错误：SettingsView.spec.ts 既有 66 处交互先例） | 采纳 | 已核实 SettingsView.spec.ts 存在；§3-7 改为必写定向用例，含服务端生成 id 的更新响应回填 |

### v3 审第三轮（gpt-5.6-sol，结论 block，4 采纳 + 1 部分采纳/含拒绝留痕）

| 发现 | 裁定 | 闭合证据 |
|---|---|---|
| P1 公开注入构造映射未点名（L301 构造块 / L702 构造映射） | 采纳 | 已核实 setting_public.go L301 `return &PublicSettings{` 与 L702 附近 safeRawJSONArray 构造行；§2-8 逐点名列出 |
| P1 saveSettings payload 缺 footer_links（~L11671 逐字段构造，漏则永不落盘） | 采纳 | 已核实 SettingsView.vue L11671 区域显式列出 custom_menu_items/custom_endpoints；§3-5 点名 `footer_links: form.footer_links` |
| P1 前端 PublicSettings 类型与 app.ts 默认对象未纳入 | 采纳 | 已核实 types/index.ts ~L248 与 app.ts ~L357 均只列 menu/endpoints；§3-1/§3-3 点名补字段 |
| P2 并发保存覆盖语义未定义 | 部分采纳 + 拒绝版本校验机制 | §6 记录为全站设置模型既定行为（last-write-wins 与 custom_menu_items 等一致）；拒绝单独加版本校验——合同 §2 平行语义 + §5 新机制须先证明既有机制无法闭合 |
| P2 旧链清理验证范围不足（仅 frontend/src）+ 无部署后检查命令 | 采纳 | §4 改全库 grep + 部署后检查命令（公开 API footer_links / 线上 bundle 无硬编码 / 注入链路核验）；§3-7 补注入与 XHR 同值测试 |

### v4 审第四轮（gpt-5.6-sol，结论 block，7 采纳 + 1 用户裁定闭合）

| 发现 | 裁定 | 闭合证据 |
|---|---|---|
| P1 公开构造与注入构造混写（L301 为字符串字段 map 取值，方案写法编译不过） | 采纳 | 已核实 L301 构造块为 `settings[SettingKeyCustomEndpoints]` 直取；§2-8 分离：L301 用 `settings[SettingKeyFooterLinks]`，注入构造（~L702）才用 `safeRawJSONArray` |
| P1 UpdateSettingsRequest 位置错（真实绑定类型在 admin handler L24，非 dto 文件） | 采纳 | 已核实 admin/setting_handler_update.go L24 `type UpdateSettingsRequest struct`、L172-173 指针字段先例；§2-2 改归属，dto 文件仅留 struct/解析器/响应字段 |
| P1 坏 JSON 静默降级违反禁兜底，需明确语义裁定 | **用户裁定闭合**（2026-09-24） | 裁定=镜像既有语义（坏 JSON→[]，同 custom_menu_items/custom_endpoints 已接受契约）；授权依据 §1；ParseFooterLinks 坏 JSON→空切片测试即损坏值语义测试 |
| P1 提交语义自相矛盾（无条件携带 vs 仅修改过才提交） | 采纳 | 已核实 SettingsView L11615 区整组提交、无脏状态机制；§3-5 统一为整组无条件携带，指针语义仅属 API 直调方 |
| P2 前端 computed"排序+过滤"未授权过滤 | 采纳 | §3-4 改"仅复制+排序，无过滤/净化/可见性规则"（契约 §1 全量展示） |
| P2 B1 白名单 -run 'FooterLink' 跑不到 schema 测试 | 采纳 | 已核实 TestPublicSettingsInjectionPayload_SchemaDoesNotDrift（dto 测试 L26）；B1 白名单改 `-run 'FooterLink\|PublicSettingsInjectionPayload_SchemaDoesNotDrift'` |
| P2 B2 白名单缺 i18n/app store/rg 命令 | 采纳 | B2 白名单补 check:i18n、app.spec.ts 定向命令、全库 rg 扫描（豁免 docs/证据） |
| P2 部署检查为占位描述不可复现 | 采纳 | §4 具体化：$BASE_URL + curl -fsS + jq -e 断言 + bundle 下载 grep + rg 扫描，输出落证据目录 |

### v5 审第五轮（gpt-5.6-sol，结论 block，4/4 采纳）

| 发现 | 裁定 | 闭合证据 |
|---|---|---|
| P1 B2 重新引入条件提交语义（v5 替换串行宽失配静默未生效） | 采纳 | 已核实 B2 L34 残留"指针语义：仅修改过才提交（同…"；彻底清除并改写为整组无条件携带；补验收断言"未编辑友链时 payload 仍含该字段" |
| P2 KeyUsageView 同款 footer 消费者未登记（L394 same pattern as HomeView） | 采纳（登记排除） | 已核实；§3-7 登记按用户裁定范围排除（首页 footer；导航站互查仅看首页），B2 禁区禁触该文件；后端下发不限 |
| P2 部署检查 grep -c 命中时反而成功（无零值断言）+ 源码/部署产物混为一类 | 采纳 | §4 改 `! grep -q` 非零失败断言 + 显式分离"源码残留检查"与"部署产物检查" |
| P2 服务端生成 ID 回填只有测试点无实现规则 | 采纳 | §3-5 补实现语义：保存成功后以更新响应回写 form.footer_links（或重新 loadSettings），保证本地行获得稳定 id；后续删除/排序/再保存不携带空 id |

### 终审（闸②，gpt-5.6-sol，结论 block→主会话消化处置，4 条：3 拒 1 采）

| 发现 | 裁定 | 依据（可闭合的权威条款/代码边界） |
|---|---|---|
| P1 坏 JSON 静默→[] 缺日志/可观测 | **拒绝** | 语义经用户裁定（2026-09-24「镜像既有语义」）授权；镜像链 ParseCustomMenuItems/ParseCustomEndpoints 同为静默解析——单链加日志制造平行差异，三链同加=超出已批准范围（终审三项完整性第一条）。§1 已留裁定与理由 |
| P1 公开下发前未重校验持久化值 | **拒绝** | 镜像既有已接受契约（公开 handler 对 custom_endpoints 同样只反序列化不重校验）；"公开读路径加校验边界+失败关闭"=方案外新机制，且直接违反用户裁定（失败关闭使单条设置损坏打爆整个公开设置接口）。风险面由"唯一写入路径带校验"+"坏 JSON 裁定"覆盖，与全站设置链一致 |
| P2 sort_order 排序语义可能伪实现 | **拒绝（审查者漏读快照前端段）** | ①前端 HomeView sortedFooterLinks computed 按 sort_order 升序，HomeView.footer.spec 两模式排序断言在；②SettingsView 增删改排重索引使数组序≡sort_order；③后端写读回环断言读回一致（dto 往返 SortOrder 断言在）；④非数字 sort_order 被 Go DTO int 反序列化失败关闭，API 直调无法注入；⑤公开 API 返回顺序=存储序（契约未承诺 API 排序），展示顺序由前端 computed 保证（§3-4） |
| P2 存量 4 失败归因证据不足（仅叙述） | **采纳（证据留档补强）** | 对照实验已实际执行（HEAD f7f75be48 干净 worktree + 主仓 node_modules 软链同环境，同 spec 4 failed/23 passed），但未落档——补齐到验收证据目录（命令+完整输出+环境说明）；验证表述改为"2255 通过 + 4 存量失败（对照证据）"，不宣称全量零失败 |

**终审放行依据**：拒绝 3 条均给出能闭合的具体权威条款（用户裁定/镜像契约/既有代码断言）；采纳 1 条为证据归档非代码差异——依据防循环条款（合同 §6：无实质代码差异不得再送审），处置表即终审闭环记录；最终验收权在主会话（外审=证据与建议）。
