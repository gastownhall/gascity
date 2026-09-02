package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestPromptDelivery(t *testing.T) {
	const prompt = "do the work"
	quoted := shellquote.Quote(prompt)

	tests := []struct {
		name    string
		promp   string
		isACP   bool
		rp      *config.ResolvedProvider
		nudge   string
		runtime string
		want    promptDeliveryResult
	}{
		{
			name:  "empty prompt delivers nothing",
			promp: "",
			nudge: "wake",
			want:  promptDeliveryResult{Nudge: "wake"},
		},
		{
			name:  "acp prepends to nudge",
			promp: prompt,
			isACP: true,
			nudge: "wake",
			want: promptDeliveryResult{
				Nudge:     prependStartupPromptToNudge(prompt, "wake"),
				Delivered: true,
			},
		},
		{
			name:  "prompt-mode none prepends to nudge",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "none"},
			nudge: "",
			want: promptDeliveryResult{
				Nudge:     prependStartupPromptToNudge(prompt, ""),
				Delivered: true,
			},
		},
		{
			name:  "default arg mode uses quoted suffix",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "arg"},
			nudge: "wake",
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				Nudge:        "wake",
				Delivered:    true,
			},
		},
		{
			name:  "nil provider defaults to quoted suffix",
			promp: prompt,
			rp:    nil,
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				Delivered:    true,
			},
		},
		{
			name:  "flag mode with flag sets both suffix and flag",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "flag", PromptFlag: "--prompt"},
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				PromptFlag:   "--prompt",
				Delivered:    true,
			},
		},
		{
			name:  "flag mode without flag is not delivered",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "flag"},
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				Delivered:    false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := promptDelivery(tt.promp, tt.isACP, tt.rp, tt.nudge, tt.runtime, nil)
			if err != nil {
				t.Fatalf("promptDelivery() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("promptDelivery() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// --- Oversized-prompt argv-safety guard (gastownhall/gascity ga-q8wgom.1.1) ---
//
// promptDelivery must never place a prompt into an OS exec() argv when that
// argv would risk MAX_ARG_STRLEN/E2BIG. The guard triggers when EITHER the
// raw prompt bytes reach maxPromptSuffixRawBytes OR the shellquote.Quote-encoded
// bytes reach maxPromptSuffixQuotedBytes (quoting can inflate size well past the
// raw length). Only arg/flag delivery (the `default:` branch, which is the only
// branch that ever populates PromptSuffix) is affected: ACP and prompt_mode=none
// already route through the nudge unconditionally and must stay untouched
// regardless of prompt size.

// repeatToBytes returns a string built by repeating unit until it reaches
// exactly n bytes, so callers can construct precise byte-length fixtures
// without hand-counting.
func repeatToBytes(unit string, n int) string {
	if len(unit) == 0 || n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n)
	for b.Len() < n {
		remaining := n - b.Len()
		if remaining >= len(unit) {
			b.WriteString(unit)
		} else {
			b.WriteString(unit[:remaining])
		}
	}
	return b.String()
}

func TestPromptDeliveryOversized(t *testing.T) {
	arg := &config.ResolvedProvider{PromptMode: "arg"}

	t.Run("just under both thresholds preserves existing argv behavior", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes-1)
		quoted := shellquote.Quote(prompt)
		if len(quoted) >= maxPromptSuffixQuotedBytes {
			t.Fatalf("fixture invalid: quoted len %d already at/above quoted threshold %d", len(quoted), maxPromptSuffixQuotedBytes)
		}
		got, err := promptDelivery(prompt, false, arg, "wake", "tmux", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error: %v", err)
		}
		want := promptDeliveryResult{PromptSuffix: quoted, Nudge: "wake", Delivered: true}
		if got != want {
			t.Errorf("promptDelivery() = %+v, want %+v", got, want)
		}
	})

	t.Run("raw bytes at threshold triggers guard even though quoted alone would not", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		if len(prompt) < maxPromptSuffixRawBytes {
			t.Fatalf("fixture invalid: raw len %d below raw threshold %d", len(prompt), maxPromptSuffixRawBytes)
		}
		got, err := promptDelivery(prompt, false, arg, "wake", "tmux", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error: %v", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" {
			t.Errorf("promptDelivery() must not place an oversized prompt in argv, got PromptSuffix len=%d PromptFlag=%q", len(got.PromptSuffix), got.PromptFlag)
		}
		if !got.Delivered || !got.OversizedFallback {
			t.Errorf("promptDelivery() = %+v, want Delivered=true OversizedFallback=true", got)
		}
		wantNudge := prependStartupPromptToNudge(prompt, "wake")
		if got.Nudge != wantNudge {
			t.Errorf("promptDelivery().Nudge = (len %d), want prepended nudge (len %d)", len(got.Nudge), len(wantNudge))
		}
	})

	t.Run("quoted bytes at threshold triggers guard even though raw alone would not", func(t *testing.T) {
		// Every embedded single-quote inflates 1 raw byte into 4 quoted bytes
		// ('\''), so a prompt of single-quote characters crosses the quoted
		// threshold at roughly a quarter of the byte count needed to cross the
		// raw threshold directly.
		prompt := repeatToBytes("'", maxPromptSuffixRawBytes/2)
		quoted := shellquote.Quote(prompt)
		if len(prompt) >= maxPromptSuffixRawBytes {
			t.Fatalf("fixture invalid: raw len %d already at/above raw threshold %d", len(prompt), maxPromptSuffixRawBytes)
		}
		if len(quoted) < maxPromptSuffixQuotedBytes {
			t.Fatalf("fixture invalid: quoted len %d below quoted threshold %d", len(quoted), maxPromptSuffixQuotedBytes)
		}
		got, err := promptDelivery(prompt, false, arg, "wake", "tmux", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error: %v", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" {
			t.Errorf("promptDelivery() must not place an oversized (quoted) prompt in argv, got PromptSuffix len=%d PromptFlag=%q", len(got.PromptSuffix), got.PromptFlag)
		}
		if !got.Delivered || !got.OversizedFallback {
			t.Errorf("promptDelivery() = %+v, want Delivered=true OversizedFallback=true", got)
		}
	})

	t.Run("multi-byte UTF-8 is counted in bytes, not runes", func(t *testing.T) {
		// U+1F389 PARTY POPPER is 4 bytes in UTF-8. 25001 repeats is only
		// 25001 runes but 100004 bytes -- over the raw byte threshold. A
		// rune-counting implementation would wrongly treat this as small.
		prompt := strings.Repeat("\U0001F389", 25001)
		if len(prompt) < maxPromptSuffixRawBytes {
			t.Fatalf("fixture invalid: expected >= %d raw bytes from UTF-8 repeats, got %d", maxPromptSuffixRawBytes, len(prompt))
		}
		got, err := promptDelivery(prompt, false, arg, "wake", "tmux", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error: %v", err)
		}
		if got.PromptSuffix != "" {
			t.Errorf("promptDelivery() must treat a 100KB+ UTF-8 prompt as oversized by byte length, got PromptSuffix len=%d", len(got.PromptSuffix))
		}
		if !got.OversizedFallback {
			t.Errorf("promptDelivery() OversizedFallback = false, want true for a byte-oversized UTF-8 prompt")
		}
	})

	t.Run("flag mode is also guarded", func(t *testing.T) {
		flag := &config.ResolvedProvider{PromptMode: "flag", PromptFlag: "--prompt"}
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		got, err := promptDelivery(prompt, false, flag, "wake", "tmux", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error: %v", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" {
			t.Errorf("promptDelivery() must not place an oversized flag-mode prompt in argv, got PromptSuffix len=%d PromptFlag=%q", len(got.PromptSuffix), got.PromptFlag)
		}
		if !got.OversizedFallback {
			t.Errorf("promptDelivery() OversizedFallback = false, want true for oversized flag mode")
		}
	})

	t.Run("ACP is unaffected by size", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes*2)
		got, err := promptDelivery(prompt, true, arg, "wake", "subprocess", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for ACP: %v", err)
		}
		want := promptDeliveryResult{Nudge: prependStartupPromptToNudge(prompt, "wake"), Delivered: true}
		if got != want {
			t.Errorf("promptDelivery() ACP oversized = %+v, want %+v (byte lengths only)", promptDeliveryResultLens(got), promptDeliveryResultLens(want))
		}
		if got.OversizedFallback {
			t.Errorf("promptDelivery() OversizedFallback = true for ACP, want false: ACP was never argv-bound so this isn't a size-triggered fallback")
		}
	})

	t.Run("prompt_mode none is unaffected by size", func(t *testing.T) {
		none := &config.ResolvedProvider{PromptMode: "none"}
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes*2)
		got, err := promptDelivery(prompt, false, none, "wake", "subprocess", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for prompt_mode=none: %v", err)
		}
		if got.OversizedFallback {
			t.Errorf("promptDelivery() OversizedFallback = true for prompt_mode=none, want false")
		}
		if !got.Delivered {
			t.Errorf("promptDelivery() Delivered = false for prompt_mode=none oversized, want true")
		}
	})

	t.Run("argv-safe runtime (t3bridge) is unaffected by size", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes*2)
		quoted := shellquote.Quote(prompt)
		got, err := promptDelivery(prompt, false, arg, "wake", "t3bridge", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for t3bridge: %v", err)
		}
		want := promptDeliveryResult{PromptSuffix: quoted, Nudge: "wake", Delivered: true}
		if got != want {
			t.Errorf("promptDelivery() t3bridge oversized: PromptSuffix len=%d Delivered=%v OversizedFallback=%v, want PromptSuffix len=%d Delivered=true OversizedFallback=false",
				len(got.PromptSuffix), got.Delivered, got.OversizedFallback, len(want.PromptSuffix))
		}
	})

	t.Run("nudge-fallback runtime (tmux) routes through nudge with no argv bytes", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		got, err := promptDelivery(prompt, false, arg, "wake", "tmux", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for tmux: %v", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" {
			t.Errorf("promptDelivery() tmux oversized must carry zero argv bytes, got PromptSuffix len=%d PromptFlag=%q", len(got.PromptSuffix), got.PromptFlag)
		}
		if !got.Delivered || !got.OversizedFallback {
			t.Errorf("promptDelivery() tmux oversized = %+v, want Delivered=true OversizedFallback=true", got)
		}
	})

	t.Run("empty runtime name (stock default) routes through nudge, not hard-fail", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		got, err := promptDelivery(prompt, false, arg, "wake", "", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for the stock default (empty) runtime name: %v", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" {
			t.Errorf("promptDelivery() default-runtime oversized must carry zero argv bytes, got PromptSuffix len=%d PromptFlag=%q", len(got.PromptSuffix), got.PromptFlag)
		}
		if !got.Delivered || !got.OversizedFallback {
			t.Errorf("promptDelivery() default-runtime oversized = %+v, want Delivered=true OversizedFallback=true", promptDeliveryResultLens(got))
		}
	})

	t.Run("legacy t3bridge exec spelling stays argv-safe", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		quoted := shellquote.Quote(prompt)
		got, err := promptDelivery(prompt, false, arg, "wake", "exec:/usr/lib/gc/gc-session-t3", nil)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for the legacy t3bridge exec spelling: %v", err)
		}
		if got.PromptSuffix != quoted {
			t.Errorf("promptDelivery() legacy t3bridge exec spelling PromptSuffix len=%d, want len=%d", len(got.PromptSuffix), len(quoted))
		}
		if got.OversizedFallback {
			t.Errorf("promptDelivery() OversizedFallback = true for the legacy t3bridge exec spelling, want false: it is the same argv-safe native runtime as %q", "t3bridge")
		}
	})

	t.Run("pack-declared nudge-fallback runtime routes through nudge with no argv bytes", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		packRuntimes := map[string]config.DiscoveredRuntime{
			"acme-runtime": {Name: "acme-runtime", PromptDelivery: "nudge-fallback"},
		}
		got, err := promptDelivery(prompt, false, arg, "wake", "acme-runtime", packRuntimes)
		if err != nil {
			t.Fatalf("promptDelivery() unexpected error for a pack-declared nudge-fallback runtime: %v", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" {
			t.Errorf("promptDelivery() pack-declared nudge-fallback oversized must carry zero argv bytes, got PromptSuffix len=%d PromptFlag=%q", len(got.PromptSuffix), got.PromptFlag)
		}
		if !got.Delivered || !got.OversizedFallback {
			t.Errorf("promptDelivery() pack-declared nudge-fallback oversized = %+v, want Delivered=true OversizedFallback=true", promptDeliveryResultLens(got))
		}
	})

	t.Run("pack-declared runtime without prompt_delivery still hard-fails", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		packRuntimes := map[string]config.DiscoveredRuntime{
			"acme-runtime": {Name: "acme-runtime"},
		}
		_, err := promptDelivery(prompt, false, arg, "wake", "acme-runtime", packRuntimes)
		if err == nil {
			t.Fatalf("promptDelivery() error = nil, want a hard-fail error for a pack runtime with no prompt_delivery declared")
		}
		if !errors.Is(err, errOversizedPromptUnsupportedRuntime) {
			t.Errorf("promptDelivery() error = %v, want errors.Is(err, errOversizedPromptUnsupportedRuntime)", err)
		}
	})

	t.Run("unsupported runtime (subprocess) hard-fails before Start", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		got, err := promptDelivery(prompt, false, arg, "wake", "subprocess", nil)
		if err == nil {
			t.Fatalf("promptDelivery() error = nil, want a hard-fail error for subprocess with an oversized prompt")
		}
		if !errors.Is(err, errOversizedPromptUnsupportedRuntime) {
			t.Errorf("promptDelivery() error = %v, want errors.Is(err, errOversizedPromptUnsupportedRuntime)", err)
		}
		if got.PromptSuffix != "" || got.PromptFlag != "" || got.Delivered {
			t.Errorf("promptDelivery() must return a zero-value result alongside the hard-fail error, got %+v", got)
		}
	})

	t.Run("unknown/unclassified runtime name defaults to hard-fail, not silent argv passthrough", func(t *testing.T) {
		prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
		_, err := promptDelivery(prompt, false, arg, "wake", "some-future-custom-runtime", nil)
		if err == nil {
			t.Fatalf("promptDelivery() error = nil, want a hard-fail error for an unclassified runtime with an oversized prompt")
		}
		if !errors.Is(err, errOversizedPromptUnsupportedRuntime) {
			t.Errorf("promptDelivery() error = %v, want errors.Is(err, errOversizedPromptUnsupportedRuntime)", err)
		}
	})

	t.Run("hard-fail error is actionable but never contains prompt content", func(t *testing.T) {
		const marker = "TESTMARKERMUSTNOTLEAK"
		prompt := marker + repeatToBytes("a", maxPromptSuffixRawBytes)
		quoted := shellquote.Quote(prompt)
		_, err := promptDelivery(prompt, false, arg, "wake", "subprocess", nil)
		if err == nil {
			t.Fatalf("promptDelivery() error = nil, want hard-fail error")
		}
		msg := err.Error()
		if strings.Contains(msg, marker) {
			t.Errorf("promptDelivery() error leaks prompt content: %q", msg)
		}
		for _, want := range []string{"subprocess", "100000", "128000"} {
			if !strings.Contains(msg, want) {
				t.Errorf("promptDelivery() error %q missing expected actionable detail %q", msg, want)
			}
		}
		_ = quoted // byte counts, not content, belong in the message
	})
}

func TestPromptDeliverySupportFor(t *testing.T) {
	tests := []struct {
		runtime  string
		runtimes map[string]config.DiscoveredRuntime
		want     promptDeliverySupport
	}{
		{runtime: "tmux", want: promptDeliverySupportNudgeFallback},
		{runtime: "t3bridge", want: promptDeliverySupportArgvSafe},
		{runtime: "exec:/usr/lib/gc/gc-session-t3", want: promptDeliverySupportArgvSafe},
		{runtime: "subprocess", want: promptDeliverySupportUnsupported},
		{runtime: "", want: promptDeliverySupportNudgeFallback},
		{runtime: " tmux", want: promptDeliverySupportNudgeFallback},
		{runtime: "herdr", want: promptDeliverySupportNudgeFallback},
		{runtime: "k8s", want: promptDeliverySupportNudgeFallback},
		{runtime: "exec:./run.sh", want: promptDeliverySupportUnsupported},
		{runtime: "ssh:host", want: promptDeliverySupportUnsupported},
		{runtime: "hybrid", want: promptDeliverySupportNudgeFallback},
		{runtime: "auto", want: promptDeliverySupportUnsupported},
		{runtime: "totally-unknown-custom-runtime", want: promptDeliverySupportUnsupported},
		{
			runtime:  "acme-runtime",
			runtimes: map[string]config.DiscoveredRuntime{"acme-runtime": {Name: "acme-runtime", PromptDelivery: "nudge-fallback"}},
			want:     promptDeliverySupportNudgeFallback,
		},
		{
			runtime:  " acme-runtime",
			runtimes: map[string]config.DiscoveredRuntime{"acme-runtime": {Name: "acme-runtime", PromptDelivery: "nudge-fallback"}},
			want:     promptDeliverySupportNudgeFallback,
		},
		{
			runtime:  "acme-runtime-unset",
			runtimes: map[string]config.DiscoveredRuntime{"acme-runtime-unset": {Name: "acme-runtime-unset"}},
			want:     promptDeliverySupportUnsupported,
		},
		{
			runtime:  "acme-runtime-missing",
			runtimes: map[string]config.DiscoveredRuntime{"other-runtime": {Name: "other-runtime", PromptDelivery: "nudge-fallback"}},
			want:     promptDeliverySupportUnsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.runtime, func(t *testing.T) {
			if got := promptDeliverySupportFor(tt.runtime, tt.runtimes); got != tt.want {
				t.Errorf("promptDeliverySupportFor(%q) = %v, want %v", tt.runtime, got, tt.want)
			}
		})
	}
	// The zero value must be the safe (hard-fail) classification: a future
	// bug that leaves this unset must fail loud, never silently allow an
	// oversized prompt into argv.
	var zero promptDeliverySupport
	if zero != promptDeliverySupportUnsupported {
		t.Errorf("promptDeliverySupport zero value = %v, want promptDeliverySupportUnsupported (fail-safe default)", zero)
	}
}

// promptDeliveryResultLens renders a promptDeliveryResult with its string
// fields replaced by byte lengths, for diagnostics that must not print
// prompt content (which can be 100KB+) into test failure output.
func promptDeliveryResultLens(r promptDeliveryResult) string {
	return strings.Join([]string{
		"PromptSuffixLen=" + strconv.Itoa(len(r.PromptSuffix)),
		"PromptFlag=" + r.PromptFlag,
		"NudgeLen=" + strconv.Itoa(len(r.Nudge)),
		"Delivered=" + strconv.FormatBool(r.Delivered),
		"OversizedFallback=" + strconv.FormatBool(r.OversizedFallback),
	}, " ")
}
