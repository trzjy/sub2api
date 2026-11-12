package service

import (
	"strings"
)

// 上游失败换号策略 —— 全站唯一事实源。
//
// 这里收口两件此前各自解释的事：
//
//  1. 「哪些上游状态码值得换号」。
//     此前 GatewayService.shouldFailoverUpstreamError 与
//     OpenAIGatewayService.shouldFailoverUpstreamError 各维护一份内容相同、
//     注释却不同的集合（后者是前者的复制粘贴）。改一处不会同步另一处。
//
//  2. 「上游 4xx 该怪账号还是怪客户」。
//     此前只有零散特例被单独识别（审核拒绝、模型不存在、上下文窗口超限），
//     其余 4xx 一律当成「客户请求写错了」原样透传。
//
// 第 2 条最典型的受害者是模型名改写：客户请求模型 A，我们按账号的 model_mapping
// 改写成 B 发给上游，上游回 4xx 说「参数不对」——那是 B 不接受这个请求，
// 不是客户写错了 A。此时把上游原文透传给客户，等于让客户去修一个我们自己造出来的问题。
//
// 2026-11-12 生产实证：当日 upstream 400 共 186 条，其中「本次请求的模型名被改写过」
// 的占 186/186（100%），180 条来自账号 982 的 deepseek-flash → spark-x2.6-4b
// 映射（上游讯飞错误码 10005「用户参数值有错误」）。同一客户端在其它账号
// （LongCat / ahooqq）几乎零失败，证明不是客户请求有问题。
//
// 规则收口在本文件，所有错误判定点都调它，不在各协议路径上各加一条分支。

// shouldFailoverUpstreamStatusCode 判定上游状态码是否值得换一个账号重试。
//
// 402（余额/额度耗尽）与 OpenAI 网关口径一致：上游说这个账号当前不可服务，
// 就换号，而不是把错误透传给客户。
func shouldFailoverUpstreamStatusCode(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 405, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

// IsMappingInducedRejection 判定「这次上游 4xx 由模型名改写造成」，
// 即责任在账号/上游侧，不在客户侧。
//
// 成立条件：
//  1. 上游返回 4xx（请求类拒绝）。5xx 属上游可用性问题，不归本判据。
//  2. 本次请求的模型名在账号的 model_mapping 里被改写过——即查表命中且映射结果
//     与查表键不同。恒等映射（键 == 值）等价于没换模型，不算改写；
//     没配映射也不算。
//
// 不成立时（客户自己把请求写错），行为与改造前完全一致：原样透传，不换号。
//
// mappingLookupModel 是「本次请求实际用于账号模型映射查表的模型名」。
// 它在多数路径上就是客户端请求的模型名；但 Anthropic Messages 入站路径上
// 映射用的是归一化后的名字（剥 reasoning 后缀等），那里的查表键是归一化结果。
// 传错会导致误判，调用点应与实际查表处保持同一个变量。
func IsMappingInducedRejection(account *Account, mappingLookupModel string, statusCode int) bool {
	if account == nil {
		return false
	}
	if statusCode < 400 || statusCode >= 500 {
		return false
	}
	lookup := strings.TrimSpace(mappingLookupModel)
	if lookup == "" {
		return false
	}
	mapped, matched := account.ResolveMappedModel(lookup)
	if !matched {
		return false
	}
	return strings.TrimSpace(mapped) != lookup
}

// modelMappingInducedRejectionReason 标记「上游请求类拒绝由模型名改写造成」。
// 与客户端参数错误区分：责任在账号/上游侧，按账号级故障处理——不停号、
// 不原地重试，只换下一个账号；换号耗尽时按上游不可用告知客户端，
// 不让客户去修一个我们自己改写出来的问题。
const modelMappingInducedRejectionReason = GatewayFailureReason("model_mapping_induced_rejection")

// applyMappingInducedRejection 把「改写归因」的换号语义写进错误对象。
//
// 与既有 isOpenAIUpstreamSafetyRejection 同款处置：换号、不停号、不原地重试。
//
// **刻意不覆盖客户端可见状态码与文案**（外审 must_fix #1 的整改）：
// 上游拒绝的原因无法从错误体可靠区分——讯飞 10005「用户参数值有错误」既可能是
// 客户参数写错，也可能是目标模型不认这个请求。若把 400 改写成 502「上游服务
// 暂时不可用」，客户自己的参数错误就会被说成「上游挂了」，那是误导，且违反锁定项
// 「不要把客户自己的错丢给客户」。
//
// 因此本机制的边界划在「要不要换号」这一侧：
//   - 换号成功 → 客户拿到正常响应（改造前是 400，这是本轮的全部收益）；
//   - 换号也失败 → 候选耗尽，**仍透传上游原始 400**，与改造前逐字节一致。
//
// 误判代价（客户请求本身有错时多换几次号、多几次上游调用）是有界且可接受的；
// 误判成「上游不可用」则是不可接受的误导。
func applyMappingInducedRejection(failoverErr *UpstreamFailoverError, mappingInduced bool) {
	if failoverErr == nil || !mappingInduced {
		return
	}
	failoverErr.RetryableOnSameAccount = false
	failoverErr.RequestScopedTransient = false
	failoverErr.Scope = GatewayFailureScopeAccount
	failoverErr.Reason = modelMappingInducedRejectionReason
	failoverErr.NextAccountAction = NextAccountRetry
}
