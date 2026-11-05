//go:build !windows

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// guardDiscoveryFixture is a real SessionHandle whose transcript search paths
// are a Claude projects root plus a Codex sessions root, both under t.TempDir.
// HOME is redirected so the default roots can never reach the operator's own
// ~/.claude or ~/.codex.
type guardDiscoveryFixture struct {
	handle     *SessionHandle
	claudeRoot string
	codexRoot  string
	workDir    string
	sessionKey string
}

// newGuardDiscoveryFixture starts a session and then rewrites its recorded
// provider to provider and, when keyed is false, drops its session_key: the
// shape of an agent that declares only a start_command.
func newGuardDiscoveryFixture(t *testing.T, provider string, keyed bool) guardDiscoveryFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	claudeRoot := t.TempDir()
	codexRoot := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "agent-workdir")

	handle, store, _, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
	})
	handle.adapter.SearchPaths = []string{claudeRoot, codexRoot}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	setMeta := func(store *beads.MemStore, key, value string) {
		t.Helper()
		if err := store.SetMetadata(handle.sessionID, key, value); err != nil {
			t.Fatalf("SetMetadata(%s): %v", key, err)
		}
	}
	setMeta(store, "provider", provider)
	setMeta(store, "provider_kind", provider)
	if !keyed {
		setMeta(store, "session_key", "")
	}
	return guardDiscoveryFixture{
		handle:     handle,
		claudeRoot: claudeRoot,
		codexRoot:  codexRoot,
		workDir:    workDir,
		sessionKey: info.SessionKey,
	}
}

// writeLiveSubagentTranscript writes the Claude parent transcript at the slug
// path a Claude session uses, with one background Agent spawn that has no
// terminal notification, plus the subagent file that maps it.
func (f guardDiscoveryFixture) writeLiveSubagentTranscript(t *testing.T) {
	t.Helper()
	slugDir := filepath.Join(f.claudeRoot, sessionlog.ProjectSlug(f.workDir))
	subagents := filepath.Join(slugDir, f.sessionKey, "subagents")
	if err := os.MkdirAll(subagents, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", subagents, err)
	}
	parent := `{"uuid":"u1","type":"assistant","timestamp":"2026-06-12T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_live","name":"Agent","input":{"run_in_background":true,"description":"long task"}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(slugDir, f.sessionKey+".jsonl"), []byte(parent), 0o644); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	agent := `{"uuid":"a1","type":"system","parentToolUseId":"toolu_live"}` + "\n"
	if err := os.WriteFile(filepath.Join(subagents, "agent-live.jsonl"), []byte(agent), 0o644); err != nil {
		t.Fatalf("write agent transcript: %v", err)
	}
}

// writeCodexRollout writes one Codex rollout whose session_meta cwd is cwd.
func (f guardDiscoveryFixture) writeCodexRollout(t *testing.T, ts time.Time, cwd, uuid string) string {
	t.Helper()
	path := codexWorkerRolloutPathWithID(t, f.codexRoot, ts, uuid)
	writeWorkerTestJSONL(t, path, []map[string]any{codexWorkerSessionMeta(cwd), codexWorkerTurnContext()})
	return path
}

// assertNoLiveSubagents fails unless the guard found nothing to protect. A
// session with no usable transcript reports ErrHistoryUnavailable, which every
// kill caller treats as fail-open.
func assertNoLiveSubagents(t *testing.T, live []InFlightSubagent, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("InFlightBackgroundSubagents error = %v, want nil or ErrHistoryUnavailable", err)
	}
	if len(live) != 0 {
		t.Fatalf("live = %#v, want none", live)
	}
}

// guardRolloutDecoys is the size of the Codex tree the guard must not walk. It
// is the order of magnitude the reviewer of ga-5cokvb.1 measured, scaled down.
const guardRolloutDecoys = 200

// The guard reads Claude-format transcripts only, so a kill must never walk the
// Codex tree to find one. Every decoy rollout is a FIFO: opening one blocks
// until a writer appears, so a lookup that reaches it hangs and the test fails
// by timeout instead of passing quietly. A session with an empty or "auto"
// provider and no Claude transcript is what sent the generic finder down to the
// Codex scan (31,549 rollouts, about 20 s, in the measured kill).
func TestInFlightBackgroundSubagents_DoesNotOpenCodexRollouts(t *testing.T) {
	for _, provider := range []string{"", "auto"} {
		t.Run("provider="+provider, func(t *testing.T) {
			f := newGuardDiscoveryFixture(t, provider, false)

			dayDir := filepath.Join(f.codexRoot, "2026", "06", "12")
			if err := os.MkdirAll(dayDir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dayDir, err)
			}
			fifos := make([]string, 0, guardRolloutDecoys)
			for i := range guardRolloutDecoys {
				path := filepath.Join(dayDir, fmt.Sprintf("rollout-2026-06-12T10-00-00-decoy-%03d.jsonl", i))
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Skipf("mkfifo unsupported here: %v", err)
				}
				fifos = append(fifos, path)
			}

			type result struct {
				live []InFlightSubagent
				err  error
			}
			done := make(chan result, 1)
			go func() {
				live, err := InFlightBackgroundSubagents(context.Background(), f.handle)
				done <- result{live, err}
			}()
			// Release a lookup that is blocked on a decoy, or its goroutine
			// outlives the test. Opening the write end is what unblocks a reader.
			t.Cleanup(func() {
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					select {
					case <-done:
						return
					default:
					}
					for _, path := range fifos {
						if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
							_ = w.Close()
						}
					}
					time.Sleep(10 * time.Millisecond)
				}
			})

			select {
			case res := <-done:
				done <- res // let Cleanup see the lookup finished
				assertNoLiveSubagents(t, res.live, res.err)
			case <-time.After(5 * time.Second):
				t.Fatal("the guard's transcript lookup is blocked opening a Codex rollout: it must not enumerate the Codex tree")
			}
		})
	}
}

// The generic finder keeps its wider search for its other callers (gc session
// logs, telemetry, the API): the guard's narrowing must not leak into it. This
// is the contrast case for the test above, and proves the fixture's Codex
// rollout is one the generic auto lookup really does reach.
func TestSessionHandleTranscriptPath_GenericLookupStillFindsCodexRollout(t *testing.T) {
	f := newGuardDiscoveryFixture(t, "", false)
	want := f.writeCodexRollout(t, time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC), f.workDir, "019e0000-aaaa-7000-8000-000000000001")

	got, err := f.handle.TranscriptPath(context.Background())
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if got != want {
		t.Fatalf("TranscriptPath = %q, want the Codex rollout %q", got, want)
	}
}

// A matching Codex rollout is not evidence the guard can use: it parses Claude
// records only, so with no Claude transcript there is nothing to protect.
func TestInFlightBackgroundSubagents_IgnoresMatchingCodexRollout(t *testing.T) {
	f := newGuardDiscoveryFixture(t, "", false)
	f.writeCodexRollout(t, time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC), f.workDir, "019e0000-aaaa-7000-8000-000000000001")

	live, err := InFlightBackgroundSubagents(context.Background(), f.handle)
	assertNoLiveSubagents(t, live, err)
}

// A Claude session keeps its protection whatever provider string the session
// bead recorded, keyed or reached only by the work-directory slug.
func TestInFlightBackgroundSubagents_FindsClaudeTranscriptForEveryProviderShape(t *testing.T) {
	for _, provider := range []string{"claude", "", "auto"} {
		for _, keyed := range []bool{true, false} {
			t.Run(fmt.Sprintf("provider=%q/keyed=%t", provider, keyed), func(t *testing.T) {
				f := newGuardDiscoveryFixture(t, provider, keyed)
				f.writeLiveSubagentTranscript(t)

				live, err := InFlightBackgroundSubagents(context.Background(), f.handle)
				if err != nil {
					t.Fatalf("InFlightBackgroundSubagents: %v", err)
				}
				if len(live) != 1 || live[0].AgentID != "live" || !strings.Contains(live[0].Description, "long task") {
					t.Fatalf("live = %#v, want the one live background subagent", live)
				}
			})
		}
	}
}

// Nothing on disk at all is fail-open, exactly as before: no live subagents,
// and an error only of the kind every kill caller already tolerates.
func TestInFlightBackgroundSubagents_NoTranscriptIsFailOpen(t *testing.T) {
	for _, provider := range []string{"claude", "", "auto"} {
		t.Run("provider="+provider, func(t *testing.T) {
			f := newGuardDiscoveryFixture(t, provider, false)

			live, err := InFlightBackgroundSubagents(context.Background(), f.handle)
			assertNoLiveSubagents(t, live, err)
		})
	}
}
