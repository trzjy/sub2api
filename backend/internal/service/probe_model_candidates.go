package service

import (
	"sort"
	"strings"
)

// ProbeModelCandidate 是一个可探模型候选：同时保留「请求侧模型键」与「上游真名」两个事实。
//
// 两个都要保留的原因：cursor 分支要的是**入站键**（它要求键命中映射，
// 出站时再把键解析成上游名）；通用分支要的是**上游真名**（直接发给上游）。
// 两者都从同一份事实（账号的 model_mapping）派生 ⇒ 一个事实源、两种投影。
type ProbeModelCandidate struct {
	// InboundKey 是请求侧模型名（= credentials.model_mapping 的键）。
	InboundKey string
	// Upstream 是上游真名（= credentials.model_mapping 的值）。
	Upstream string
}

// probeModelCandidates 收集该账号可探模型的候选 —— 全站探针挑模型名的**唯一事实源**。
//
// 候选来源：管理员在账号编辑弹窗「模型限制 → 模型白名单」里维护的账号模型名单
// （credentials.model_mapping）解析出的上游真名。这是管理员认定该账号可服务的模型集合，
// 探针从里面挑来做文本探测。
//
// 只排除**物理上不可探**的候选：
//   - 解析不到 / 为空（不是可发出的模型名）；
//   - 含 `*` 的通配符（不是具体模型名）；
//   - 图片/媒体模型（isOpenAIImageGenerationModel —— 图片模型无法做文本探测）。
//
// **不做任何基于模型名形态的过滤**（不按 `gpt-` 前缀、不按厂商前缀）：名单里的模型名
// 一律可用（管理员可自定义命名，例如 codex-auto-review 一类非 `gpt-` 命名的模型）。
// 按名字形态丢弃候选会把「明明有可用模型」变成「无模型可探」⇒ 账号永远回不了池。
//
// **绝不回落任何硬编码模型名**：映射为空就返回空切片，由调用方按各自既有语义处理
// （取首个 / 逐候选回退）。抓一个猜测的模型名去打上游必然 404，
// 那是账号永久卡在探测态的根因（见 docs-local/probe-model-selection-unification-plan-20261123.md）。
//
// 返回**完整候选列表**，不预先挑一个：各调用方按自己既有语义消费
// —— 只需一个的取首个，需要逐个回退的用整张列表。
func probeModelCandidates(acc *Account) []ProbeModelCandidate {
	if acc == nil {
		return nil
	}
	mapping := acc.GetModelMapping()
	if len(mapping) == 0 {
		return nil
	}
	candidates := make([]ProbeModelCandidate, 0, len(mapping))
	seen := make(map[string]struct{}, len(mapping))
	for key := range mapping {
		inboundKey := strings.TrimSpace(key)
		if inboundKey == "" {
			continue
		}
		upstream, matched := acc.ResolveMappedModel(key)
		upstream = strings.TrimSpace(upstream)
		if !matched || upstream == "" || strings.Contains(upstream, "*") {
			continue
		}
		if isOpenAIImageGenerationModel(upstream) {
			continue
		}
		if _, dup := seen[upstream]; dup {
			continue
		}
		seen[upstream] = struct{}{}
		candidates = append(candidates, ProbeModelCandidate{
			InboundKey: inboundKey,
			Upstream:   upstream,
		})
	}
	// 按上游真名字典序排序以保证候选顺序可复现（map 遍历顺序随机）。
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Upstream != candidates[j].Upstream {
			return candidates[i].Upstream < candidates[j].Upstream
		}
		return candidates[i].InboundKey < candidates[j].InboundKey
	})
	return candidates
}

// pickProbeModelCandidate 在候选里挑一个：按排序后取首个以保证可复现。
//
// 候选收集处（probeModelCandidates）已排除物理上不可探的取值，故此处对任何候选
// 一视同仁，不再按模型名形态二次筛选，也不丢弃任何候选。
func pickProbeModelCandidate(candidates []ProbeModelCandidate) ProbeModelCandidate {
	if len(candidates) == 0 {
		return ProbeModelCandidate{}
	}
	return candidates[0]
}

// pickProbeModelUpstream 取候选中首个的上游真名；无候选返回空串。
func pickProbeModelUpstream(candidates []ProbeModelCandidate) string {
	return pickProbeModelCandidate(candidates).Upstream
}

// pickProbeModelInboundKey 取候选中首个的请求侧模型键；无候选返回空串。
func pickProbeModelInboundKey(candidates []ProbeModelCandidate) string {
	return pickProbeModelCandidate(candidates).InboundKey
}

// probeModelUpstreams 返回候选的上游真名列表（供需要逐候选回退的调用方使用）。
func probeModelUpstreams(acc *Account) []string {
	candidates := probeModelCandidates(acc)
	if len(candidates) == 0 {
		return nil
	}
	upstreams := make([]string, 0, len(candidates))
	for _, c := range candidates {
		upstreams = append(upstreams, c.Upstream)
	}
	return upstreams
}
