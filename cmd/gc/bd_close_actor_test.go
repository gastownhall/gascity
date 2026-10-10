package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCloseActorForOwnClaim(t *testing.T) {
	session := map[string]string{
		"GC_SESSION_ID":   "ci-wisp-fe8",
		"GC_SESSION_NAME": "rig--gc__review-synthesizer-1-pool",
		"BEADS_ACTOR":     "rig--gc__review-synthesizer-1-pool",
	}
	getenv := func(env map[string]string) func(string) string { return func(k string) string { return env[k] } }
	held := func(assignee string) beads.Bead { return beads.Bead{ID: "ci-1", Assignee: assignee} }

	for _, tc := range []struct {
		name    string
		args    []string
		targets map[string]beads.Bead
		env     map[string]string
		// effective is the BEADS_ACTOR the bd child would run under; nil
		// means the same value as the process env.
		effective *string
		want      string
	}{
		{
			"own claim under the session bead id closes as that id",
			[]string{"close", "ci-1", "--reason", "done"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8")},
			session, nil, "ci-wisp-fe8",
		},
		{
			"update to closed is a close too",
			[]string{"update", "ci-1", "--status", "closed"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8")},
			session, nil, "ci-wisp-fe8",
		},
		{
			"a bead held by another session keeps the session's own actor",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-other")},
			session, nil, "",
		},
		{
			"already the actor needs no change",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("rig--gc__review-synthesizer-1-pool")},
			session, nil, "",
		},
		{
			"unassigned bead needs no change",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("")},
			session, nil, "",
		},
		{
			"outside a session nothing changes",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8")},
			map[string]string{"BEADS_ACTOR": "human"},
			nil, "",
		},
		{
			"an unread target leaves bd's check to decide",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{},
			session, nil, "",
		},
		{
			"targets held by two identities are not merged",
			[]string{"close", "ci-1", "ci-2"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8"), "ci-2": {ID: "ci-2", Assignee: "rig--gc__review-synthesizer-1-pool"}},
			session, nil, "",
		},
		{
			"a metadata update is not a close",
			[]string{"update", "ci-1", "--set-metadata", "gc.outcome=pass"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8")},
			session, nil, "",
		},
		{
			"a claim under the session name is a chair claim, not the session's own (#6324)",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("rig--gc__review-synthesizer-1-pool")},
			session, strPtr("city-default-actor"), "",
		},
		{
			"a claim under the alias is a chair claim, not the session's own (#6324)",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("review-synthesizer")},
			map[string]string{"GC_SESSION_ID": "ci-wisp-fe8", "GC_ALIAS": "review-synthesizer", "BEADS_ACTOR": "ci-wisp-fe8"},
			strPtr("city-default-actor"), "",
		},
		{
			"the process env's BEADS_ACTOR is not an own identity when the child runs under another",
			[]string{"close", "ci-1", "ci-2"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8"), "ci-2": {ID: "ci-2", Assignee: "rig--gc__review-synthesizer-1-pool"}},
			session, strPtr("city-default-actor"), "",
		},
		{
			"the child's effective actor counts as an own identity",
			[]string{"close", "ci-1", "ci-2"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8"), "ci-2": {ID: "ci-2", Assignee: "ci-wisp-fe8"}},
			session, strPtr("city-default-actor"), "ci-wisp-fe8",
		},
		{
			"an effective actor already matching the assignee needs no change",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("ci-wisp-fe8")},
			session, strPtr("ci-wisp-fe8"), "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effective := tc.env["BEADS_ACTOR"]
			if tc.effective != nil {
				effective = *tc.effective
			}
			if got := closeActorForOwnClaim(tc.args, tc.targets, getenv(tc.env), effective); got != tc.want {
				t.Fatalf("closeActorForOwnClaim(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestWithEnvValueReplacesEveryPriorEntry(t *testing.T) {
	got := withEnvValue([]string{"A=1", "BEADS_ACTOR=old", "B=2", "BEADS_ACTOR=older"}, "BEADS_ACTOR", "new")
	if want := []string{"A=1", "B=2", "BEADS_ACTOR=new"}; !slices.Equal(got, want) {
		t.Fatalf("withEnvValue = %v, want %v", got, want)
	}
}

// ownClaimCloseCity wires a city whose bd passthrough runs a stub that answers
// every read with the given bead and records the BEADS_ACTOR each invocation
// runs under, one line per call.
func ownClaimCloseCity(t *testing.T, assignee string) (actorLog string) {
	t.Helper()
	_, _ = bdSQLRefusalCityDir(t, "")
	binDir := t.TempDir()
	actorLog = filepath.Join(t.TempDir(), "bd-actors.txt")
	issue := `[{"id":"demo-1","title":"work","status":"in_progress","issue_type":"task","assignee":"` + assignee + `","created_at":"2026-10-01T00:00:00Z"}]`
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"${BEADS_ACTOR:-}\" >> \"" + actorLog + "\"\nprintf '%s\\n' '" + issue + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_SESSION_ID", "demo-wisp-fe8")
	t.Setenv("GC_SESSION_NAME", "demo--worker-1")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("BEADS_ACTOR", "demo--worker-1")
	return actorLog
}

// closeActorFromLog returns the BEADS_ACTOR the close invocation ran under.
func closeActorFromLog(t *testing.T, actorLog string) string {
	t.Helper()
	data, err := os.ReadFile(actorLog)
	if err != nil {
		t.Fatalf("reading bd actor log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if verb, actor, _ := strings.Cut(line, " "); verb == "close" {
			return actor
		}
	}
	t.Fatalf("bd close was never invoked; log = %q", data)
	return ""
}

// TestDoBdClosesOwnClaimUnderTheSessionBeadID pins the doBd wiring: a session
// closing a bead claimed under its session bead id runs bd close as that id.
func TestDoBdClosesOwnClaimUnderTheSessionBeadID(t *testing.T) {
	actorLog := ownClaimCloseCity(t, "demo-wisp-fe8")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-1", "--reason", "done"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd close = %d; stderr=%q", code, stderr.String())
	}
	if got := closeActorFromLog(t, actorLog); got != "demo-wisp-fe8" {
		t.Fatalf("bd close ran as %q, want the session bead id demo-wisp-fe8", got)
	}
}

// TestDoBdDoesNotCloseChairNamedClaimsAsTheChair pins #6324 on the close path:
// a session name or alias is a chair a successor session can share, so a claim
// recorded under one is not this session's own, and bd's assignee check must
// still see the session's actor rather than the chair name.
func TestDoBdDoesNotCloseChairNamedClaimsAsTheChair(t *testing.T) {
	for _, chair := range []string{"demo--worker-1", "worker-1"} {
		t.Run(chair, func(t *testing.T) {
			actorLog := ownClaimCloseCity(t, chair)
			t.Setenv("BEADS_ACTOR", "")

			var stdout, stderr bytes.Buffer
			if code := doBd([]string{"close", "demo-1"}, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd close = %d; stderr=%q", code, stderr.String())
			}
			if got := closeActorFromLog(t, actorLog); got == chair {
				t.Fatalf("bd close ran as the chair name %q; a chair-named claim is not the session's own", got)
			}
		})
	}
}
