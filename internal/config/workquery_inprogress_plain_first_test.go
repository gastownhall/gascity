package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bd-1.0.5 crash-recovery tier reads the session's first in_progress row with
// plain `bd list`, exactly as stock does, and asks the same one-row question with
// --include-ephemeral only when the plain read returns no row. That second read is
// what recovers a no-history step, which plain `bd list` hides and the ephemeral
// probe misses. Reading inclusively first would let an owned ephemeral row that
// sorts ahead of an ordinary durable step, and that the serve gate rejects as
// blocked or held, hide the durable step the plain read would have returned.

const (
	plainFirstSession = "sess-1"

	plainFirstDurableRow = `{"id":"durable-step","status":"in_progress","assignee":"sess-1"}`
	plainFirstNoHistory  = `{"id":"no-history-step","status":"in_progress","assignee":"sess-1","no_history":true}`
	plainFirstBlockedRow = `{"id":"wisp","status":"in_progress","assignee":"sess-1","ephemeral":true}`
	plainFirstHeldRow    = `{"id":"wisp","status":"in_progress","assignee":"sess-1","ephemeral":true,"labels":["hold:mayor"]}`
	plainFirstAnchorRow  = `{"id":"workflow-root","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}}`

	plainFirstOpenBlocker = `[{"id":"wisp","dependencies":[{"id":"gate","status":"open","dependency_type":"blocks"}]}]`
	plainFirstNoBlockers  = `[{"dependencies":[]}]`

	plainFirstInclusiveOneRowRead = "list --status in_progress --include-ephemeral --assignee=sess-1 --json --limit=1"
	plainFirstInclusiveWindowRead = "list --status in_progress --include-ephemeral --assignee=sess-1 --json --limit=20"
)

// plainFirstRecovery is one simulated bd for the recovery tier: the rows each
// in_progress read answers, the ephemeral snapshot, and the dependencies `bd show`
// reports for the row the serve gate asks about.
type plainFirstRecovery struct {
	plainOneRow     string
	inclusiveOneRow string
	inclusiveWindow string
	ephemeral       string
	blockersByID    map[string]string
}

// script renders the fake bd. Every invocation is appended to $BD_LOG so a test
// can assert which reads the generated command issued.
func (r plainFirstRecovery) script() string {
	var show strings.Builder
	for id, deps := range r.blockersByID {
		show.WriteString("      " + id + ") printf '%s' '" + deps + "' ;;\n")
	}
	return `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BD_LOG"
case "$1" in
  list)
    case "$*" in
      *--include-ephemeral*--limit=1) printf '%s' '` + r.inclusiveOneRow + `' ;;
      *--include-ephemeral*--limit=20) printf '%s' '` + r.inclusiveWindow + `' ;;
      *--limit=1) printf '%s' '` + r.plainOneRow + `' ;;
      *) printf '[]' ;;
    esac ;;
  query) printf '%s' '` + r.ephemeral + `' ;;
  show)
    case "$2" in
` + show.String() + `      *) printf '` + plainFirstNoBlockers + `' ;;
    esac ;;
  *) printf '[]' ;;
esac
`
}

// plainFirstCompositions are the two commands that embed the recovery tier: the
// standalone assigned-in-progress query and the combined work query, whose tier
// defers a graph workflow anchor.
func plainFirstCompositions(topo QueryTopology) map[string]string {
	a := Agent{Name: "worker", Dir: "hello-world"}
	return map[string]string{
		"standalone": a.EffectiveAssignedInProgressQueryFor(topo),
		"combined":   a.EffectiveWorkQueryFor(topo),
	}
}

// run executes command against the simulated bd and returns the served row ids
// and the bd invocations it made.
func (r plainFirstRecovery) run(t *testing.T, command string) (map[string]bool, string) {
	t.Helper()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	out := runShellWithFakeBd(t, command, map[string]string{
		"GC_SESSION_ID": plainFirstSession,
		"BD_LOG":        bdLog,
	}, r.script())
	calls, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	return workQueryOutputIDs(t, out), string(calls)
}

func plainFirstBD105() QueryTopology {
	return QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}
}

// An owned ephemeral row that the inclusive reader lists first and the serve gate
// rejects must not hide the session's ordinary durable in_progress step, which
// the plain reader serves. Reviewer finding: PR #7644 comment 6198127470.
func TestRecoveryKeepsDurableStepBehindRejectedEphemeralRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wisp      string
		blockers  map[string]string
		ephemeral string
	}{
		{
			name:      "blocked-ephemeral-first",
			wisp:      plainFirstBlockedRow,
			blockers:  map[string]string{"wisp": plainFirstOpenBlocker},
			ephemeral: "[" + plainFirstBlockedRow + "]",
		},
		{
			name:      "held-ephemeral-first",
			wisp:      plainFirstHeldRow,
			ephemeral: "[" + plainFirstHeldRow + "]",
		},
	} {
		sim := plainFirstRecovery{
			plainOneRow:     "[" + plainFirstDurableRow + "]",
			inclusiveOneRow: "[" + tc.wisp + "]",
			inclusiveWindow: "[" + tc.wisp + "," + plainFirstDurableRow + "]",
			ephemeral:       tc.ephemeral,
			blockersByID:    tc.blockers,
		}
		for composition, command := range plainFirstCompositions(plainFirstBD105()) {
			t.Run(tc.name+"/"+composition, func(t *testing.T) {
				ids, calls := sim.run(t, command)
				if !ids["durable-step"] || ids["wisp"] || len(ids) != 1 {
					t.Fatalf("recovery served %v, want only the durable step behind the rejected ephemeral row\nbd calls:\n%s", ids, calls)
				}
				if strings.Contains(calls, "--include-ephemeral") {
					t.Fatalf("a plain read that returned a row must not be followed by an inclusive read\nbd calls:\n%s", calls)
				}
			})
		}
	}
}

// The inclusive one-row read is the fallback for a plain read with no row: it is
// what recovers a no-history step, and it is issued once.
func TestRecoveryReadsInclusivelyOnlyWhenPlainReadReturnsNoRow(t *testing.T) {
	sim := plainFirstRecovery{
		plainOneRow:     "[]",
		inclusiveOneRow: "[" + plainFirstNoHistory + "]",
		inclusiveWindow: "[" + plainFirstNoHistory + "]",
		ephemeral:       "[]",
	}
	for composition, command := range plainFirstCompositions(plainFirstBD105()) {
		t.Run(composition, func(t *testing.T) {
			ids, calls := sim.run(t, command)
			if !ids["no-history-step"] || len(ids) != 1 {
				t.Fatalf("recovery served %v, want the no-history step\nbd calls:\n%s", ids, calls)
			}
			if got := strings.Count(calls, plainFirstInclusiveOneRowRead); got != 1 {
				t.Fatalf("inclusive one-row read issued %d times, want once\nbd calls:\n%s", got, calls)
			}
			plain := strings.Index(calls, "list --status in_progress --assignee=sess-1 --json --limit=1")
			if plain < 0 || plain > strings.Index(calls, plainFirstInclusiveOneRowRead) {
				t.Fatalf("the plain one-row read must come first\nbd calls:\n%s", calls)
			}
		})
	}
}

// Behind a graph workflow anchor the bounded second read stays inclusive, so a
// no-history step behind the anchor is still found.
func TestRecoveryGraphAnchorCandidatesReadStaysInclusive(t *testing.T) {
	sim := plainFirstRecovery{
		plainOneRow:     "[" + plainFirstAnchorRow + "]",
		inclusiveOneRow: "[" + plainFirstAnchorRow + "]",
		inclusiveWindow: "[" + plainFirstAnchorRow + "," + plainFirstNoHistory + "]",
		ephemeral:       "[]",
	}
	command := plainFirstCompositions(plainFirstBD105())["combined"]
	ids, calls := sim.run(t, command)
	if !ids["no-history-step"] || ids["workflow-root"] || len(ids) != 1 {
		t.Fatalf("recovery served %v, want the no-history step behind the anchor\nbd calls:\n%s", ids, calls)
	}
	if !strings.Contains(calls, plainFirstInclusiveWindowRead) {
		t.Fatalf("the graph-anchor candidates read must be inclusive\nbd calls:\n%s", calls)
	}
	if strings.Contains(calls, plainFirstInclusiveOneRowRead) {
		t.Fatalf("a plain read that returned the anchor row must not be followed by an inclusive one-row read\nbd calls:\n%s", calls)
	}
}

// Only a bd-1.0.5 city asks for the inclusive reads; the default tier and the
// federated reader keep the bytes they had.
func TestRecoveryInclusiveReadsAreBD105SingleStoreOnly(t *testing.T) {
	const fallbackStart = `if [ -z "$r" ] || [ "$r" = "[]" ]; then r=$(bd list --status in_progress --include-ephemeral `
	for _, shellVar := range []string{"id", "cand"} {
		modern := assignedInProgressTierCommand(shellVar, plainFirstBD105())
		plain := strings.Index(modern, `r=$(bd list --status in_progress --assignee="$`+shellVar+`" --json --limit=1)`)
		fallback := strings.Index(modern, fallbackStart)
		if plain < 0 || fallback < plain {
			t.Errorf("%s: bd-1.0.5 tier must read plain first and fall back inclusively: %q", shellVar, modern)
		}
		if got := strings.Count(modern, "--include-ephemeral"); got != 1 {
			t.Errorf("%s: bd-1.0.5 tier has %d inclusive reads, want only the fallback: %q", shellVar, got, modern)
		}
		window := assignedInProgressCandidatesTierCommand(shellVar, plainFirstBD105())
		if !strings.Contains(window, `bd list --status in_progress --include-ephemeral --assignee="$`+shellVar+`" --json --limit=20`) {
			t.Errorf("%s: bd-1.0.5 candidates read must stay inclusive: %q", shellVar, window)
		}
		for name, command := range map[string]string{
			"default tier":       assignedInProgressTierCommand(shellVar, QueryTopology{}),
			"default candidates": assignedInProgressCandidatesTierCommand(shellVar, QueryTopology{}),
		} {
			if strings.Contains(command, "--include-ephemeral") {
				t.Errorf("%s: %s must not ask for --include-ephemeral: %q", shellVar, name, command)
			}
		}
	}
}
