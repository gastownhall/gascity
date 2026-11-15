package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestPrimeHookWarnsWhenItIsTheSoleCarrierOfALargePrompt covers ga-4k4zfk AC4.
// Claude Code inlines SessionStart hook output only up to about 10,000
// characters and spills the rest to a file, so a managed launch whose startup
// prompt reaches the session only through the hook silently loses most of it.
// `gc prime --hook` must leave one structured WARN naming the session and the
// byte size whenever it is about to emit such a prompt without a delivery
// marker, and stay quiet in every case where another mechanism carries it.
func TestPrimeHookWarnsWhenItIsTheSoleCarrierOfALargePrompt(t *testing.T) {
	const inlineLimit = 10_000
	cases := []struct {
		name        string
		hookMode    bool
		suppress    bool
		marker      string
		sessionName string
		promptLen   int
		wantWarn    bool
		wantSession string
	}{
		{name: "oversized prompt with no marker warns", hookMode: true, sessionName: "sess-1", promptLen: inlineLimit + 1, wantWarn: true, wantSession: "sess-1"},
		{name: "unset session name falls back to the agent name", hookMode: true, promptLen: inlineLimit + 1, wantWarn: true, wantSession: "agent"},
		{name: "prompt at the limit does not warn", hookMode: true, promptLen: inlineLimit},
		{name: "small prompt does not warn", hookMode: true, promptLen: 500},
		{name: "empty prompt does not warn", hookMode: true},
		{name: "suppressed prompt does not warn", hookMode: true, suppress: true, promptLen: inlineLimit + 1},
		{name: "marker set means the nudge carries it", hookMode: true, marker: "1", promptLen: inlineLimit + 1},
		{name: "plain gc prime output is not a hook payload", hookMode: false, promptLen: inlineLimit + 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := &captureHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(handler))
			t.Cleanup(func() { slog.SetDefault(prev) })
			t.Setenv("GC_SESSION_NAME", tc.sessionName)
			t.Setenv(startupPromptDeliveredEnv, tc.marker)

			prompt := repeatToBytes("x", tc.promptLen)
			writePrimePromptWithFormat(io.Discard, "city", "agent", prompt, tc.hookMode, "", tc.suppress, "", nil)

			var warns []slog.Record
			for _, rec := range handler.records {
				if rec.Level == slog.LevelWarn {
					warns = append(warns, rec)
				}
			}
			if !tc.wantWarn {
				if len(warns) != 0 {
					t.Fatalf("got %d WARN records, want none: %v", len(warns), warns)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("got %d WARN records, want exactly 1", len(warns))
			}
			fields := recordFields(warns[0])
			if got := fields["session"].String(); got != tc.wantSession {
				t.Errorf("session = %q, want %q", got, tc.wantSession)
			}
			if got := fields["bytes"].Int64(); got != int64(tc.promptLen) {
				t.Errorf("bytes = %d, want %d", got, tc.promptLen)
			}
			if got := fields["limit"].Int64(); got != inlineLimit {
				t.Errorf("limit = %d, want %d", got, inlineLimit)
			}
			if strings.Contains(warns[0].Message, prompt) {
				t.Errorf("WARN message leaks prompt content")
			}
		})
	}
}
