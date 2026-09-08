# 价格管理中心（Pricing Admin Console）

## Why

当前模型定价的可观测性只存在于日志与服务器文件里：

- 远程价格表（`pricing.remote_url`）同步是否成功、上次同步时间、失败原因，只能 `docker logs` + 查看 `data/model_pricing.json`；
- 哪些已上线模型没有被任何价格层覆盖，只能在计费漏费后翻日志（`Calculate cost failed: ... pricing not found`）才发现；
- 远程表没有的模型（自定义别名、新模型）只能改代码内置价或绕道分组/渠道配置补价；
- 分组倍率与模型基准价两层叠加后的最终单价，管理员无法在后台直接验证。

## What Changes

- 新增管理端"价格管理"页面与 `/api/v1/admin/pricing/*` 接口族，包含五个能力：
  1. **同步状态**：远程表同步状态（上次成功/尝试时间、本地哈希、模型数、最近错误）+ 手动"立即同步"按钮。
  2. **价格目录**：全局价格目录浏览（搜索/分页/来源筛选），每个模型标注实际价格来源分层（`custom` > `remote` > `builtin`），并标注仅图片价（不可用于 token 计费）的条目。
  3. **未覆盖检测**：双通道扫描——静态扫分组配置（ModelsListConfig、分组模型价格）+ 动态扫 usage_logs 近 N 天实际计费模型（含 tokens>0 且 actual_cost=0 的对账标记）；判定复用运行时同一套查价链，杜绝误报。
  4. **自定义价格层**：新增 DB 持久层 `custom_model_pricing`（含区间表），全局生效、支持通配符模型名、可启停；CRUD 经现有管理操作审计中间件自动留痕；远程同步永不覆盖。
  5. **生效价试算**：按模型 × 分组输出解析链叠加分组倍率后的最终单价与样例费用。
- 计费解析链插入 custom 层：`分组价 → 渠道价 → 自定义价（新）→ 远程表 → 内置兜底`。
- 网关计费出现"无价可循"（ErrModelPricingUnavailable）时按模型去重记录（进程内，含首末时间与次数），在同步状态接口中作为"线上计费缺口"暴露。

## Capabilities

### New Capabilities

- `pricing-admin-console`: 定义价格管理页面的同步状态观测、手动同步、目录浏览（来源分层）、未覆盖双通道扫描、自定义价格 CRUD 与生效价试算。

### Modified Capabilities

- 模型定价解析链新增全局 custom 层（介于渠道价与远程表之间）；不改变分组/渠道价语义与既有优先级。

## Impact

- **数据库**：新增 `custom_model_pricing` 与 `custom_model_pricing_intervals` 两表（列结构对齐 `channel_model_pricing` 系列，去掉 channel_id，增加 enabled/remark/created_by）；不修改任何现有表。
- **后端**：新增 repository/service/handler 各一个；`ModelPricingResolver.Resolve` 插入 custom 查找（未配置 custom 服务时行为完全不变）；`PricingService` 增加状态快照与手动同步方法（纯新增）；网关计费零成本路径增加缺口记录钩子。
- **前端**：新增 `views/admin/PricingView.vue`、`api/admin/pricing.ts`、路由、侧栏菜单、zh/en i18n。
- **兼容性**：无 breaking change；custom 层为空时解析结果与现状逐字节一致。
- **审计**：自定义价格的全部写操作走现有 admin 审计中间件（POST/PUT/DELETE 自动记录）。
- **多实例说明**：custom 层快照 60s 周期刷新保证最终一致，同实例 CRUD 即时生效；线上计费缺口记录为进程内数据（与现状日志一致的可观测等级）。

## Non-goals

- 不提供对远程同步表本身的编辑（目录中的 remote 条目只读，调价走 custom 层覆盖）。
- 不做价格变更的历史版本/生效时间窗（审计日志已留痕）。
- 不改变 LiteLLM 家族模糊匹配逻辑，扫描判定直接复用它。
