package service

// Cursor 通道统一字段表（CARD-01 阶段 1：判据收口）。
//
// 本文件只建立统一字段表与唯一查询入口 cursorFieldClassOf，把散落在多处维护的
// exec 字段 / interaction 字段 / 原生拒接字段 / 已实现子字段白名单收口到一张表。
// 只换判据来源、不改行为：本文件不接入任何派发 switch，也不修改既有文件
// （cursor_agent_duplex.go / cursor_native_exec_reject.go 等在本单为禁区）。
//
// 字段号常量直接引用既有文件的声明，避免重复定义导致"两套真相"：
//   - 原生拒接字段号：cursorFldNativeReject*（cursor_native_exec_reject.go）
//   - 交互询问字段号：cursorFldInteraction*（cursor_agent_duplex.go）
//   - exec 元数据字段号：cursorFldExecMeta*（cursor_agent_duplex.go）

// cursorMessageDomain 标识 Cursor 上行消息所处的消息域。
type cursorMessageDomain int

const (
	// cursorDomainExec 对应 AgentServerMessage.f2 ExecServerMessage。
	cursorDomainExec cursorMessageDomain = iota
	// cursorDomainInteraction 对应 AgentServerMessage.f7 InteractionQuery。
	cursorDomainInteraction
)

// cursorFieldClass 是字段的分类。
type cursorFieldClass int

const (
	// cursorFieldUnknown 未知字段（阶段 1 维持 502）。
	cursorFieldUnknown cursorFieldClass = iota
	// cursorFieldMetadata 可忽略元数据（f1, f15, f19, f55, f57）。
	cursorFieldMetadata
	// cursorFieldHandler 专门处理的委托字段（f10 request_context, f36 mcp_state, f11 mcp_args）。
	cursorFieldHandler
	// cursorFieldNative 11 类已裁定原生工具字段。
	cursorFieldNative
	// cursorFieldInteraction 10 类已知交互询问类型。
	cursorFieldInteraction
)

// cursorExecNativeRejectOrder 按既有优先级顺序列出 11 类原生字段号。
// 直接引用 cursor_native_exec_reject.go 中声明的常量（与 cursorNativeExecRejectField 同序）。
var cursorExecNativeRejectOrder = []int{
	cursorFldNativeRejectShellArgs,
	cursorFldNativeRejectWriteArgs,
	cursorFldNativeRejectDeleteArgs,
	cursorFldNativeRejectGrepArgs,
	cursorFldNativeRejectReadArgs,
	cursorFldNativeRejectLsArgs,
	cursorFldNativeRejectDiagnosticsArgs,
	cursorFldNativeRejectShellStreamArgs,
	cursorFldNativeRejectBackgroundShellSpawnArgs,
	cursorFldNativeRejectFetchArgs,
	cursorFldNativeRejectWriteShellStdinArgs,
}

// cursorInteractionQueryOrder 列出 10 类交互询问字段号。
// 直接引用 cursor_agent_duplex.go 中声明的常量。
var cursorInteractionQueryOrder = []int{
	cursorFldInteractionWebSearch,
	cursorFldInteractionAskQuestion,
	cursorFldInteractionSwitchMode,
	cursorFldInteractionCreatePlan,
	cursorFldInteractionSetupVM,
	cursorFldInteractionWebFetch,
	cursorFldInteractionPrManagement,
	cursorFldInteractionMcpAuth,
	cursorFldInteractionReplaceEnv,
	cursorFldInteractionConnectScm,
}

// cursorFieldKey 是字段表的复合键：(消息域, 字段号)。
type cursorFieldKey struct {
	domain cursorMessageDomain
	number int
}

// cursorFieldClassTable 把 (消息域, 字段号) 映射到分类。
//
// ExecServerMessage 域承载：11 类原生工具字段、可忽略元数据、委托字段（handler）。
// InteractionQuery 域承载：10 类已知交互询问类型。
// 未在本表登记的字段一律归为 cursorFieldUnknown。
//
// 表由下面几份「类别清单」派生，不逐条手写 map 项：每个类别只有一份清单。
// 手写 map 项与清单并存就会出现两份数据，改一处漏一处 → 分类与实际字段对不上
// （既有代码就曾因两份清单漂移漏掉过 custom 子工具）。
// 优先级顺序（原生 11 类的拒绝形状顺序）也由同一批清单承载，
// 供 cursorNativeExecRejectField 取「第一个命中的字段号」用 —— 顺序与登记同源。
var cursorFieldClassTable = func() map[cursorFieldKey]cursorFieldClass {
	table := make(map[cursorFieldKey]cursorFieldClass)

	// ExecServerMessage 域：11 类原生工具字段（清单顺序即拒绝形状的优先级顺序）。
	for _, n := range cursorExecNativeRejectOrder {
		table[cursorFieldKey{domain: cursorDomainExec, number: n}] = cursorFieldNative
	}

	// ExecServerMessage 域：5 类可忽略元数据（f1 id / f15 exec_id / f19 span_context /
	// f55 accept_hook / f57 machine_id）。
	for _, n := range cursorExecMetadataOrder {
		table[cursorFieldKey{domain: cursorDomainExec, number: n}] = cursorFieldMetadata
	}

	// ExecServerMessage 域：3 类委托字段（f10 request_context / f36 mcp_state /
	// f11 mcp_args）。它们由专门的处理分支应答，既不是元数据也不是原生工具。
	for _, n := range cursorExecHandlerOrder {
		table[cursorFieldKey{domain: cursorDomainExec, number: n}] = cursorFieldHandler
	}

	// InteractionQuery 域：10 类已知交互询问类型。
	for _, n := range cursorInteractionQueryOrder {
		table[cursorFieldKey{domain: cursorDomainInteraction, number: n}] = cursorFieldInteraction
	}
	return table
}()

// cursorExecMetadataOrder 列出 5 类可忽略的 exec 元数据字段号。
// 直接引用 cursor_agent_duplex.go 中声明的常量。
var cursorExecMetadataOrder = []int{
	cursorFldExecMetaID,
	cursorFldExecMetaExecID,
	cursorFldExecMetaSpanContext,
	cursorFldExecMetaAcceptHook,
	cursorFldExecMetaMachineID,
}

// cursorExecHandlerOrder 列出 3 类委托字段号：f10 request_context / f36 mcp_state /
// f11 mcp_args。
var cursorExecHandlerOrder = []int{10, 36, 11}

// cursorFieldClassOf 是唯一对外的字段分类查询入口。
//
// 查表命中返回对应分类；未命中（含未知消息域）返回 cursorFieldUnknown。
func cursorFieldClassOf(domain cursorMessageDomain, number int) cursorFieldClass {
	if c, ok := cursorFieldClassTable[cursorFieldKey{domain: domain, number: number}]; ok {
		return c
	}
	return cursorFieldUnknown
}
