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
var cursorFieldClassTable = map[cursorFieldKey]cursorFieldClass{
	// --- ExecServerMessage 域：11 类原生工具字段 ---
	{domain: cursorDomainExec, number: cursorFldNativeRejectShellArgs}:                cursorFieldNative, // f2  shell_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectWriteArgs}:                cursorFieldNative, // f3  write_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectDeleteArgs}:               cursorFieldNative, // f4  delete_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectGrepArgs}:                 cursorFieldNative, // f5  grep_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectReadArgs}:                 cursorFieldNative, // f7  read_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectLsArgs}:                   cursorFieldNative, // f8  ls_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectDiagnosticsArgs}:          cursorFieldNative, // f9  diagnostics_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectShellStreamArgs}:          cursorFieldNative, // f14 shell_stream_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectBackgroundShellSpawnArgs}: cursorFieldNative, // f16 background_shell_spawn_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectFetchArgs}:                cursorFieldNative, // f20 fetch_args
	{domain: cursorDomainExec, number: cursorFldNativeRejectWriteShellStdinArgs}:      cursorFieldNative, // f23 write_shell_stdin_args

	// --- ExecServerMessage 域：可忽略元数据 ---
	{domain: cursorDomainExec, number: cursorFldExecMetaID}:          cursorFieldMetadata, // f1  id
	{domain: cursorDomainExec, number: cursorFldExecMetaExecID}:      cursorFieldMetadata, // f15 exec_id
	{domain: cursorDomainExec, number: cursorFldExecMetaSpanContext}: cursorFieldMetadata, // f19 span_context
	{domain: cursorDomainExec, number: cursorFldExecMetaAcceptHook}:  cursorFieldMetadata, // f55 accept_hook_additional_contexts
	{domain: cursorDomainExec, number: cursorFldExecMetaMachineID}:   cursorFieldMetadata, // f57 machine_id

	// --- ExecServerMessage 域：委托字段（handler）---
	{domain: cursorDomainExec, number: 10}: cursorFieldHandler, // f10 request_context
	{domain: cursorDomainExec, number: 36}: cursorFieldHandler, // f36 mcp_state
	{domain: cursorDomainExec, number: 11}: cursorFieldHandler, // f11 mcp_args

	// --- InteractionQuery 域：10 类已知交互询问类型 ---
	{domain: cursorDomainInteraction, number: cursorFldInteractionWebSearch}:    cursorFieldInteraction, // f2  web_search
	{domain: cursorDomainInteraction, number: cursorFldInteractionAskQuestion}:  cursorFieldInteraction, // f3  ask_question
	{domain: cursorDomainInteraction, number: cursorFldInteractionSwitchMode}:   cursorFieldInteraction, // f4  switch_mode
	{domain: cursorDomainInteraction, number: cursorFldInteractionCreatePlan}:   cursorFieldInteraction, // f7  create_plan
	{domain: cursorDomainInteraction, number: cursorFldInteractionSetupVM}:      cursorFieldInteraction, // f8  setup_vm_environment
	{domain: cursorDomainInteraction, number: cursorFldInteractionWebFetch}:     cursorFieldInteraction, // f9  web_fetch
	{domain: cursorDomainInteraction, number: cursorFldInteractionPrManagement}: cursorFieldInteraction, // f10 pr_management
	{domain: cursorDomainInteraction, number: cursorFldInteractionMcpAuth}:      cursorFieldInteraction, // f11 mcp_auth
	{domain: cursorDomainInteraction, number: cursorFldInteractionReplaceEnv}:   cursorFieldInteraction, // f13 replace_env
	{domain: cursorDomainInteraction, number: cursorFldInteractionConnectScm}:   cursorFieldInteraction, // f14 connect_scm
}

// cursorFieldClassOf 是唯一对外的字段分类查询入口。
//
// 查表命中返回对应分类；未命中（含未知消息域）返回 cursorFieldUnknown。
func cursorFieldClassOf(domain cursorMessageDomain, number int) cursorFieldClass {
	if c, ok := cursorFieldClassTable[cursorFieldKey{domain: domain, number: number}]; ok {
		return c
	}
	return cursorFieldUnknown
}
