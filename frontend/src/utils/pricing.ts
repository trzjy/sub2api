/**
 * formatScaled formats a per-token (or per-request) USD price scaled by `scale`.
 *
 *   formatScaled(0.000003, 1_000_000)    → "$3.00"     // ≥$0.1 固定 2 位小数
 *   formatScaled(9.2e-8,   1_000_000)    → "$0.092"    // <$0.1 保留 2 位有效数字
 *   formatScaled(0.5,        1)          → "$0.50"     // per request
 *   formatScaled(null,       1_000_000)  → "-"
 *
 * 展示约定：≥$0.1 一律固定 2 位小数（历史口径）；<$0.1 退到 2 位有效数字并去掉
 * 尾零——极小价格（缓存读等）硬凑 2 位小数会有不可接受的舍入失真。计费侧仍用
 * 全精度，本函数只管展示。
 * `minFractionDigits` 把结果补齐到最少小数位（不影响已超出的小数）。
 */

/* w64 conflict-resolution (second wave, B10 — funds_or_state / semantic_conflict)
 *
 * 冲突点：formatScaled 的数值格式化语句，三方互斥（base 单行 / 我方分档 / 上游单行替换）。
 *   base       (value * scale).toPrecision(10) 再去掉尾部零（正则形态）
 *   OURS       分档：|scaled| >= 0.1 固定 2 位小数；0 < |scaled| < 0.1 取 2 位有效数字并去尾零
 *   UPSTREAM   仍是 toPrecision(10)，但去尾零换成 Number((value * scale).toPrecision(10)).toString()
 *
 * 裁决：keep OURS（展示口径）。依据两条：
 *   1) 派发单"跨文件签名权威表"第 7 条 —— funds 数值/口径保留我方值，双方值须登记；
 *   2) 前端批规则 —— 展示口径冲突默认我方。
 * 我方口径是产品侧既定约定：>= $0.1 固定 2 位小数便于价格列纵向对齐；< $0.1 退 2 位
 * 有效数字避免缓存读价被硬凑成 "$0.00"。上游口径会把 "$3.00" 打回 "$3"、"$0.20" 打回
 * "$0.2"，并在 1e-10 这类值上输出科学计数法 "$1e-10"，UI 不可读。
 *
 * 双方值逐条登记：取证口径是 29 条用例（我方 spec 13 条 + 上游 spec 11 条 + 双方 doc
 * 示例 2 条，含参数重复项 + 本卡补的 minFractionDigits 探针 3 条），node 三方实现实算
 * 而非推断，结果一致 13 / 互斥 16。完整表（含每条形成的 verdict 与 base 参考列）落在
 * __tests__/formatScaled.spec.ts 的 FUSION_CASES，并在 spec 里用内联的 base/upstream
 * 参考实现动态自证，证据见报告 (c)。
 *   一致 13 条（我方 == 上游，B11 可直接吸收）：
 *     (3e-6,1e6,2) 与 (0.000003,1e6,2) 共 3 条 "$3.00" | (5e-8,1e6,0) "$0.05"
 *     (0,1,2) "$0.00" | (null,1e6,0) 共 2 条 "-" | (9.2e-9,1e6,0) "$0.0092"
 *     (2e-8,1e6,0) "$0.02" | (0.5,1,2) "$0.50" | (9.2e-8,1e6,0) "$0.092"
 *     (45,1,3) "$45.000" | (3e-6,1e6,3) "$3.000"
 *   互斥 16 条（我方 / 上游）：
 *     (3e-6,1e6,0)      "$3.00"                 / "$3"
 *     (0.000003,1e6,0)  "$3.00"                 / "$3"
 *     (0.2,1,0)         "$0.20"                 / "$0.2"
 *     (0.5,1,0)         "$0.50"                 / "$0.5"
 *     (45,1,0)          "$45.00"                / "$45"
 *     (0,1,0)           "$0.00"                 / "$0"
 *     (1.2426470588235295e-7,1e6,0) "$0.12"     / "$0.1242647059"
 *     (1.863970588e-8,1e6,0)        "$0.019"    / "$0.01863970588"
 *     (9.18e-11,1e6,0)              "$0.000092" / "$0.0000918"
 *     (1.25e-8,1e6,2)   "$0.012"                / "$0.0125"
 *     (1e-10,1,0)       "$0.0000000001"         / "$1e-10"
 *     (1.25e-10,1,0)    "$0.00000000013"        / "$1.25e-10"
 *     (1e-20,1,0)       "$0.00000000000000000001" / "$1e-20"
 *     (1e-16,1e6,0)     "$0.0000000001"         / "$1e-10"
 *     (1e10,1,0)        "$10000000000.00"       / "$10000000000"
 *     (1.25e-8,1e6,4)   "$0.0120"               / "$0.0125"
 *
 * minFractionDigits 分支现状：双方 spec 都没有 mfd > 2 的用例，而我方两个分支恒产出
 * ≥2 位小数，所以登记的前 26 条里该分支一次都没被激活（变异 M7 把补齐逻辑整段禁用后
 * 26 条仍全绿，是本轮暴露的盲区）。补 3 条探针 P1/P2/P3 把它激活：P1 (45,1,3) 与
 * P2 (3e-6,1e6,3) 证明补齐后双方一致，P3 (1.25e-8,1e6,4) 证明"只补齐不截断"在我方
 * 口径下成立且与上游互斥。该分支本身双方实现逐字相同（base/ours/upstream 三方一致），
 * 不属于本卡冲突范围，探针只为让 29 条登记值不再有死代码路径。
 *
 * 上游那一行不是口径变更，而是修 base 的一个真 bug：toPrecision(10) 在 |x| >= 1e10 或
 * |x| < 1e-6 时输出科学计数法（如 "1.000000000e-10"），base 的去尾零正则会把指数尾数的 0
 * 当成尾零剥掉，于是 1e-10 显示成 e-1、1e10 显示成 e+1 —— 数量级错乱。上游用
 * Number(...) 转字符串绕开正则。我方两个分支都走 toFixed（恒为定点十进制，不含指数），
 * 该 bug 在我方口径下结构性不存在，故上游此行无可吸收的代码，只需钉住其意图：
 * formatScaled(1e10, 1) 必须保住数量级（见 __tests__/formatScaled.spec.ts 的 W64-M 组）。
 *
 * 本卡吸收该意图的唯一代码改动：<0.1 分支的位数加 100 上限。原式
 * 1 - floor(log10|scaled|) 在 |scaled| < 1e-100 时算出 >100，越出 toFixed 的合法域
 * (0..100) 会抛 RangeError，令整张价格表渲染中断；上游形态不会抛（它输出 "1e-150"）。
 * 实用区间远达不到该量级（渠道价 1e-9..1e-4 乘 1e6 得 1e-3..1e2），加 clamp 前后
 * 上面 29 条登记值与 1e-300..1e300 的对数扫描输出逐字相同（spec 的
 * describe 'w64: clamp 到 100 位小数（实用区间零行为变化）'，报告 (c) 的 M1 轮）。
 *
 * 已知边界（不补断言，交由 B11 判是否值得收）：|scaled| ≥ 1e21 时 toFixed 自身回退科
 * 学计数法（(1e21).toFixed(2) === '1e+21'），"我方恒为定点十进制"在该量级不成立。此
 * 处双方输出恰好相同（都是 "$1e+21"），属共同的表示极限而非口径分歧；spec 的
 * magnitude 组只覆盖到 1e10，未把 1e21 钉成断言（变异 M13 用于证明该边界确实不被守）。
 *
 * 收敛单元与 owner：
 *   (a) 本文件 formatScaled 的展示口径 —— w64，keep OURS，已定案。
 *   (b) 上游新增测试 frontend/src/utils/__tests__/pricing.formatScaled.spec.ts —— 上游
 *       独有文件，本机入树数 0（当前不存在）。B11 若原样灌入，其 11 条里 7 条必挂
 *       （5 条 magnitude + (0.000003,1e6,0) + (1.25e-8,1e6,2)），余 4 条与我一至。
 *       处置方向：按上面登记值改写期望，不要反向替换实现。缺席哨兵见 spec 的 W64-A 组。
 *   (c) 四个消费方 PricingRow.vue / SupportedModelChip.vue / PlazaMergedTable.vue /
 *       PlazaModelPricingTable.vue —— 调用形态双方一致（上游只在 SupportedModelChip 多
 *       一处按秒拼接，属他卡），口径裁决不需要改任何调用点。
 *   (d) 计费后端 —— 全精度另算，本函数只管展示，无跨端契约需要同步。
 */
export function formatScaled(value: number | null, scale: number, minFractionDigits = 0): string {
  if (value == null) return '-'
  const scaled = value * scale
  let s: string
  if (scaled !== 0 && Math.abs(scaled) < 0.1) {
    const decimals = Math.min(100, Math.max(2, 1 - Math.floor(Math.log10(Math.abs(scaled)))))
    s = scaled.toFixed(decimals).replace(/\.?0+$/, '')
  } else {
    s = scaled.toFixed(2)
  }
  if (minFractionDigits > 0 && !s.includes('e')) {
    const dot = s.indexOf('.')
    const digits = dot === -1 ? 0 : s.length - dot - 1
    if (digits < minFractionDigits) {
      s = (dot === -1 ? `${s}.` : s) + '0'.repeat(minFractionDigits - digits)
    }
  }
  return `$${s}`
}

import type { UserPricingInterval } from '@/api/channels'

type TokenPrices = Pick<UserPricingInterval, 'input_price' | 'output_price' | 'cache_write_price' | 'cache_write_1h_price' | 'cache_read_price'>

export function resolveIntervalPrices(iv: UserPricingInterval, base: TokenPrices): UserPricingInterval {
  const price = (absolute: number | null | undefined, multiplier: number | null | undefined, fallback: number | null | undefined) =>
    absolute ?? (fallback == null ? null : fallback * (multiplier ?? 1))
  return {
    ...iv,
    input_price: price(iv.input_price, iv.input_multiplier, base.input_price),
    output_price: price(iv.output_price, iv.output_multiplier, base.output_price),
    cache_write_price: price(iv.cache_write_price, iv.cache_write_multiplier, base.cache_write_price),
    // Resolver uses an explicit cache-write price for both durations unless 1h is overridden.
    cache_write_1h_price: iv.cache_write_1h_price ?? iv.cache_write_price ?? price(null, iv.cache_write_multiplier, base.cache_write_1h_price),
    cache_read_price: price(iv.cache_read_price, iv.cache_read_multiplier, base.cache_read_price)
  }
}
