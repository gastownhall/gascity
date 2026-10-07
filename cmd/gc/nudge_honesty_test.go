package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// The nudge-honesty rows. Every one of these is a place the nudge path told an
// operator something that was not true: a store that would not open reported no
// cause, and a queued item reported plain success whether or not the live leg
// had even been attempted.

// TestOpenNudgeBeadStoreReportsWhyItCouldNotOpen: the seam used by the poll and
// drain helpers stays nil-tolerant (their contract is "no store means do
// nothing"), but the error form the operator-facing call sites use must carry
// the cause.
func TestOpenNudgeBeadStoreReportsWhyItCouldNotOpen(t *testing.T) {
	// A regular file where the city directory should be: the open genuinely
	// fails, which is the case the swallowed error used to render as an
	// unexplained "opening city store for X".
	notACity := filepath.Join(t.TempDir(), "city-is-a-file")
	if err := os.WriteFile(notACity, []byte("not a city"), 0o600); err != nil {
		t.Fatalf("seeding the fixture: %v", err)
	}

	store, err := openNudgeBeadStoreErr(notACity)
	if err == nil {
		t.Skipf("openNudgeBeadStoreErr(%q) opened a store over a plain file; this row needs a fixture the store layer actually refuses", notACity)
	}
	if store.Store != nil {
		t.Fatal("a failed open returned a usable store")
	}
	if !strings.Contains(err.Error(), notACity) {
		t.Fatalf("error = %q, want the offending city path named", err)
	}
}

// TestQueuedNudgeResultNamesTheQueueAndTheDowngrade: "Queued nudge for X" was
// printed identically whether the nudge had been queued by request or silently
// downgraded from a live delivery the provider cannot take — and it never said
// WHERE the item went. The queue's authority is the flock'd state.json (the
// shadow bead is a projection of it), so that path is what an operator needs.
func TestQueuedNudgeResultNamesTheQueueAndTheDowngrade(t *testing.T) {
	cityPath := t.TempDir()
	target := nudgeTarget{
		cityPath: cityPath,
		alias:    "worker-1",
		agent:    config.Agent{Name: "worker"},
		resolved: &config.ResolvedProvider{Name: "codex"},
	}

	var stdout, stderr bytes.Buffer
	if code := writeQueuedSessionNudgeResult(target, nudgeDeliveryWaitIdle, false,
		worker.NudgeUndeliveredProviderUnsupported, &stdout, &stderr); code != 0 {
		t.Fatalf("writeQueuedSessionNudgeResult = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, nudgequeue.StatePath(cityPath)) {
		t.Fatalf("queued message = %q, want the state.json queue path named", out)
	}
	if !strings.Contains(out, "live delivery is unsupported") || !strings.Contains(out, "codex") {
		t.Fatalf("queued message = %q, want the skipped live leg and its provider named", out)
	}

	// Control: a nudge queued BY REQUEST carries no downgrade note — the note
	// must describe something that happened, not decorate every queue write.
	stdout.Reset()
	if code := writeQueuedSessionNudgeResult(target, nudgeDeliveryQueue, false, "", &stdout, &stderr); code != 0 {
		t.Fatalf("writeQueuedSessionNudgeResult = %d, want 0", code)
	}
	out = stdout.String()
	if strings.Contains(out, "unsupported") || strings.Contains(out, "idle boundary") {
		t.Fatalf("queued-by-request message = %q, want no downgrade note", out)
	}
	if !strings.Contains(out, nudgequeue.StatePath(cityPath)) {
		t.Fatalf("queued-by-request message = %q, want the queue path named", out)
	}
}

// TestQueuedNudgeDowngradeNoteDistinguishesItsCauses keeps the two downgrades
// distinguishable: an unsupported transport is a permanent property of the
// runtime, while a missed idle boundary is a transient state of the session, and
// an operator acts differently on each.
func TestQueuedNudgeDowngradeNoteDistinguishesItsCauses(t *testing.T) {
	target := nudgeTarget{resolved: &config.ResolvedProvider{Name: "codex"}}
	unsupported := queuedNudgeDowngradeNote(target, worker.NudgeUndeliveredProviderUnsupported)
	noIdle := queuedNudgeDowngradeNote(target, worker.NudgeUndeliveredNoIdleBoundary)
	if unsupported == "" || noIdle == "" || unsupported == noIdle {
		t.Fatalf("downgrade notes must differ and be non-empty; unsupported=%q no-idle=%q", unsupported, noIdle)
	}
	// An open dialog is a third, distinct cause: nothing is wrong with the
	// transport or the session's pacing, a person has to answer something first.
	blocked := queuedNudgeDowngradeNote(target, worker.NudgeUndeliveredBlockedByDialog)
	if blocked == "" || blocked == unsupported || blocked == noIdle {
		t.Fatalf("a dialog-blocked note must be non-empty and differ from the others; blocked=%q unsupported=%q no-idle=%q", blocked, unsupported, noIdle)
	}
	if !strings.Contains(blocked, "dialog") {
		t.Fatalf("dialog-blocked note = %q, want the open dialog named", blocked)
	}
	if got := queuedNudgeDowngradeNote(target, ""); got != "" {
		t.Fatalf("note for a non-downgrade = %q, want empty", got)
	}
}

// TestSessionNudgeIntoAnOpenDialogIsQueuedNotReportedDelivered: with a question
// dialog open on the pane the worker boundary refuses to type — a nudge's Enter
// would answer the dialog — and reports the nudge undelivered with reason
// blocked_by_dialog. Wait-idle queued that for the dispatcher; the default
// --delivery=immediate fell through to "Nudged <target>" / outcome "delivered"
// with nothing typed and nothing queued, so the operator was told a message had
// landed that was in fact dropped. Neither delivery mode, in neither output
// format, may claim delivery for a nudge that was held back; both must leave it
// queued.
func TestSessionNudgeIntoAnOpenDialogIsQueuedNotReportedDelivered(t *testing.T) {
	for _, mode := range []nudgeDeliveryMode{nudgeDeliveryImmediate, nudgeDeliveryWaitIdle} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", mode, asJSON), func(t *testing.T) {
				t.Setenv("GC_BEADS", "file")
				t.Setenv("GC_HOME", t.TempDir())
				// Queueing starts a poller for a live session; none may outlive the test.
				prevPoller := startNudgePoller
				startNudgePoller = func(string, string, string) error { return nil }
				t.Cleanup(func() { startNudgePoller = prevPoller })

				dir := t.TempDir()
				store := openNudgeBeadStore(dir)
				fake := runtime.NewFake()
				mgr := newSessionManagerWithConfig(dir, store, fake, nil)
				info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
					t.Fatalf("Start: %v", err)
				}
				// Idle long enough that wait-idle's idle boundary is satisfied: the
				// only thing standing between the nudge and the pane is the dialog.
				fake.SetActivity(info.SessionName, time.Now().Add(-45*time.Minute))
				fake.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "question", Prompt: "How do you want to proceed?"})
				beforeCalls := len(fake.Calls)

				target := nudgeTarget{
					cityPath:    dir,
					agent:       config.Agent{Name: "worker"},
					resolved:    &config.ResolvedProvider{Name: "claude"},
					sessionID:   info.ID,
					sessionName: info.SessionName,
				}
				var stdout, stderr bytes.Buffer
				code := deliverSessionNudgeWithWorker(target, store, fake, "check deploy status", mode, asJSON, &stdout, &stderr)

				for _, call := range fake.Calls[beforeCalls:] {
					if call.Method == "Nudge" || call.Method == "NudgeNow" {
						t.Fatalf("typed into a pane with an open dialog: %+v", call)
					}
				}
				if code != 0 {
					t.Fatalf("exit = %d, want 0 (the nudge is queued); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				pending, _, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
				if err != nil {
					t.Fatalf("listQueuedNudgesForTarget: %v", err)
				}
				if len(pending) != 1 {
					t.Fatalf("queued nudges = %d, want 1: the held-back nudge must not be dropped; stdout=%q", len(pending), stdout.String())
				}

				if asJSON {
					var got sessionNudgeJSON
					if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
						t.Fatalf("stdout is not the nudge JSON: %v; stdout=%q", err, stdout.String())
					}
					if got.Outcome != "queued" || !got.Queued {
						t.Fatalf("JSON outcome=%q queued=%v, want outcome %q queued true (%+v)", got.Outcome, got.Queued, "queued", got)
					}
					return
				}
				out := stdout.String()
				if strings.Contains(out, "Nudged ") {
					t.Fatalf("stdout = %q claims delivery of a nudge that was held back", out)
				}
				if !strings.HasPrefix(out, "Queued nudge for ") {
					t.Fatalf("stdout = %q, want the queued confirmation", out)
				}
				// Only immediate delivery checks for the dialog itself. Wait-idle
				// never sees an idle boundary on a dialog pane and says so, which
				// is already an accurate reason.
				if mode == nudgeDeliveryImmediate && !strings.Contains(out, "dialog") {
					t.Fatalf("stdout = %q, want the open dialog named as the reason", out)
				}
			})
		}
	}
}

// TestManagedNudgeWakeReportsASkippedWake: the enqueue succeeded and the wake did
// not. Returning nil for both made a queued-but-unwoken nudge indistinguishable
// from a delivered one.
func TestManagedNudgeWakeReportsASkippedWake(t *testing.T) {
	var warnings bytes.Buffer
	prev := nudgeWarningWriter
	nudgeWarningWriter = &warnings
	t.Cleanup(func() { nudgeWarningWriter = prev })

	target := nudgeTarget{cityPath: t.TempDir(), alias: "worker-1", agent: config.Agent{Name: "worker"}}
	if err := requestManagedNudgeWake(target, nil); err != nil {
		t.Fatalf("requestManagedNudgeWake = %v, want nil (the enqueue still stands)", err)
	}
	if !strings.Contains(warnings.String(), "no managed wake was requested") {
		t.Fatalf("warnings = %q, want the skipped wake reported", warnings.String())
	}
	if !strings.Contains(warnings.String(), "no session store") {
		t.Fatalf("warnings = %q, want the missing precondition named", warnings.String())
	}
}
