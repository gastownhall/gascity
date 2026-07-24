package runtime

import "strings"

// Coarse reason tokens for recoverable provider transport failures. They double
// as the dedup fingerprint for the recovery state machine (ga-qox): one failure
// episode keeps a single reason across reconcile ticks so retries stay bounded,
// while a genuinely different failure class re-arms recovery.
const (
	// transportFailureDNSLookup is a name-resolution failure — Node's ENOTFOUND
	// (Claude) or the Rust std/reqwest "failed to lookup address information"
	// (Codex).
	transportFailureDNSLookup = "dns_lookup_failure"
	// transportFailureStreamDisconnected is Codex's "stream disconnected before
	// completion" — the provider stream dropped mid-response.
	transportFailureStreamDisconnected = "stream_disconnected"
	// transportFailureWSFallback is Codex falling back from WebSockets to HTTPS
	// after a transport timeout.
	transportFailureWSFallback = "transport_ws_fallback"
	// transportFailureRequestSend is the reqwest "error sending request for url"
	// transport error.
	transportFailureRequestSend = "request_send_failure"
)

// transportFailureTailLines bounds how close to the current prompt a transport
// failure must appear to count as the turn's final output. A tight recent
// suffix plus terminal-block matching keeps pasted, scrolled-back, or
// successfully-retried error text from false-triggering recovery: only a
// live-turn failure immediately preceding the current idle prompt qualifies
// (ga-qox forensic guidance).
const transportFailureTailLines = 20

// ClassifyInertTransportFailure reports whether pane content shows a provider
// transport failure that aborted the turn and left the session inert at its
// prompt, plus a stable coarse reason token for dedup and telemetry.
//
// recoverable is true only when a high-confidence, provider-neutral
// transport-failure block (Codex WebSockets→HTTPS fallback timeout / HTTPS
// stream disconnect, or a DNS lookup failure such as Claude's ENOTFOUND)
// immediately precedes the current idle prompt in the recent pane tail.
//
// Requiring the prompt after the terminal block means a prompt retained from a
// prior turn cannot make a currently-active retry look idle. Requiring the
// immediately preceding block means a warning followed by a successful HTTPS
// fallback, or any historical/pasted error followed by healthy output, cannot
// false-trigger. The classifier is pure and provider-neutral — it matches
// provider error strings, never a role or a provider name in Go control flow.
func ClassifyInertTransportFailure(content string) (recoverable bool, reason string) {
	tail := recentTail(content, transportFailureTailLines)
	if tail == "" {
		return false, ""
	}
	terminalBlock := terminalOutputBlockBeforePrompt(tail)
	if terminalBlock == "" {
		return false, ""
	}
	for _, line := range strings.Split(terminalBlock, "\n") {
		if !isTransportFailureBlockLine(line) {
			return false, ""
		}
	}
	reason = transportFailureReason(terminalBlock)
	return reason != "", reason
}

// transportFailureReason returns the coarse reason token for the highest-signal
// transport failure present in content, or "" if none. Precedence surfaces the
// most actionable root cause first: a name-resolution failure is reported over
// the downstream stream/transport symptoms it triggers.
func transportFailureReason(content string) string {
	// capture-pane can split a long terminal message across physical lines.
	// Collapse whitespace before matching so a wrapped
	// "failed to lookup address information" retains its DNS fingerprint.
	content = strings.Join(strings.Fields(content), " ")
	switch {
	case containsDNSLookupFailure(content):
		return transportFailureDNSLookup
	case strings.Contains(content, "stream disconnected before completion"):
		return transportFailureStreamDisconnected
	case strings.Contains(content, "Falling back from WebSockets to HTTPS transport"):
		return transportFailureWSFallback
	case strings.Contains(content, "error sending request for url"):
		return transportFailureRequestSend
	default:
		return ""
	}
}

// terminalOutputBlockBeforePrompt returns the final contiguous output block
// immediately before the most recent prompt in content. Blank lines and TUI
// horizontal rules directly around the prompt are presentation chrome. A
// prompt that appears before newer output yields no terminal block after it,
// which is how an active turn with an old retained prompt is rejected.
func terminalOutputBlockBeforePrompt(content string) string {
	lines := strings.Split(content, "\n")
	prompt := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isAgentPromptIndicator(lines[i]) {
			prompt = i
			break
		}
	}
	if prompt < 0 {
		return ""
	}
	// The most recent prompt may belong to a completed failed turn while a newer
	// turn is already producing output below it. In that case the session is not
	// inert on the old error, even though the old error/prompt pair remains in
	// the bounded tail.
	if suffix := strings.Join(lines[prompt+1:], "\n"); transportFailureReason(suffix) != "" || containsTurnOutput(suffix) {
		return ""
	}

	i := prompt - 1
	for i >= 0 && isPromptSpacingOrChrome(lines[i]) {
		i--
	}
	if i < 0 {
		return ""
	}
	end := i
	for i >= 0 && !isPromptSpacingOrChrome(lines[i]) {
		i--
	}
	return strings.Join(lines[i+1:end+1], "\n")
}

// isAgentPromptIndicator is the narrow live-agent subset of the generic
// dialog prompt detector. Shell prompts ($/%/#) are deliberately excluded: if
// the provider process exited back to a shell, injecting a continuation nudge
// as shell input would not preserve or resume the agent conversation.
func isAgentPromptIndicator(line string) bool {
	trimmed := strings.ReplaceAll(line, "\u00a0", " ")
	trimmed = strings.TrimRight(trimmed, " \t")
	trimmed = stripLeadingBoxBorder(trimmed)
	for _, prefix := range []string{"❯", "›", ">"} {
		rest, ok := strings.CutPrefix(trimmed, prefix+" ")
		if trimmed == prefix || (ok && !isNumberedMenuRow(rest)) {
			return true
		}
	}
	return false
}

func containsTurnOutput(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\u00a0", " "))
		for _, prefix := range []string{"●", "•", "⏺", "⎿", "└", "·", "✶", "✻"} {
			if strings.HasPrefix(line, prefix) {
				return true
			}
		}
	}
	return false
}

// isTransportFailureBlockLine accepts only terminal error text and the small
// set of physical-line continuations produced when tmux wraps the known
// provider messages. Rejecting arbitrary lines in the terminal block is what
// distinguishes a failed turn from a warning followed by healthy output.
func isTransportFailureBlockLine(line string) bool {
	line = strings.TrimSpace(strings.ReplaceAll(line, "\u00a0", " "))
	if line == "" {
		return true
	}
	// Provider UIs prefix healthy assistant/tool output with these glyphs. An
	// exact error string quoted in a successful final explanation is evidence,
	// not a terminal provider error.
	for _, prefix := range []string{"●", "•", "⏺", "⎿", "└", "✓"} {
		if strings.HasPrefix(line, prefix) {
			return false
		}
	}
	if transportFailureReason(line) != "" {
		return true
	}
	lower := strings.ToLower(line)
	for _, fragment := range []string{
		"stream error:",
		"api error:",
		"request timed out",
		"getaddrinfo",
		"nodename nor servname",
		"servname provided",
		"or not known",
		"chatgpt.com/backend-api",
		"codex/responses",
		"api.anthropic.com",
	} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func isPromptSpacingOrChrome(line string) bool {
	trimmed := strings.TrimSpace(strings.ReplaceAll(line, "\u00a0", " "))
	if trimmed == "" {
		return true
	}
	for _, r := range trimmed {
		switch r {
		case '─', '━', '═', '┄', '┅', '┈', '┉', '╌', '╍', '-', '_':
			// Horizontal TUI rule.
		default:
			return false
		}
	}
	return true
}

// containsDNSLookupFailure detects a name-resolution failure across providers:
// Node's ENOTFOUND (Claude) and the Rust std/reqwest "failed to lookup address
// information" (Codex).
func containsDNSLookupFailure(content string) bool {
	return strings.Contains(content, "ENOTFOUND") ||
		strings.Contains(content, "failed to lookup address information")
}

// recentTail returns at most the last n lines of content, joined with newlines.
// It bounds a screen scan to the recent suffix so older scrollback cannot
// contribute to a match.
func recentTail(content string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
