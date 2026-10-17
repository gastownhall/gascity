package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Coverage for [beads] bd_list_ready_status: the crash-recovery tier reads
// `bd list --status in_progress --ready --exclude-label=<holds> --limit=N`, so
// the filter runs before the limit and a blocked or held first row no longer
// hides the agent's other workable in_progress beads.
//
// Before it, the limit-1 read returned the first in_progress row whatever its
// state. inProgressBlockedByEnrichmentScript then (correctly) refused a blocked
// or held row and fell through to the ready tiers, but `bd ready` excludes
// in_progress, so a second, workable in_progress bead was never served.
//
// The fake bd below models bd 1.3.1's `bd list`: --ready drops rows bd itself
// computes as not ready (not_ready: an open blocker, a future defer_until, or a
// blocked or deferred parent; beads internal/storage/sqlbuild/ready.go and
// issueops/ready_work.go), --exclude-label drops labeled rows, and both apply
// before --limit. not_ready is the fake's stand-in for bd's own computation;
// `deps` is what `bd show` reports, which is all the shell enrichment sees.

var bdListReadyStatusOn = QueryTopology{Beads: BeadsConfig{BDListReadyStatus: true}}

// fakeBdListReadyStatus returns a fake bd 1.3.1 whose in_progress rows are
// rows (a JSON array; every row assigned to sess-1 unless it says otherwise)
// and whose `bd ready` answers ready.
func fakeBdListReadyStatus(rows, ready string) string {
	return `#!/bin/sh
rows='` + rows + `'
case "$1" in
  list)
    ready=0; limit=0; excl='[]'; assignee=''
    for a in "$@"; do
      case "$a" in
        --ready) ready=1 ;;
        --limit=*) limit=${a#--limit=} ;;
        --assignee=*) assignee=${a#--assignee=} ;;
        --exclude-label=*) excl=$(printf '%s' "$excl" | jq -c --arg l "${a#--exclude-label=}" '. + [$l]') ;;
      esac
    done
    printf '%s' "$rows" | jq -c --argjson ready "$ready" --argjson limit "$limit" --argjson excl "$excl" --arg who "$assignee" '
      [.[] | select((.assignee // "sess-1") == $who)
           | select($ready == 0 or ((.not_ready // false) | not))
           | select([(.labels // [])[] | . as $l | select($excl | index($l))] | length == 0)]
      | (if $limit > 0 then .[:$limit] else . end)
      | map(del(.not_ready, .deps) | .assignee = (.assignee // "sess-1") | .status = "in_progress")' ;;
  show)
    printf '%s' "$rows" | jq -c --arg id "$2" '[.[] | select(.id == $id) | {id, status: "in_progress", dependencies: (.deps // [])}]' ;;
  ready) printf '%s' '` + ready + `' ;;
  *) printf '[]' ;;
esac
`
}

const (
	// blockedInProgressRow has an open `blocks` edge: bd computes it not
	// ready, and the shell enrichment sees the open blocker too.
	blockedInProgressRow  = `{"id":"wk-blocked","not_ready":true,"deps":[{"id":"gate-1","status":"open","dependency_type":"blocks"}]}`
	workableInProgressRow = `{"id":"wk-ready"}`
)

func runBDListReadyStatusTier(t *testing.T, script string, bdScript string) []string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	out := runShellWithFakeBd(t, script+`printf "[]"`, map[string]string{"GC_SESSION_ID": "sess-1"}, bdScript)
	return workQueryOutputIDOrder(t, strings.TrimSpace(out))
}

// Test 1: a blocked in_progress row first and a workable one second. Tier 1
// serves the second with the bool set. Unset, it serves nothing: the defect.
func TestBDListReadyStatusServesWorkableInProgressBehindBlockedRow(t *testing.T) {
	bd := fakeBdListReadyStatus(`[`+blockedInProgressRow+`,`+workableInProgressRow+`]`, `[]`)
	for _, tc := range []struct {
		name   string
		script func(QueryTopology) string
	}{
		{"standard", standardAssignedInProgressWorkQueryScript},
		{"standard-combined", standardAssignedWorkQueryScript},
		{"legacy-control", legacyControlAssignedInProgressWorkQueryScript},
		{"legacy-control-combined", legacyControlAssignedWorkQueryScript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runBDListReadyStatusTier(t, tc.script(bdListReadyStatusOn), bd); len(got) != 1 || got[0] != "wk-ready" {
				t.Fatalf("bool set: served %v, want [wk-ready]", got)
			}
			if got := runBDListReadyStatusTier(t, tc.script(QueryTopology{}), bd); len(got) != 0 {
				t.Fatalf("bool unset: served %v; the blocked first row should hide wk-ready (the defect the bool fixes)", got)
			}
		})
	}
}

// Test 2: a held in_progress row first and a workable one second, for each
// dispatch hold label.
func TestBDListReadyStatusServesWorkableInProgressBehindHeldRow(t *testing.T) {
	for _, label := range beadmeta.DispatchHoldLabels {
		t.Run(label, func(t *testing.T) {
			held := `{"id":"wk-held","labels":["` + label + `"]}`
			bd := fakeBdListReadyStatus(`[`+held+`,`+workableInProgressRow+`]`, `[]`)
			if got := runBDListReadyStatusTier(t, standardAssignedInProgressWorkQueryScript(bdListReadyStatusOn), bd); len(got) != 1 || got[0] != "wk-ready" {
				t.Fatalf("bool set: served %v, want [wk-ready]", got)
			}
			if got := runBDListReadyStatusTier(t, standardAssignedInProgressWorkQueryScript(QueryTopology{}), bd); len(got) != 0 {
				t.Fatalf("bool unset: served %v; the held first row should hide wk-ready", got)
			}
		})
	}
}

// Test 3 (regression): a blocked in_progress row and a ready assigned bead.
// The ready tier still serves the ready bead.
func TestBDListReadyStatusBlockedInProgressFallsThroughToReadyTier(t *testing.T) {
	bd := fakeBdListReadyStatus(`[`+blockedInProgressRow+`]`, `[{"id":"wk-next","status":"open","assignee":"sess-1"}]`)
	for _, topo := range []QueryTopology{{}, bdListReadyStatusOn} {
		if got := runBDListReadyStatusTier(t, standardAssignedWorkQueryScript(topo), bd); len(got) != 1 || got[0] != "wk-next" {
			t.Fatalf("bool=%t: served %v, want [wk-next]", topo.Beads.BDListReadyStatus, got)
		}
	}
}

// Test 4 (regression): only blocked in_progress rows. Nothing is served.
func TestBDListReadyStatusOnlyBlockedInProgressServesNothing(t *testing.T) {
	bd := fakeBdListReadyStatus(`[`+blockedInProgressRow+`,{"id":"wk-blocked-2","not_ready":true,"deps":[{"id":"gate-2","status":"in_progress","dependency_type":"waits-for"}]}]`, `[]`)
	for _, topo := range []QueryTopology{{}, bdListReadyStatusOn} {
		if got := runBDListReadyStatusTier(t, standardAssignedWorkQueryScript(topo), bd); len(got) != 0 {
			t.Fatalf("bool=%t: served %v, want nothing", topo.Beads.BDListReadyStatus, got)
		}
	}
}

// What bd's --ready adds over the shell enrichment, pinned one case each: the
// enrichment reads only the row's own blocking edges, so a deferred row and a
// child of a blocked parent pass it. With the bool set the reader drops them
// first and the workable row behind them is served.
func TestBDListReadyStatusSkipsRowsBdComputesNotReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
	}{
		{"deferred", `{"id":"wk-first","not_ready":true}`},
		{"child-of-blocked-parent", `{"id":"wk-first","not_ready":true,"deps":[{"id":"epic-1","status":"open","dependency_type":"parent-child"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := fakeBdListReadyStatus(`[`+tc.row+`,`+workableInProgressRow+`]`, `[]`)
			if got := runBDListReadyStatusTier(t, standardAssignedInProgressWorkQueryScript(bdListReadyStatusOn), bd); len(got) != 1 || got[0] != "wk-ready" {
				t.Fatalf("bool set: served %v, want [wk-ready]", got)
			}
			if got := runBDListReadyStatusTier(t, standardAssignedInProgressWorkQueryScript(QueryTopology{}), bd); len(got) != 1 || got[0] != "wk-first" {
				t.Fatalf("bool unset: served %v, want [wk-first] (the enrichment alone does not see it)", got)
			}
		})
	}
}

const graphRootFields = `"metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2"}`

// Test 5a: a blocked graph.v2 root with a ready step. The root is skipped (as
// the enrichment already did) and the step is served.
func TestBDListReadyStatusBlockedGraphRootServesStep(t *testing.T) {
	root := `{"id":"workflow-root","not_ready":true,` + graphRootFields + `,"deps":[{"id":"gate-1","status":"open","dependency_type":"blocks"}]}`
	bd := fakeBdListReadyStatus(`[`+root+`,{"id":"step-1"}]`, `[]`)
	for _, script := range []func(QueryTopology) string{standardAssignedWorkQueryScript, legacyControlAssignedWorkQueryScript} {
		if got := runBDListReadyStatusTier(t, script(bdListReadyStatusOn), bd); len(got) != 1 || got[0] != "step-1" {
			t.Fatalf("served %v, want [step-1]", got)
		}
	}
}

// Test 5b: a ready graph.v2 root with a ready step. The root is kept as the
// anchor and the step is served ahead of it.
func TestBDListReadyStatusReadyGraphRootKeepsAnchorAndServesStep(t *testing.T) {
	root := `{"id":"workflow-root",` + graphRootFields + `}`
	bd := fakeBdListReadyStatus(`[`+root+`,{"id":"step-1"}]`, `[]`)
	for _, script := range []func(QueryTopology) string{standardAssignedWorkQueryScript, legacyControlAssignedWorkQueryScript} {
		if got := runBDListReadyStatusTier(t, script(bdListReadyStatusOn), bd); len(got) != 1 || got[0] != "step-1" {
			t.Fatalf("served %v, want [step-1]", got)
		}
	}
	// A root alone stays the anchor fallback.
	bd = fakeBdListReadyStatus(`[`+root+`]`, `[]`)
	if got := runBDListReadyStatusTier(t, standardAssignedInProgressWorkQueryScript(bdListReadyStatusOn), bd); len(got) != 1 || got[0] != "workflow-root" {
		t.Fatalf("root alone: served %v, want [workflow-root]", got)
	}
}

// Tests 6 and 8: with the bool set, every crash-recovery read (limit 1 and the
// limit-20 anchor read, single-store and federated) carries --ready and one
// --exclude-label per dispatch hold label, and nothing else in the script
// does: the ephemeral probes and the ready tiers are unchanged.
func TestBDListReadyStatusFlagsOnlyOnCrashRecoveryReads(t *testing.T) {
	want := ` --ready --exclude-label=` + beadmeta.HoldMayorLabel + ` --exclude-label=` + beadmeta.HoldExternalLabel + ` --assignee=`
	for _, fed := range []bool{false, true} {
		on := QueryTopology{Beads: BeadsConfig{BDListReadyStatus: true}, FederatedReady: fed}
		off := QueryTopology{FederatedReady: fed}
		for _, shape := range parityAgentShapes() {
			for _, v := range parityVariants() {
				if v.override(shape.agent) != "" {
					continue
				}
				got := v.forTopo(shape.agent, on)
				base := v.forTopo(shape.agent, off)
				reads := strings.Count(base, ` --status in_progress --assignee=`)
				if n := strings.Count(got, want); n != reads {
					t.Errorf("fed=%t %s/%s: %d crash-recovery reads carry the flags, want all %d", fed, shape.name, v.name, n, reads)
				}
				if stripped := strings.ReplaceAll(got, want, ` --assignee=`); stripped != base {
					t.Errorf("fed=%t %s/%s: the bool changed more than the crash-recovery reads", fed, shape.name, v.name)
				}
			}
		}
	}
	probe := ephemeralAssignedInProgressProbeScript("id", bdListReadyStatusOn) +
		ephemeralAssignedInProgressProbeScriptDeferringGraphAnchor("id", bdListReadyStatusOn) +
		ephemeralAssignedReadyProbeScript("id", bdListReadyStatusOn)
	if strings.Contains(probe, "--ready") || strings.Contains(probe, "--exclude-label") {
		t.Fatalf("ephemeral probes picked up the reader flags: %q", probe)
	}
}

// Test 7: with the bool unset, every generated query is byte-identical to the
// committed goldens (which predate the bool), for every shape, kind, bd level
// and topology the golden matrix covers.
func TestBDListReadyStatusUnsetIsByteIdentical(t *testing.T) {
	shapes := []parityShape{
		{"normal", &Agent{Name: "worker"}},
		{"pool", &Agent{Name: "worker", PoolName: "worker-pool"}},
		{"legacy", &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}},
	}
	for _, shape := range shapes {
		for _, v := range parityVariants() {
			for _, flag := range []struct {
				name string
				topo QueryTopology
			}{
				{"bd104", QueryTopology{Beads: BeadsConfig{BDListReadyStatus: false}}},
				{"bd105", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105, BDListReadyStatus: false}}},
				{"bd104_federated", QueryTopology{Beads: BeadsConfig{BDListReadyStatus: false}, FederatedReady: true}},
				{"bd105_federated", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105, BDListReadyStatus: false}, FederatedReady: true}},
			} {
				if strings.HasSuffix(flag.name, "_federated") && !v.federates {
					continue
				}
				name := shape.name + "_" + v.name + "_" + flag.name + ".golden"
				want, err := os.ReadFile(filepath.Join("testdata", "workquery", name))
				if err != nil {
					t.Fatalf("read golden %s: %v", name, err)
				}
				if got := v.forTopo(shape.agent, flag.topo); got != string(want) {
					t.Errorf("%s: bool unset changed the generated query", name)
				}
			}
		}
	}
}
