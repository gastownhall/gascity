package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// TestHookIdentityFallbackResolvesAsBefore runs the hooks that take the exact
// identity with GC_SESSION_ID unset and stale. Each must take the legacy
// resolution and say exactly what it says with the exact identity — the same
// golden — and a message it sends must carry the same sender route.
//
// A GC_SESSION_ID naming a CLOSED session bead — an earlier incarnation that
// held the same alias — is pinned by its own golden: the hook serves nothing
// for it and says so (errHookSessionClosed), rather than falling back to the
// alias the live session now holds.
func TestHookIdentityFallbackResolvesAsBefore(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "hook_budget"))
	if err != nil {
		t.Fatalf("golden dir: %v", err)
	}
	prevUpdate := hookBudgetUpdateGolden
	hookBudgetUpdateGolden = os.Getenv(hookBudgetGoldenUpdateEnv) != ""
	t.Cleanup(func() { hookBudgetUpdateGolden = prevUpdate })
	scenarios := map[string]hookBudgetScenario{}
	for _, sc := range hookBudgetScenarios() {
		scenarios[sc.name] = sc
	}
	for _, name := range []string{
		"mail check --inject/one mail",
		"mail check --inject/auto-handoff",
		"mail check --inject/priority window",
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
			sessionID func(t *testing.T, f hookBudgetFixture) string
			// golden names the variant's own golden; empty shares the
			// scenario's.
			golden string
		}{
			{name: "exact", sessionID: func(_ *testing.T, f hookBudgetFixture) string { return f.sessionID }},
			{name: "unset", sessionID: func(*testing.T, hookBudgetFixture) string { return "" }},
			{name: "stale", sessionID: func(*testing.T, hookBudgetFixture) string { return "tc-999" }},
			{name: "stale closed", sessionID: closedIncarnationSessionID, golden: sc.name + "/stale closed session"},
		} {
			t.Run(name+"/"+variant.name, func(t *testing.T) {
				f := newHookBudgetFixture(t)
				sc.seed(t, f)
				t.Setenv("GC_SESSION_ID", variant.sessionID(t, f))
				run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) })
				if run.code != 0 {
					t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
				}
				golden := sc.name
				if variant.golden != "" {
					golden = variant.golden
				} else if variant.name != "exact" && hookBudgetUpdateGolden {
					// A shared golden is written from the exact identity
					// only; the fallbacks are compared against it.
					hookBudgetUpdateGolden = false
					t.Cleanup(func() { hookBudgetUpdateGolden = true })
				}
				assertHookBudgetGolden(t, goldenDir, golden, f, run)
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

// closedIncarnationSessionID creates a closed session bead that held the
// fixture session's alias — an earlier incarnation whose id a hook still
// carries — and returns its id.
func closedIncarnationSessionID(t *testing.T, f hookBudgetFixture) string {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:    "earlier " + hookBudgetIdentity,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": hookBudgetIdentity, "session_name": hookBudgetIdentity + "-earlier"},
	})
	if err != nil {
		t.Fatalf("Create earlier incarnation: %v", err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatalf("Close earlier incarnation: %v", err)
	}
	resetHookBudgetProcessState(t)
	return b.ID
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

// closeHookSession closes the fixture session's own bead, as a session that
// has been retired while its hooks still run.
func (f hookBudgetFixture) closeHookSession(t *testing.T) {
	t.Helper()
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if err := store.Close(f.sessionID); err != nil {
		t.Fatalf("Close(%s): %v", f.sessionID, err)
	}
	resetHookBudgetProcessState(t)
}

// TestClosedHookSessionIsNotServed pins the hooks of a session whose own bead
// (GC_SESSION_ID) is closed: none of them injects or archives anything, and
// none falls back to GC_ALIAS — which a successor session may hold, or a named
// session may materialize — so a retired session never consumes its
// successor's mail, handoffs or nudges. Each says why on stderr.
func TestClosedHookSessionIsNotServed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		run     func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int
		content string
	}{
		{name: "mail check --inject", run: mailCheckInject, content: "status please"},
		{name: "prime --hook", run: primeHook, content: "context cycle"},
		{name: "nudge drain --inject", run: nudgeDrainInject, content: "review queued work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookBudgetFixture(t)
			f.sendMail(t, "status please")
			f.autoHandoff(t)
			handoffID := onlyAutoHandoffID(t, f)
			f.queueNudge(t)
			f.closeHookSession(t)

			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return tc.run(t, f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			if strings.Contains(run.stdout, tc.content) {
				t.Fatalf("a closed session's hook injected %q:\n%s", tc.content, run.stdout)
			}
			if !strings.Contains(run.stderr, "closed") {
				t.Fatalf("skipping a closed session was silent; stderr=%q", run.stderr)
			}
			if b := getBead(t, f, handoffID); b.Status != "open" || b.Metadata[mail.ReadMetadataKey] != "" {
				t.Fatalf("a closed session's hook archived the handoff: status %q metadata %v", b.Status, b.Metadata)
			}
			for _, op := range run.ops {
				if op.Kind == beadstest.OpCreate {
					t.Fatalf("a closed session's hook created a bead (a successor materialized?):\n%s", describeHookBudgetOps(run.ops))
				}
			}
		})
	}
}

// TestSessionStartMailRefusesAClosedOwnSession pins the SessionStart mail read
// that is not handed a proven live session: when GC_SESSION_ID resolves to a
// closed bead it surfaces neither the auto-handoff block nor the ordinary-mail
// block, and says why.
func TestSessionStartMailRefusesAClosedOwnSession(t *testing.T) {
	f := newHookBudgetFixture(t)
	f.autoHandoff(t)
	f.closeHookSession(t)
	store, err := openCityStoreAt(f.cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	t.Cleanup(func() { resetHookBudgetProcessState(t) })
	var stderr strings.Builder
	startMail := sessionStartMailInjection(store, nil, f.cityDir, &stderr)
	if !startMail.ownSessionClosed || startMail.autoHandoff.text != "" || startMail.autoHandoff.afterDelivery != nil {
		t.Fatalf("closed own session: ownSessionClosed=%v autoHandoff=%q", startMail.ownSessionClosed, startMail.autoHandoff.text)
	}
	if !strings.Contains(stderr.String(), "closed") {
		t.Fatalf("refusal was silent; stderr=%q", stderr.String())
	}
}
