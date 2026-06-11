package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Context-usage injection — the context-pressure sibling of clock_inject.go.
//
// Gas City has canonical handoff machinery (`gc handoff`, the PreCompact
// auto-handoff, deployment handoff skills) but agents have no signal for WHEN
// to trigger it: a session cannot see its own context usage (the provider
// footer is rendered for humans only), so unmonitored agents run into context
// compaction by default — losing the deliberate wrap-up (durable notes, bead
// updates, clean seams) the handoff machinery exists to provide.
//
// This reads the provider hook input (UserPromptSubmit JSON on stdin carries
// transcript_path), computes the session's current context footprint from the
// last usage entry in the transcript, and injects ONE line of guidance —
// folded into the same single provider payload as the clock (see
// cmd_nudge.go), so JSON hook formats stay one valid document.
//
// THRESHOLD-GATED BY DESIGN — not an always-on countdown. Model-provider
// guidance (Anthropic, Claude Fable 5 migration notes) documents "context
// anxiety": a continuously visible remaining-context count induces premature
// wrap-up and unprompted session-splitting. Below the advisory threshold this
// injects NOTHING. Above it, the message is actionable ("steer toward a clean
// handoff point", "run your handoff process now") and explicitly tells the
// agent NOT to panic-stop at the advisory tier.
//
//	< advisory (default 60%)  : silent
//	advisory..urgent (60–80%) : plan toward a clean handoff point
//	> urgent (default 80%)    : trigger the canonical handoff now
//
// Knobs: GC_INJECT_CONTEXT=0|false|off disables; GC_CONTEXT_ADVISORY_PCT and
// GC_CONTEXT_URGENT_PCT override the thresholds; GC_CONTEXT_WINDOW_TOKENS
// overrides the context-window size when model-string detection is wrong.
// Fail-safe: any parse/read problem returns "" — never blocks a prompt.

// hookStdinInput is the subset of the provider hook JSON we need.
type hookStdinInput struct {
	TranscriptPath string `json:"transcript_path"`
}

// transcriptUsage is the usage block shape inside provider transcript entries.
type transcriptUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// contextInjectLine returns the context-usage guidance line for the session
// whose hook input JSON is in hookInput, or "" when disabled, below the
// advisory threshold, or on any error (fail-safe silent).
func contextInjectLine(hookInput []byte) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_INJECT_CONTEXT"))) {
	case "0", "false", "off":
		return ""
	}
	var in hookStdinInput
	if err := json.Unmarshal(hookInput, &in); err != nil || strings.TrimSpace(in.TranscriptPath) == "" {
		return ""
	}
	tokens, model, ok := lastTranscriptUsage(in.TranscriptPath)
	if !ok {
		return ""
	}
	return contextUsageMessage(tokens, contextWindowTokens(model))
}

// lastTranscriptUsage reads the tail of a provider transcript (JSONL) and
// returns the context footprint of the most recent usage entry: prompt-side
// input tokens + cache reads + cache writes ≈ current context size.
func lastTranscriptUsage(path string) (tokens int, model string, ok bool) {
	const tailBytes = 2 << 20 // last 2MiB is ample for the newest entries
	f, err := os.Open(path)   //nolint:gosec // path comes from the provider hook input
	if err != nil {
		return 0, "", false
	}
	defer f.Close() //nolint:errcheck // read-only
	if st, err := f.Stat(); err == nil && st.Size() > tailBytes {
		if _, err := f.Seek(st.Size()-tailBytes, io.SeekStart); err != nil {
			return 0, "", false
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, tailBytes))
	if err != nil {
		return 0, "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"usage"`) {
			continue
		}
		var entry struct {
			Message struct {
				Model string           `json:"model"`
				Usage *transcriptUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		if u.InputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
			continue
		}
		tokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		model = entry.Message.Model
		ok = true // keep scanning: we want the LAST qualifying entry
	}
	return tokens, model, ok
}

// contextWindowTokens resolves the model's context window. 1M-class models
// are detected from the model string; everything else gets a conservative
// 200k default. GC_CONTEXT_WINDOW_TOKENS overrides both.
func contextWindowTokens(model string) int {
	if v := strings.TrimSpace(os.Getenv("GC_CONTEXT_WINDOW_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	ml := strings.ToLower(model)
	if strings.Contains(ml, "[1m]") || strings.Contains(ml, "fable") || strings.Contains(ml, "mythos") {
		return 1_000_000
	}
	return 200_000
}

// contextUsageMessage renders the guidance line for tokens used of window, or
// "" below the advisory threshold.
func contextUsageMessage(tokens, window int) string {
	if window <= 0 {
		return ""
	}
	advisory := thresholdPct("GC_CONTEXT_ADVISORY_PCT", 60)
	urgent := thresholdPct("GC_CONTEXT_URGENT_PCT", 80)
	pct := 100 * float64(tokens) / float64(window)
	k := func(n int) string { return fmt.Sprintf("%dk", (n+500)/1000) }
	switch {
	case pct < float64(advisory):
		return ""
	case pct <= float64(urgent):
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%). Advisory: ample room remains — do NOT stop or split the session for this; steer current work toward a clean handoff point rather than opening new long-horizon tasks.\n",
			k(tokens), k(window), pct)
	default:
		return fmt.Sprintf(
			"Context usage: %s/%s (~%.0f%%) — HIGH. Finish or checkpoint the current task and run your handoff process now (durable notes + work-item updates) before context compaction does it for you. Do not start new work.\n",
			k(tokens), k(window), pct)
	}
}

func thresholdPct(env string, def int) int {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			return n
		}
	}
	return def
}

// readHookStdin returns the provider hook input JSON from stdin when stdin is
// a pipe (the hook invocation shape). Interactive/manual invocations (stdin is
// a terminal) return nil so the command never blocks waiting for input.
func readHookStdin() []byte {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return nil
	}
	return data
}
