//go:build unit

package service

import (
	"strings"
	"testing"
)

func TestNormalizeDevinSystemPrompt(t *testing.T) {
	t.Run("each trigger is rewritten", func(t *testing.T) {
		cases := []struct{ from, to string }{
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
		for _, c := range cases {
			in := "prefix " + c.from + " suffix"
			got := normalizeDevinSystemPrompt(in)
			if !strings.Contains(got, c.to) {
				t.Errorf("expected rewritten text %q in %q", c.to, got)
			}
			if strings.Contains(got, c.from) {
				t.Errorf("original trigger %q still present in %q", c.from, got)
			}
		}
	})

	t.Run("unchanged when no trigger present", func(t *testing.T) {
		in := "You are a helpful assistant.\n\nAnswer concisely."
		if got := normalizeDevinSystemPrompt(in); got != in {
			t.Errorf("expected unchanged prompt, got %q", got)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		in := "You are Claude Code, Anthropic's official CLI for Claude. " +
			"Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases. " +
			"Claude Code is available as a CLI in the terminal, desktop app (Mac/Windows), web app (claude.ai/code), and IDE extensions (VS Code, JetBrains)."
		once := normalizeDevinSystemPrompt(in)
		twice := normalizeDevinSystemPrompt(once)
		if twice != once {
			t.Errorf("normalization not idempotent:\n once = %q\ntwice = %q", once, twice)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		if got := normalizeDevinSystemPrompt(""); got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})
}
