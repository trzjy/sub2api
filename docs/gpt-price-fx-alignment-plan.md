# GPT 两组价格汇率对齐 + deepseek-v4-pro 上游对齐方案

- 日期：2026-09-24
- 状态：已获用户裁定，待外审后执行
- 生产库：`yiyutu-server` → `docker exec sub2api-postgres psql -U sub2api -d sub2api`
- SQL：`docs/gpt-price-fx-alignment.sql`（同目录附录，唯一执行入口）

## 1. 改什么 / 依据什么权威 / 完成标准（合同 §1）

**改什么**：仅改生产库 3 行数据——`groups` 表 id=82、id=37 的 `rate_multiplier` + `model_pricing`；id=49 的 `model_pricing`（追加 deepseek-v4-pro 一张组卡）。不改任何代码、不改 `custom_model_pricing`、不改订阅组。

**依据什么权威**：
1. 用户裁定（2026-09-24）："人家 1:1 充值，我们是 6.8:1 充值，在人家价格基础上，我们应该先除 6.8，再加一部分利润" + 追问裁定利润率 = **30%**、范围 = **GPT 两组 + deepseek-v4-pro**。
2. 用户提供的上游价卡实证：gpt-6-astra 卡面价（≤272K $1.30 / >272K $2.60 / 输出 $6.50、$9.75 / 缓存写 $1.625 读 $0.13 等），证明上游广场展示的就是"基价 × 组倍率"后的**实收价**。
3. 上游实收价：`api.shaulaapi.com/api/v1/model-plaza`（匿名接口，取证存 `/tmp/shaula-plaza.json`，副本入证据目录）。
4. 计费语义（仓库 SSOT）：`billing_service.go` `bd.ActualCost = bd.TotalCost * rateMultiplier`；解析链 `Group → Channel → Custom → LiteLLM → Fallback`（`model_pricing_resolver.go:53`），组卡覆盖 custom 行——这是 v4-pro 只动组 49 不动全局 custom 行的机制依据。

**定价规则（唯一口径）**：

```
我们目标实收(¥/M) = 上游实收(¥/M，因上游 1:1，$即¥) × 1.30
组卡 raw($/token) = 目标实收 ¥/M ÷ 6.8 ÷ 组rate_multiplier × 1e-6
校验恒等式: raw × mult × 6.8 × 1e6 = 上游实收 × 1.30
```

**做到什么算完成**：
1. SQL 在生产一个事务内执行成功（3 行 UPDATE）。
2. 执行后 `GET /api/v1/model-plaza`（等配置缓存刷新后）两组全部模型的有效价 = 本方案目标表逐条一致（±0.01 ¥/M 舍入）。
3. 证据落盘 `/home/zjy/.sub2api-acceptance/sub2api-gpt-price-fx-20260924/`（00-deploy、99-final-report、前后配置、plaza 取证）。
4. 明确登记遗留挂账（§6），不含未声明的行为变化。

## 2. 现状问题（根因）

两组 GPT 组卡为空，基价走 LiteLLM 内置目录，价格数值是从上游**原样抄来的美元数**（如 gpt-5.5 in $5/M），组倍率也原样抄（0.25/0.15），全程未折算 6.8 汇率。上游同样 1:1 收费 → 我们实收恰好虚高约 6.8~8.1 倍：

- 组37 gpt-max满血：gpt-5.5 实收 ¥85/M ≈ 上游满血组（¥5/M）的 **7.0×**
- 组82 GPT-特价：gpt-5.5 实收 ¥34/M ≈ 上游 PLUS 组（¥4.225/M）的 **8.1×**

（DeepSeek/GLM/Kimi 计量组此前已按 CodeBuddy/火山实测成本校准过，不在本方案范围，仅 v4-pro 一款经裁定纳入。）

## 3. 锚定规则（无单一路径时的取数规则）

上游不同分组同模型价不同，逐模型按下表锚定（已获裁定范围=两组+v4-pro；锚定选择属于实施取数，非新语义）：

| 我方组 | 锚定上游组 | 倍率 | 适用 |
|---|---|---|---|
| 组82 GPT-特价 | GPT PLUS | ×0.13 | 上游 PLUS 组有的模型 |
| 组82 GPT-特价 | GPT Pro | ×0.24 | 上游 PLUS 没有的模型（gpt-5.2、5.3-codex-spark、5.4、5.4-mini、5.6-luna） |
| 组82 GPT-特价 | 画布·GPT 生图 ×1 | ×1 | gpt-image-2 |
| 组82 GPT-特价 | 无锚定 | — | 上游完全没有的模型（gpt-6、gpt-image-1/1.5、2.5-flare/sunburst）：现价 ÷6.8 ×1.3 保持相对位置 |
| 组37 gpt-max | GPT 满血版（不降智） | ×0.25 | 上游满血组有的模型 |
| 组37 gpt-max | GPT Pro | ×0.24 | 满血组没有的（gpt-5.6） |
| 组37 gpt-max | 画布·GPT 生图 ×1 | ×1 | gpt-image-2 |
| 组37 gpt-max | 无锚定 | — | gpt-6：现价 ÷6.8 ×1.3 |
| 组49 DeepSeek 计量 v4-pro | DeepSeek 福利 | ×0.06→×1.3 | 用户指定范围 |

## 4. 目标对照表（实收 ¥/M，输入价；输出/缓存价按上游卡同比例，完整值见 SQL）

组82（mult 0.1500→**0.0249**，17 卡）：

| 模型 | 锚 | 上游实收 ¥/M | 我们目标 ¥/M | 现价 ¥/M |
|---|---|---|---|---|
| codex-auto-review | PLUS | 0.0975 | 0.1268 | ~5.10 |
| gpt-5.2 | Pro | 0.4200 | 0.5460 | ~9.28 |
| gpt-5.3-codex-spark | Pro | 0.3600 | 0.4680 | ~7.14 |
| gpt-5.4 | Pro | 0.6000 | 0.7800 | ~9.18 |
| gpt-5.4-mini | Pro | 0.1800 | 0.2340 | ~2.38 |
| gpt-5.5 | PLUS | 0.6500 | 0.8450 | ~34.00 |
| gpt-5.6 | PLUS | 0.5200 | 0.6760 | ~17.00 |
| gpt-5.6-luna | Pro | 0.0480 | 0.0624 | ~1.53 |
| gpt-5.6-sol | PLUS | 0.5200 | 0.6760 | ~17.00 |
| gpt-5.6-terra | PLUS | 0.2600 | 0.3380 | ~8.50 |
| gpt-6 | 无锚定 | （现价） | 1.9500 | 10.20 |
| gpt-6-astra | PLUS | 1.3000 | 1.6900 | ~42.50 |
| gpt-image-1 | 无锚定 | （现价） | 0.9750 | 5.10 |
| gpt-image-1.5 | 无锚定 | （现价） | 0.9750 | 5.10 |
| gpt-image-2 | 生图×1 | 5.0000 | 6.5000 | ~5.10* |
| gpt-image-2.5-flare | 无锚定 | （现价） | 0.9750 | 5.10 |
| gpt-image-2.5-sunburst | 无锚定 | （现价） | 0.9750 | 5.10 |

\* gpt-image-2 现价按 LiteLLM 目录 in $0.00566/ tok 折算后口径有出入，目标价锚上游生图卡 ¥5.00×1.3=¥6.50，为上调，单独列出避免"降价"误读。

组37（mult 0.2500→**0.0478**，8 卡）：

| 模型 | 锚 | 上游实收 ¥/M | 我们目标 ¥/M | 现价 ¥/M |
|---|---|---|---|---|
| gpt-5.5 | 满血 | 1.2500 | 1.6250 | 85.00 |
| gpt-5.6 | Pro | 0.9600 | 1.2480 | 68.00 |
| gpt-5.6-luna | 满血 | 0.0500 | 0.0650 | 3.40 |
| gpt-5.6-sol | 满血 | 1.0000 | 1.3000 | 68.00 |
| gpt-5.6-terra | 满血 | 0.5000 | 0.6500 | 34.00 |
| gpt-6 | 无锚定 | （现价） | 3.2500 | 17.00 |
| gpt-6-astra | 满血 | 2.5000 | 3.2500 | 106.25 |
| gpt-image-2 | 生图×1 | 5.0000 | 6.5000 | ~28.9 |

组49 v4-pro（mult 0.12 不变，加 1 卡）：输入 ¥0.54→**0.702**，输出 ¥1.62→**2.106**，缓存读 ¥0.018→**0.0234**。

**数值精度说明**：`groups.rate_multiplier` 是 numeric(10,4)，倍率只能 4 位小数（0.0249 / 0.0478），舍入差已折入组卡 raw 价，恒等式按"raw × mult × 6.8 × 1e6 = 上游 × 1.30"精确成立。组卡 `model_pricing` 是 jsonb 全精度浮点，无 12 位限制。

**长上下文阶梯**：两组 `long_context_pricing_enabled` 已为 true（峰值速率 false），上游带 >272K 阶梯的模型（5.4/5.5/5.6 系列/astra/auto-review）卡内 intervals 第二档（输入×2、输出×1.5、缓存×2）会真实计费，与上游卡一致；上游无阶梯的（5.2、5.3-codex-spark、5.4-mini、v4-pro）卡内无 intervals。

## 5. 影响面（消费者闭包）

| 消费方 | 影响 |
|---|---|
| 计费主链路（billing_service.go） | 组卡经 resolver 生效，ActualCost=TotalCost×mult，语义不变、数值变 |
| 模型广场（/api/v1/model-plaza） | 组卡带 platform/models，广场按组展示新有效价；配置有缓存，需等刷新后验证 |
| 订阅组 57/60/96（deepseek 订阅） | **不受影响**：v4-pro 新卡只进组 49，custom 行 27 未动，订阅额度消耗速度不变 |
| custom_model_pricing 行 27 | **不动**（计量组 v4-pro 由组卡覆盖） |
| 组 49 其他模型（v4.1-flash 等） | **不动**：组卡只含 v4-pro，其余模型仍走 custom 行 |
| 上游账号 bookkeeping（gateway_usage_billing accountCost） | 不动。尾宿账号 rate_multiplier 按 1:1 假设记账的问题见 §6 挂账 |
| 充值/汇率（payment FX_RATES CNY=6.8） | 不动，本方案正是把模型计费对齐到该汇率 |

## 6. 风险与挂账（如实登记，不在本次处理）

1. **v4-pro 新价低于火山编码实测成本**：新实收 ¥0.702/M（in），若该组路由到火山 coding 包（实测约 ¥1.1/M）则毛利为负。当前组 49 `model_routing` 为空 `{}`（禁用），按现有路由不触发；挂账：后续若启用火山路由需复核。
2. **非尾宿账号在 GPT 组内的毛利不可控**：zzz(0.05×)、WawAPI(0.06×)、api-top(0.14×) 等账号同样吃到新售价，各自毛利=售价−其上游成本，未逐一核算。本次只对齐售价端；账号侧 bookkeeping 挂账后续单独任务。
3. **尾宿账号 rate_multiplier 记账口径**：账号倍率按"我方标价×倍率=上游扣费"且按 1:1 汇率填的（0.06~1.0），实际 6.8 汇率下数值口径偏大 6.8 倍，只影响内部成本统计，不影响用户实扣。挂账：待用户裁定是否把账号倍率×6.8 归一（会改变成本报表读数）。
4. **v4-pro 缓存读从免费变收费**：custom 行 27 缓存读为空（免费），新组卡按上游福利卡开始收 ¥0.0234/M（约等于忽略量级），与"按上游卡对齐"一致，明示不算偏差。
5. **image 类按 token 计费**：gpt-image 系列在上游和我方均为 token 计价（billing_mode=token），本次照抄上游口径，不含按张计费改造。
6. **无锚定模型缓存字段偏差（已执行后实证，接受并登记）**：gpt-6（组 82/37）与 gpt-image-1/1.5/2.5-flare/2.5-sunburst（组 82）5 张组卡的 cache_write/cache_read 留 NULL。计费语义（billing_service.go:1336 `applyChannelTokenPriceOverrides`，组卡路径 resolver.go:316 走同一函数）：nil 字段保留 LiteLLM 目录基价 ×新倍率，不清零。结果：缓存计费 = 目录基价 × 新倍率，约为规则值的 86.8%（低 13%，对用户有利）。例：组 82 gpt-6 缓存写实收 ¥2.115/M（规则值 ¥2.4375），缓存读 ¥0.169/M（规则值 ¥0.195）；绝对差 ≤¥0.32/M。如需严格对齐规则值，候选补丁 raw 值：gpt-6 组82 cw=1.4397e-5/cr=1.15172e-6；组37 cw=1.24985e-5/cr=9.99877e-7；image 系列组82 cr=1.43966e-6。未应用（改动最小化，待用户裁定）。
7. **无锚定模型 >272K 长上下文阶梯（已实证，非问题）**：组卡 intervals 在 Resolve() 中被剥离（model_pricing_resolver.go），但阶梯由 LiteLLM 目录条目的 LongContext 字段乘法保留（billing_service.go:483-485，阈值 272K，in ×2 / out ×1.5 / cache ×2，受 long_context_pricing_enabled 开关）。plaza-after 实测组 82 gpt-6 组卡显示 >272K 区间 = tier-1 ×2/×1.5（2.30333e-05 = 1.15166e-05×2），与新 tier-1 单价成比例，语义与改前一致。

## 7. 回滚

事务原子：执行失败自动回滚。执行后需回滚时，用证据目录 `backup-group-pricing-20260924.json` 中的现值反向 UPDATE（82: 0.1500/`[]`；37: 0.2500/`[]`；49: `[]`）。custom 行 27 未动，无连带回滚。

## 8. 验收清单（执行结果 2026-09-25）

- [x] ~~外审（codex-companion review，资金边界 §6）完成且发现项已逐条消费~~ → **用户裁定豁免**（2026-09-25："那你直接改不就行了吗，这玩意儿又不用外审"）。改前 codex-companion 已尝试 3 次均因审查通道自身故障（上游 gpt-5.6-sol 后端与 codex 0.155.1 工具协议不匹配）无法产出审查内容，按合同失败关闭处理后用户明示豁免。
- [x] 生产备份文件落盘（backup-group-pricing-20260924.json）
- [x] SQL 单事务执行成功，3 行 UPDATE，行数核对 3（BEGIN/UPDATE 1×3/COMMIT）
- [x] plaza API 逐模型核对 + 2026-09-25 复核（证据 06）：123 项 OK / 0 FAIL / 6 处 DEV-DECLARED（全部为 §6.6 无锚定卡 NULL 缓存字段，目录基价×新倍率，低 13%，用户有利）/ 3 处 OK-BY-DESIGN（image 系目录无 token 输出基准，卡内取 in=out）；>272K 区间 t2/t1 比例 ×2/×1.5 全部命中（§6.7）
- [x] 组 49 v4-pro 有效价 in ¥0.702 / out ¥2.106 命中；订阅组 57/60/96 未动（SQL 未含其 id）
- [ ] 证据目录齐备：00-deploy.md、99-final-report.md、backup、执行后 plaza 取证、SQL 副本 → 见 /home/zjy/.sub2api-acceptance/sub2api-gpt-price-fx-20260924/
- [x] 自审多轮收敛（执行遗漏 / 全局一致性 / 资金边界）→ 结果见 99-final-report.md
