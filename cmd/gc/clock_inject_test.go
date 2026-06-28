package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestFormatClockLine(t *testing.T) {
	if _, err := time.LoadLocation("America/Los_Angeles"); err != nil {
		t.Skip("no tzdata for America/Los_Angeles")
	}
	t.Setenv("GC_OPERATOR_TZ", "America/Los_Angeles")
	now := time.Date(2026, 6, 3, 21, 23, 13, 0, time.UTC) // 2:23 PM PDT
	got := formatClockLine(now)
	for _, want := range []string{
		"Current time: ",
		"Wed 2026-06-03 2:23PM PDT",
		"2026-06-03 21:23Z UTC",
		fmt.Sprintf("(epoch %d)", now.Unix()),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatClockLine() = %q, missing %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("formatClockLine should end with newline, got %q", got)
	}
}

func TestFormatClockLineInvalidTZFallsBackToLocal(t *testing.T) {
	t.Setenv("GC_OPERATOR_TZ", "Not/AZone")
	now := time.Date(2026, 6, 3, 21, 23, 13, 0, time.UTC)
	got := formatClockLine(now) // must not panic; UTC + epoch still present
	if !strings.Contains(got, "2026-06-03 21:23Z UTC") ||
		!strings.Contains(got, fmt.Sprintf("epoch %d", now.Unix())) {
		t.Errorf("fallback render wrong: %q", got)
	}
}

func TestClockInjectPrefixClaude(t *testing.T) {
	t.Setenv("GC_INJECT_CLOCK", "")
	var buf bytes.Buffer
	if line := clockInjectLine(); line != "" {
		_ = writeProviderHookContextForEvent(&buf, "", "UserPromptSubmit", line)
	}
	if !strings.Contains(buf.String(), "Current time: ") {
		t.Errorf("clock inject prefix (claude) should emit a clock line, got %q", buf.String())
	}
}

func TestClockInjectPrefixDisabled(t *testing.T) {
	t.Setenv("GC_INJECT_CLOCK", "0")
	if line := clockInjectLine(); line != "" {
		t.Errorf("clock inject disabled should produce no line, got %q", line)
	}
}

func TestClockInjectPrefixCodexIsJSON(t *testing.T) {
	t.Setenv("GC_INJECT_CLOCK", "")
	var buf bytes.Buffer
	if line := clockInjectLine(); line != "" {
		_ = writeProviderHookContextForEvent(&buf, "codex", "UserPromptSubmit", line)
	}
	s := buf.String()
	if !strings.Contains(s, "hookSpecificOutput") || !strings.Contains(s, "Current time:") {
		t.Errorf("codex inject prefix should be a JSON hook document with the clock line, got %q", s)
	}
}

// TestCmdNudgeDrainInjectClockAndNudgeSingleJSONDocument is the combined-path
// regression: when a nudge fires alongside the clock under a JSON hook format,
// stdout must be exactly one JSON document carrying both the clock line and the
// nudge content in additionalContext — not two concatenated objects.
func TestCmdNudgeDrainInjectClockAndNudgeSingleJSONDocument(t *testing.T) {
	for _, hookFormat := range []string{"codex", "gemini"} {
		t.Run(hookFormat, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_INJECT_CLOCK", "")

			cityDir := t.TempDir()
			writeNamedSessionCityTOML(t, cityDir)
			t.Setenv("GC_CITY", cityDir)

			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("openCityStoreAt: %v", err)
			}
			created, err := store.Create(beads.Bead{
				Title:  "Session: worker",
				Type:   session.BeadType,
				Status: "open",
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "worker-session",
					"agent_name":   "worker",
					"template":     "worker",
					"state":        string(session.StateActive),
				},
			})
			if err != nil {
				t.Fatalf("store.Create session: %v", err)
			}

			item := newQueuedNudgeWithOptions("worker", "check hook output", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
				SessionID: created.ID,
			})
			if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
				t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
			}

			var stdout, stderr bytes.Buffer
			code := cmdNudgeDrainWithFormat([]string{created.ID}, true, hookFormat, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%s", code, stderr.String())
			}

			// Exactly one JSON value on stdout — no concatenated documents.
			dec := json.NewDecoder(&stdout)
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("decode first JSON document: %v", err)
			}
			if dec.More() {
				t.Fatalf("stdout has more than one JSON document for %s format", hookFormat)
			}

			hook, ok := doc["hookSpecificOutput"].(map[string]any)
			if !ok {
				t.Fatalf("missing hookSpecificOutput object, got %#v", doc)
			}
			ctx, ok := hook["additionalContext"].(string)
			if !ok {
				t.Fatalf("missing additionalContext string, got %#v", hook)
			}
			if !strings.Contains(ctx, "Current time:") {
				t.Errorf("additionalContext missing clock line, got %q", ctx)
			}
			if !strings.Contains(ctx, "check hook output") {
				t.Errorf("additionalContext missing nudge content, got %q", ctx)
			}
		})
	}
}
