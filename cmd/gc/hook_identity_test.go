package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/mail"
)

// TestHookIdentityFallbackResolvesAsBefore runs the hooks that take the exact
// identity with GC_SESSION_ID unset and stale. Each must take the legacy
// resolution and say exactly what it says with the exact identity — the same
// golden — and a message it sends must carry the same sender route.
func TestHookIdentityFallbackResolvesAsBefore(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "hook_budget"))
	if err != nil {
		t.Fatalf("golden dir: %v", err)
	}
	hookBudgetUpdateGolden = false
	scenarios := map[string]hookBudgetScenario{}
	for _, sc := range hookBudgetScenarios() {
		scenarios[sc.name] = sc
	}
	for _, name := range []string{
		"mail check --inject/one mail",
		"mail check --inject/auto-handoff",
		"nudge drain --inject/one nudge",
		"nudge drain --inject/empty with usage stdin",
		"handoff --auto",
		"mail send human",
	} {
		sc, ok := scenarios[name]
		if !ok {
			t.Fatalf("no budget scenario %q", name)
		}
		var exactSender senderRoute
		for _, variant := range []struct {
			name      string
			sessionID func(f hookBudgetFixture) string
		}{
			{"exact", func(f hookBudgetFixture) string { return f.sessionID }},
			{"unset", func(hookBudgetFixture) string { return "" }},
			{"stale", func(hookBudgetFixture) string { return "tc-999" }},
		} {
			t.Run(name+"/"+variant.name, func(t *testing.T) {
				f := newHookBudgetFixture(t)
				sc.seed(t, f)
				t.Setenv("GC_SESSION_ID", variant.sessionID(f))
				run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) })
				if run.code != 0 {
					t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
				}
				assertHookBudgetGolden(t, goldenDir, sc.name, run.stdout)
				sender, sent := lastSentMessageSender(t, f)
				if !sent {
					return
				}
				if variant.name == "exact" {
					exactSender = sender
					return
				}
				if sender != exactSender {
					t.Errorf("sender route = %+v, want %+v (the exact identity's)", sender, exactSender)
				}
			})
		}
	}
}

// senderRoute is what a sent message records about its sender.
type senderRoute struct {
	from          string
	fromSessionID string
	fromDisplay   string
}

// lastSentMessageSender returns the sender route of the newest message the
// hook session sent, if it sent one.
func lastSentMessageSender(t *testing.T, f hookBudgetFixture) (senderRoute, bool) {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	rows, err := store.List(beads.ListQuery{Type: "message", IncludeClosed: true, TierMode: beads.TierBoth, Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	for _, b := range rows {
		if b.Metadata[mail.FromSessionIDMetadataKey] == "" && b.From == "human" {
			continue
		}
		return senderRoute{from: b.From, fromSessionID: b.Metadata[mail.FromSessionIDMetadataKey], fromDisplay: b.Metadata[mail.FromDisplayMetadataKey]}, true
	}
	return senderRoute{}, false
}

// TestNudgeDrainTargetsTheHookSessionBeforeItsAlias pins that the drain's own
// target is GC_SESSION_ID: an alias that no longer names the session (here, one
// naming nothing) does not stop the session's queued nudge from being
// delivered, and is never probed as a bead id.
func TestNudgeDrainTargetsTheHookSessionBeforeItsAlias(t *testing.T) {
	f := newHookBudgetFixture(t)
	f.queueNudge(t)
	t.Setenv("GC_ALIAS", "renamed-away")
	run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return nudgeDrainInject(t, f, stdout, stderr) })
	if run.code != 0 {
		t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
	}
	if !strings.Contains(run.stdout, "review queued work") {
		t.Fatalf("queued nudge not delivered to the GC_SESSION_ID session:\n%s", run.stdout)
	}
	for _, op := range run.ops {
		if op.Kind == beadstest.OpGet && op.ID == "renamed-away" {
			t.Fatalf("the alias was probed as a bead id:\n%s", describeHookBudgetOps(run.ops))
		}
	}
}
