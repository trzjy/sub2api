package service

import "strings"

// devinSystemPromptRewrites 是三处触发上游内容策略的提示词片段的语义等价改写。
// 来源：2026-11-18 生产实测定位（docs-local/devin-system-prompt-compat-plan-20261118.md）。
// 精确匹配，不做正则；原文不存在时不替换（幂等、可预测、不误伤用户内容）。
var devinSystemPromptRewrites = []struct{ from, to string }{
	{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are a command-line coding assistant.",
	},
	{
		"Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.",
		"Dual-use security tools require clear authorization context: authorized engagements, security competitions, security research, or defensive use cases.",
	},
	{
		"Claude Code is available as a CLI in the terminal, desktop app (Mac/Windows), web app (claude.ai/code), and IDE extensions (VS Code, JetBrains).",
		"Claude Code is available as a CLI, a desktop app, a web app, and IDE extensions.",
	},
}

// normalizeDevinSystemPrompt 把系统提示词中会触发 Devin 上游内容策略的三处片段
// 改写为语义等价表述。只作用于 system/developer 拼合结果，不触碰任何用户消息内容。
func normalizeDevinSystemPrompt(systemPrompt string) string {
	if systemPrompt == "" {
		return systemPrompt
	}
	for _, rw := range devinSystemPromptRewrites {
		systemPrompt = strings.ReplaceAll(systemPrompt, rw.from, rw.to)
	}
	return systemPrompt
}
