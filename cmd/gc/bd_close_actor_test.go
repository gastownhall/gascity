package main

import (
	"slices"
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
			"the child's effective actor, not the process env, decides whether a change is needed",
			[]string{"close", "ci-1"},
			map[string]beads.Bead{"ci-1": held("rig--gc__review-synthesizer-1-pool")},
			session, strPtr("city-default-actor"), "rig--gc__review-synthesizer-1-pool",
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
