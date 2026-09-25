# 用户确认包：账号额度/余额熔断与探测覆盖（阶段 0a 冻结产出）

状态：**已裁定（2026-09-26，用户四项裁定 + ④自动闭合）**：
①zhipu 阈值 = **99**；②周期探测**授权启用**（上界 ≤¥0.17/日）；③缺实证 5 账号
**活体实测各 1 次**（授权边界：单次 ≤¥1×10⁻⁴、最多 1 次/账号、验后核对状态）；
④模型可解析性 30/30 通过（无需裁）；⑤**批准进入阶段 1 派发**。
来源方案：`docs/account-quota-balance-circuit-break-plan.md`（R19 方案审通过）。
取证方式：生产库只读 SELECT（43.250.173.110 / sub2api-postgres）+ 源码直读核验 + 官方定价页。零真实上游请求。

## 裁定项①：调度阈值补配值

**现状（生产实查 settings 键 `account_scheduling_thresholds`）**：
`{"anthropic":100,"grok":100,"kimi":100,"minimax":100,"openai":100,"zhipu":100}`。
**代码语义（account_scheduling_threshold_eval.go:49）：`threshold >= 100 = 停用该平台阈值停调`**
——即 zhipu 阈值形同虚设，缺陷①当前完全依赖被动 429 反应链。

**提案**：zhipu = **99**（对齐既有被动反应链"快照任一窗口 ≥99% 即停"的判定口径，
applyCNProviderReactive429 同值），触顶 → 一个探测周期内停调到窗口重置点。
备选：95（更早熔断，代价是牺牲最后 5% 额度的可用窗口）。

**边界说明**：阈值平台枚举不含 deepseek（domain_constants.go:165-173）；deepseek payg
账号走原生余额停调路径（inferaiapi 中转已实证返回余额数据），窗口语义现状无在册
账号（火山号 #32/33 已删除），**不提案新增**。账号级覆盖键
`credentials["account_scheduling_threshold"]` 当前零使用，不启用。

## 裁定项②：付费探测成本上界与启用授权

**探测人群（payg 钱包型，共 11 账号）**：zhipu 中转 ×9（#38/79/80/82/83 TeamoRouter、
#47 Deepinfra、#49 OpenRouter、#84/91 shaulaapi）+ deepseek 中转 ×2（#87 shaulaapi、
#135 TeamoRouter——这两家中转不实现原生 `/user/balance`，实测无余额键回写）。

**成本核算（按权威计价单位，R18-F1 口径）**：
- 单次探测 = 1 请求 ≈ 输入 ~5 tokens + 输出 1 token（max_tokens=1 仅限输出；
  输入按最小输入计）。
- 计价锚（官方标准价）：GLM-5.3-Flash 输入 ¥0.8/M、输出 ¥2.8/M
  （bigmodel.cn/pricing，5 折期已于 09-09 结束按标准价计）→ **单次 ≈ ¥7×10⁻⁶**。
- 最坏上界（10× 余量，覆盖中转加价与最低计费单位差异）：**单次 ≤ ¥1×10⁻⁴**。
- 日最坏上界：11 账号 × 144 探测周期（10min 间隔）= 1,584 次/日 → **≤ ¥0.17/日**
  （名义值 ¥0.011/日）。月度 ≤ ¥5.2。
- RPM/RPD 配额占用：每账号每 10min 1 次请求（6 次/时），相对账号业务并发占用
  可忽略；探测不与真实用户请求争抢（检测不牺牲真实用户请求）。

**授权请求**：按上述上界授权周期探测启用。停用方式 = 版本回退或清除
`cn_balance_low` 语义无持久成本（无预付）。

**②执行修订（实测后，证据 `~/.sub2api-acceptance/sub2api-quota-balance-0a-20260926/04-cost-observations.md`）**：
实测发现 shaulaapi 中转按 519 输入 token 计费（中转侧固定注入）、4 家中转中
2 家忽略 max_tokens=1——名义成本 ≈¥0.1/日、**最坏上界 ≈¥9.5/日**（较本节
原估 ¥0.17/日上移，绝对量级仍为可忽略档）。需硬性控本时既有旋钮 =
BalanceCheckIntervalMinutes（现 10min），不新增机制。

## 裁定项③：平台耗尽特征证据确认

**生产实证（现成，判定器已精确识别——停调 reason 原文即证据）**：
| 中转 | 账号 | 实证文案（生产 temp_unschedulable_reason） |
|---|---|---|
| TeamoRouter | #82 | `TeamoRouter 钱包余额不足，请前往 …` |
| TeamoRouter | #38/79/80 | `当日GLM 5.3 flash福利版免费额度已耗…` |
| Deepinfra | #47 | `You need positive balance…` |
| OpenRouter | #49 | `This request requires mo…` |

**缺实证账号（三选一送裁：受控活体实测 / 豁免 / 留阻断）**：
| 账号 | 平台 | 说明 |
|---|---|---|
| #84/#91 | zhipu×shaulaapi | 无历史耗尽文案；活体实测单次上界见裁定项② |
| #87 | deepseek×shaulaapi | 同上 |
| #135 | deepseek×TeamoRouter | 同品牌 zhipu 侧已有两条实证文案，品牌级证据可否平移请裁 |
| #134 | other×星思云站 | 中转，映射含 glm/kimi/deepseek 多模型；余额语义未知 |

活体实测授权边界（若选实测）：专用非承载流量账号或直接以该账号探测、单次消耗
上界 ¥1×10⁻⁴、最多 1 次/账号、验后核对该账号状态；未获明确授权即保持阻断。

**③执行结果（2026-09-26 实测，证据 `~/.sub2api-acceptance/sub2api-quota-balance-0a-20260926/02-live-probe-results.json`）**：
#84/#91/#87/#135 全部 200——探测路径与模型映射（含 #135 的
`deepseek-v4.1-flash→deepseek-flash-free` 映射）全通；耗尽文案覆盖仍需真实耗尽
发生时验证（与 kimi/deepseek 原生路径同风险姿态，不设前置机制）。
**#134 星思云 403 API_KEY_EXPIRED：密钥已过期，该账号对真实业务流量已失效**
（运维发现，移交用户换 key/停用；认证错误域，不入余额探测机制面）。
测前测后账号状态零扰动。

## 裁定项④：模型可解析性核验（R17-F1）

`resolveWebTestModel` 无错误返回路径（account_test_service.go:675-683）：显式模型 →
`DefaultWebModelIDs(platform,"web")[0]`（zhipu=`glm-5.3-flash`，deepseek=`deepseek-chat`，
composite_platform.go:238/240）→ `GetMappedModel`（无映射原样返回）。

**核验结论：30/30 全部可解析，零阻断**。29 账号有非空 model_mapping；#82 空映射
走 zhipu 平台默认 `glm-5.3-flash`。无需豁免、无需收窄 Done-when。

## 裁定项⑤：实施批准

确认包各项裁定后，按方案 §3.4 进入阶段 1 派发编码（文件+测试对粒度，
codebuddy hy3 执行）。改动面（阶段 1 预告，最终以派发单为准）：
1. `cn_provider_balance_check_service.go` 扩展：payg 钱包账号最小完成请求探测分支
   （复用 §2.7 探测链与 `cnProviderResponseIndicatesInsufficientBalance` 判定）。
2. `probeQuota` 快照后调用 `ApplyAccountSchedulingThreshold`（照抄
   codebuddy_quota_check_service.go:119 先例，coding 账号生效）。
3. 定向测试：响应矩阵（含 429 重叠正反用例+余额分支断言）、前缀精确清除、
   原生/中转参数化、coding 触顶停调。

## 附：现状盘点全量数据（0a 只读产出，2026-09-26）

- **平台分布**：zhipu 16（11 apikey + 5 oauth）、deepseek 11（6 apikey + 5 oauth）、
  other 3；minimax/volcano 零在册账号（#32/33 火山号已删除）。
- **模式分布**：coding 2（#58/#132，窗口快照在 `zhipu_5h_*` 等键，活跃）；
  payg 15；无 mode（oauth 计量）13。
- **探测路径分配**：
  - 原生余额路径（既有机制已覆盖）：inferaiapi ×4（#52/54/56/97，
    `deepseek_balance*` 键活跃，low=false）+ 官方 deepseek oauth ×5（112-130）；
  - 最小完成请求路径（本方案新增）：payg 中转 11 个（裁定项②清单）；
  - coding 窗口阈值路径（本方案新增调用）：#58/#132；
  - oauth 计量 ×13：无窗口快照（IsCodingPlan 过滤）、无钱包语义；既有机制已观测到
    其结构化 reason 停调（#118/#121 现证），不在本方案两缺陷机制面内，列为现状
    观察项，不扩机制（防过度设计）。
- **`zhipu_balance_low` 标记**：恰 6 账号为 true（= 当前 cn_balance_low 停调集），
  反应链写入、无周期读取方——缺陷②"有判定、有停调、缺周期探测"的精确实证。
- **代码语义核验**（关键常量）：`platforms()={kimi,deepseek}`；volcano 无余额实现
  （仅额度链）；余额判定=既有文案匹配器（余额不足/insufficient balance/
  insufficient_credit/balance is not enough/no enough balance）；停调=2×探测周期
  （20min）；清除守卫=`HasPrefix(reason,"cn_balance_low")`（:255 模式）。
