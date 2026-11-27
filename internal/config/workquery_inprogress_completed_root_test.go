package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// A graph.v2 workflow root that is in_progress and still assigned, but whose
// steps are all closed, can never progress. The crash-recovery tier must not
// re-serve it to every new session of the owning identity (#5983).

const completedWorkflowRootRow = `[{"id":"wf-root-1","status":"in_progress","assignee":"sess-1",` +
	`"issue_type":"task","title":"Run of mol-verified-work",` +
	`"metadata":{"gc.kind":"workflow","gc.routed_to":"luna-worker",` +
	`"gc.formula_contract":"graph.v2"}}]`

const closedWorkflowSteps = `[{"id":"wf-step-1","status":"closed","metadata":{"gc.root_bead_id":"wf-root-1"}},` +
	`{"id":"wf-step-2","status":"closed","metadata":{"gc.root_bead_id":"wf-root-1"}}]`

// fakeBdWorkflowRootWithSteps reports the root from the in_progress list, the
// given step rows from any list filtered on gc.root_bead_id (filtered by the
// requested statuses when --status is given), and the real root show shape,
// which carries no dependencies array.
func fakeBdWorkflowRootWithSteps(stepsJSON string) string {
	return `#!/bin/sh
case "$1" in
  list)
    case "$*" in
      *gc.root_bead_id*)
        statuses=""
        prev=""
        for a in "$@"; do
          case "$prev" in --status) statuses="$a" ;; esac
          case "$a" in --status=*) statuses="${a#--status=}" ;; esac
          prev="$a"
        done
        if [ -n "$statuses" ]; then
          printf '%s' '` + stepsJSON + `' | jq -c --arg s "$statuses" '[.[] | select(.status as $st | ($s | split(",") | index($st)))]'
        else
          printf '%s' '` + stepsJSON + `'
        fi ;;
      *) printf '%s' '` + completedWorkflowRootRow + `' ;;
    esac ;;
  show) printf '%s' '[{"id":"wf-root-1","status":"in_progress","assignee":"sess-1","issue_type":"task","metadata":{"gc.kind":"workflow"}}]' ;;
  *) printf '[]' ;;
esac
`
}

func TestInProgressTierSkipsCompletedWorkflowRoot(t *testing.T) {
	rows := runInProgressTier(t, fakeBdWorkflowRootWithSteps(closedWorkflowSteps))
	if len(rows) != 0 {
		t.Fatalf("completed workflow root (all steps closed) was re-served by the crash-recovery tier: %v", rows)
	}
}

// workflowRootFake is the simulated store behind one generated query run. The
// same script answers as `bd` and as `gc` (the federated reader), so every
// query form reads the same rows.
type workflowRootFake struct {
	// inProgress holds the in_progress rows the crash-recovery read returns,
	// filtered by --assignee: the workflow root first, then anything behind it.
	inProgress string
	// members holds every workflow member row, whatever status; a member read
	// returns the ones stamped with the root it asks for.
	members string
	// memberRead is "" for a normal member read, "fail" for one that exits 1,
	// and "garbage" for one that prints output jq cannot parse.
	memberRead string
	// anchorReady answers the anchor-ready probe (a ready read on
	// gc.root_bead_id without --status) with its rows of the root it asks for.
	anchorReady string
	// assignedReady answers the assigned-ready read (`ready --assignee=`).
	assignedReady string
	// pool answers the routed pool read (`ready` with neither assignee nor
	// gc.root_bead_id).
	pool string
	// ephemeral serves the in_progress rows from the bd-1.0.4 `bd query`
	// snapshot instead of from `bd list`.
	ephemeral bool
}

func (f workflowRootFake) script() string {
	orEmpty := func(s string) string {
		if s == "" {
			return "[]"
		}
		return s
	}
	memberRead := `filter_rows "$members"`
	switch f.memberRead {
	case "fail":
		memberRead = `printf 'member store unavailable\n' >&2; exit 1`
	case "garbage":
		memberRead = `printf 'warning: not json'`
	}
	listInProgress, queryInProgress := `filter_rows "$inprog"`, `printf '[]'`
	if f.ephemeral {
		listInProgress, queryInProgress = `printf '[]'`, `printf '%s' "$inprog"`
	}
	return `#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$WF_FAKE_LOG"
members='` + orEmpty(f.members) + `'
inprog='` + orEmpty(f.inProgress) + `'
anchor_ready='` + orEmpty(f.anchorReady) + `'
assigned_ready='` + orEmpty(f.assignedReady) + `'
pool='` + orEmpty(f.pool) + `'
statuses=""; limit=""; assignee=""; root=""; prev=""
for a in "$@"; do
  case "$prev" in --status) statuses="$a" ;; --limit) limit="$a" ;; esac
  case "$a" in
    --status=*) statuses="${a#--status=}" ;;
    --limit=*) limit="${a#--limit=}" ;;
    --assignee=*) assignee="${a#--assignee=}" ;;
    gc.root_bead_id=*) root="${a#gc.root_bead_id=}" ;;
  esac
  prev="$a"
done
filter_rows() {
  printf '%s' "$1" | jq -c --arg s "$statuses" --arg l "$limit" --arg a "$assignee" --arg r "$root" '
    [.[] | select(if $s == "" then .status != "closed" else (.status as $st | $s | split(",") | index($st)) != null end)
         | select($a == "" or .assignee == $a)
         | select($r == "" or .metadata["gc.root_bead_id"] == $r)]
    | if ($l | length) > 0 and ($l | tonumber) > 0 then .[:($l | tonumber)] else . end'
}
case "$*" in
  *gc.root_bead_id=*)
    if [ "$1" = list ] || [ -n "$statuses" ]; then
      ` + memberRead + `
    else
      printf '%s' "$anchor_ready" | jq -c --arg r "$root" '[.[] | select(.metadata["gc.root_bead_id"] == $r)]'
    fi
    exit 0 ;;
esac
case "$1" in
  list) ` + listInProgress + ` ;;
  show) printf '[{"id":"%s","status":"in_progress"}]' "$2" ;;
  query)
    case "$*" in
      *status=in_progress*) ` + queryInProgress + ` ;;
      *) printf '[]' ;;
    esac ;;
  ready)
    if [ "$statuses" = in_progress ]; then
      ` + listInProgress + `
    elif [ -n "$assignee" ]; then
      printf '%s' "$assigned_ready"
    else
      printf '%s' "$pool"
    fi ;;
  *) printf '[]' ;;
esac
`
}

// workflowRootRun is the outcome of one generated query against a
// workflowRootFake.
type workflowRootRun struct {
	stdout string
	stderr string
	exit   int
	calls  []string
}

func (r workflowRootRun) ids(t *testing.T) []string {
	t.Helper()
	return workQueryOutputIDOrder(t, strings.TrimSpace(r.stdout))
}

// memberReads returns the logged calls that read a workflow's members: a
// `bd list` or a `gc ready --status` read on gc.root_bead_id. The anchor-ready
// probe reads ready rows on gc.root_bead_id too, and is not one.
func (r workflowRootRun) memberReads() []string {
	var reads []string
	for _, call := range r.calls {
		if strings.Contains(call, "gc.root_bead_id=") &&
			(strings.HasPrefix(call, "bd list ") || strings.Contains(call, " --status ")) {
			reads = append(reads, call)
		}
	}
	return reads
}

func runWorkflowRootQuery(t *testing.T, command string, env map[string]string, fake workflowRootFake) workflowRootRun {
	t.Helper()
	requireJQ(t)
	logPath := filepath.Join(t.TempDir(), "calls.log")
	runEnv := map[string]string{"WF_FAKE_LOG": logPath}
	for k, v := range env {
		runEnv[k] = v
	}
	script := fake.script()
	res := runGeneratedQueryWithBD(t, command, runEnv, script, script)
	run := workflowRootRun{stdout: res.stdout, stderr: res.stderr, exit: res.exit}
	if data, err := os.ReadFile(logPath); err == nil {
		run.calls = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return run
}

// inProgressTierForm is one generated shape of the assigned in-progress tier.
type inProgressTierForm struct {
	name      string
	command   string
	ephemeral bool
}

func inProgressTierForms() []inProgressTierForm {
	shell := func(script string) string { return script + `printf "[]"` }
	return []inProgressTierForm{
		{name: "stock", command: shell(standardAssignedInProgressWorkQueryScript(QueryTopology{}))},
		{name: "legacy-control", command: shell(legacyControlAssignedInProgressWorkQueryScript(QueryTopology{}))},
		{name: "federated", command: shell(standardAssignedInProgressWorkQueryScript(federatedTopology()))},
		{name: "bd104-ephemeral", command: shell(standardAssignedInProgressWorkQueryScript(QueryTopology{})), ephemeral: true},
	}
}

func runInProgressTierForm(t *testing.T, form inProgressTierForm, fake workflowRootFake) workflowRootRun {
	t.Helper()
	fake.ephemeral = form.ephemeral
	run := runWorkflowRootQuery(t, form.command, map[string]string{"GC_SESSION_ID": "sess-1"}, fake)
	if run.exit != 0 {
		t.Fatalf("%s: tier exited %d; stderr=%s", form.name, run.exit, run.stderr)
	}
	return run
}

// workflowRootJSON is an in_progress graph.v2 workflow root assigned to
// sess-1, as `bd list` reads it back: no dependencies array.
func workflowRootJSON(id string, extraMeta string) string {
	return `{"id":"` + id + `","status":"in_progress","assignee":"sess-1","issue_type":"task",` +
		`"metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"luna-worker"` + extraMeta + `}}`
}

// contractlessFinishedRoot and paddedKindFinishedRoot are in_progress
// workflow roots assigned to sess-1 that the graph-anchor predicate does not
// match: one has no gc.formula_contract stamp, the other a padded gc.kind.
// Both are still workflow roots to the finished-root gate.
const (
	contractlessFinishedRoot = `{"id":"wf-root-1","status":"in_progress","assignee":"sess-1","issue_type":"task",` +
		`"metadata":{"gc.kind":"workflow"}}`
	paddedKindFinishedRoot = `{"id":"wf-root-1","status":"in_progress","assignee":"sess-1","issue_type":"task",` +
		`"metadata":{"gc.kind":" workflow ","gc.formula_contract":"graph.v2"}}`
)

func memberJSON(id, root, status, kind string) string {
	return `{"id":"` + id + `","status":"` + status + `","issue_type":"task",` +
		`"metadata":{"gc.kind":"` + kind + `","gc.root_bead_id":"` + root + `"}}`
}

func jsonRows(rows ...string) string { return `[` + strings.Join(rows, ",") + `]` }

// finishedMemberShapes are member sets of a workflow whose work is done: at
// least one closed worker step, and at most the finalizer and the scope/spec
// latches still open.
func finishedMemberShapes(root string) map[string]string {
	closedStep := memberJSON("step-a", root, "closed", "task")
	return map[string]string{
		"all closed": jsonRows(closedStep, memberJSON("step-b", root, "closed", "task"),
			memberJSON("fin", root, "closed", "workflow-finalize")),
		"finalizer open": jsonRows(closedStep, memberJSON("fin", root, "open", "workflow-finalize")),
		"finalizer and scope open": jsonRows(closedStep, memberJSON("fin", root, "open", "workflow-finalize"),
			memberJSON("scope", root, "open", "scope")),
		"finalizer and spec open": jsonRows(closedStep, memberJSON("fin", root, "open", "workflow-finalize"),
			memberJSON("spec", root, "open", "spec")),
	}
}

// TestInProgressTierSkipsFinishedWorkflowRoot covers AC-1 across every form of
// the tier, for legacy unstamped roots and gc.workflow_expanded roots alike.
func TestInProgressTierSkipsFinishedWorkflowRoot(t *testing.T) {
	for _, stamp := range []struct{ name, meta string }{
		{"unstamped", ``},
		{"expanded", `,"gc.workflow_expanded":"true"`},
	} {
		for shape, members := range finishedMemberShapes("wf-root-1") {
			for _, form := range inProgressTierForms() {
				t.Run(stamp.name+"/"+shape+"/"+form.name, func(t *testing.T) {
					run := runInProgressTierForm(t, form, workflowRootFake{
						inProgress: jsonRows(workflowRootJSON("wf-root-1", stamp.meta)),
						members:    members,
					})
					if ids := run.ids(t); len(ids) != 0 {
						t.Fatalf("finished workflow root was re-served: %v (stdout %q)", ids, run.stdout)
					}
				})
			}
		}
	}
}

// TestInProgressTierSkipsFinishedWorkflowRootWithPaddedKind pins that the root
// and member kinds are compared trimmed, as every other kind read is.
func TestInProgressTierSkipsFinishedWorkflowRootWithPaddedKind(t *testing.T) {
	root := `{"id":"wf-root-1","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":" workflow "}}`
	members := jsonRows(memberJSON("step-a", "wf-root-1", "closed", "task"),
		memberJSON("fin", "wf-root-1", "open", " workflow-finalize "))
	for _, form := range inProgressTierForms() {
		t.Run(form.name, func(t *testing.T) {
			run := runInProgressTierForm(t, form, workflowRootFake{inProgress: jsonRows(root), members: members})
			if ids := run.ids(t); len(ids) != 0 {
				t.Fatalf("finished workflow root with a padded kind was re-served: %v", ids)
			}
		})
	}
}

// liveMemberShapes are member sets of a workflow that can still progress: some
// member other than the finalizer and the scope/spec latches is not closed.
func liveMemberShapes(root string) map[string]string {
	closedStep := memberJSON("step-a", root, "closed", "task")
	fin := memberJSON("fin", root, "open", "workflow-finalize")
	return map[string]string{
		"open step":         jsonRows(closedStep, memberJSON("step-b", root, "open", "task"), fin),
		"in_progress step":  jsonRows(closedStep, memberJSON("step-b", root, "in_progress", "task"), fin),
		"dep-blocked step":  jsonRows(closedStep, memberJSON("step-b", root, "blocked", "task"), fin),
		"open retry":        jsonRows(closedStep, memberJSON("retry", root, "open", "retry"), fin),
		"open check":        jsonRows(closedStep, memberJSON("check", root, "open", "check"), fin),
		"open cleanup step": jsonRows(closedStep, memberJSON("cleanup", root, "open", "cleanup"), fin),
		"open nested workflow": jsonRows(closedStep, memberJSON("nested", root, "open", "workflow"),
			memberJSON("scope", root, "open", "scope")),
	}
}

// TestInProgressTierServesWorkflowRootWithLiveMember covers AC-2: a root whose
// workflow still has live work stays the session's continuation anchor.
func TestInProgressTierServesWorkflowRootWithLiveMember(t *testing.T) {
	for _, stamp := range []struct{ name, meta string }{
		{"unstamped", ``},
		{"expanded", `,"gc.workflow_expanded":"true"`},
	} {
		for shape, members := range liveMemberShapes("wf-root-1") {
			for _, form := range inProgressTierForms() {
				t.Run(stamp.name+"/"+shape+"/"+form.name, func(t *testing.T) {
					run := runInProgressTierForm(t, form, workflowRootFake{
						inProgress: jsonRows(workflowRootJSON("wf-root-1", stamp.meta)),
						members:    members,
					})
					if ids := run.ids(t); len(ids) != 1 || ids[0] != "wf-root-1" {
						t.Fatalf("workflow root with a live member was not served: %v (stdout %q)", ids, run.stdout)
					}
				})
			}
		}
	}
}

// TestInProgressTierServesRootOnlyAndFreshWorkflowRoots covers AC-3: a root no
// worker step has closed under yet — a never-expanded root-only workflow, a
// freshly expanded one, or one whose member window the read cannot see the
// end of — is still served.
func TestInProgressTierServesRootOnlyAndFreshWorkflowRoots(t *testing.T) {
	const root = "wf-root-1"
	latches := []string{memberJSON("step-a", root, "closed", "task"), memberJSON("fin", root, "open", "workflow-finalize")}
	for i := 0; len(latches) < workflowMemberReadLimit+1; i++ {
		latches = append(latches, memberJSON("scope-"+string(rune('a'+i)), root, "open", "scope"))
	}
	for shape, members := range map[string]string{
		"root only": ``,
		"fresh":     jsonRows(memberJSON("step-a", root, "open", "task"), memberJSON("fin", root, "open", "workflow-finalize")),
		"fresh with only latches open": jsonRows(memberJSON("fin", root, "open", "workflow-finalize"),
			memberJSON("scope", root, "open", "scope")),
		"only closed topology members": jsonRows(memberJSON("scope", root, "closed", "scope"),
			memberJSON("spec", root, "closed", "spec")),
		"saturated member window": jsonRows(latches...),
	} {
		for _, form := range inProgressTierForms() {
			t.Run(shape+"/"+form.name, func(t *testing.T) {
				run := runInProgressTierForm(t, form, workflowRootFake{
					inProgress: jsonRows(workflowRootJSON(root, ``)),
					members:    members,
				})
				if ids := run.ids(t); len(ids) != 1 || ids[0] != root {
					t.Fatalf("%s workflow root was not served: %v (stdout %q)", shape, ids, run.stdout)
				}
			})
		}
	}
}

// TestInProgressTierServesWorkflowRootWhenMemberReadFails covers AC-4: a member
// read the gate cannot trust serves the root, keeps the query's exit 0, and
// says so on stderr naming the root.
func TestInProgressTierServesWorkflowRootWhenMemberReadFails(t *testing.T) {
	const root = "wf-root-4"
	members := finishedMemberShapes(root)["finalizer open"]
	for _, failure := range []string{"fail", "garbage"} {
		for _, form := range inProgressTierForms() {
			t.Run(failure+"/"+form.name, func(t *testing.T) {
				run := runInProgressTierForm(t, form, workflowRootFake{
					inProgress: jsonRows(workflowRootJSON(root, ``)),
					members:    members,
					memberRead: failure,
				})
				if ids := run.ids(t); len(ids) != 1 || ids[0] != root {
					t.Fatalf("root was not served on a %s member read: %v (stdout %q)", failure, ids, run.stdout)
				}
				if !strings.Contains(run.stderr, "gc work query: workflow member read for "+root) {
					t.Fatalf("no diagnostic naming the root on a %s member read; stderr=%q", failure, run.stderr)
				}
			})
		}
	}
}

// TestInProgressTierSkipsMemberReadForNonWorkflowCandidate covers AC-6: the
// member read costs a store round trip, so only a candidate whose trimmed
// gc.kind is workflow pays it.
func TestInProgressTierSkipsMemberReadForNonWorkflowCandidate(t *testing.T) {
	for _, kind := range []string{"task", "", "workflow-finalize", "scope"} {
		row := `{"id":"cand-1","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"` + kind + `"}}`
		for _, form := range inProgressTierForms() {
			t.Run("kind="+kind+"/"+form.name, func(t *testing.T) {
				run := runInProgressTierForm(t, form, workflowRootFake{
					inProgress: jsonRows(row),
					members:    jsonRows(memberJSON("step-a", "cand-1", "closed", "task")),
				})
				if ids := run.ids(t); len(ids) != 1 || ids[0] != "cand-1" {
					t.Fatalf("non-workflow candidate was not served: %v (stdout %q)", ids, run.stdout)
				}
				if reads := run.memberReads(); len(reads) != 0 {
					t.Fatalf("non-workflow candidate (kind %q) issued member reads: %v", kind, reads)
				}
			})
		}
	}
}

// combinedQueryForm is one generated shape of the combined work query, run for
// an agent whose pool target is hello-world/worker (or its legacy
// workflow-control twin).
type combinedQueryForm struct {
	name      string
	command   string
	ephemeral bool
}

func combinedQueryForms() []combinedQueryForm {
	worker := Agent{Name: "worker", Dir: "hello-world"}
	control := Agent{Name: ControlDispatcherAgentName, Dir: "hello-world"}
	return []combinedQueryForm{
		{name: "stock", command: worker.EffectiveWorkQueryFor(QueryTopology{})},
		{name: "legacy-control", command: control.EffectiveWorkQueryFor(QueryTopology{})},
		{name: "federated", command: worker.EffectiveWorkQueryFor(federatedTopology())},
		{name: "bd104-ephemeral", command: worker.EffectiveWorkQueryFor(QueryTopology{}), ephemeral: true},
	}
}

// runCombinedQueryForm runs one combined query form and returns the ids it
// printed. The origin gate ends the query with no output when it has nothing to
// flush, which reads as no work, the same as "[]".
func runCombinedQueryForm(t *testing.T, form combinedQueryForm, env map[string]string, fake workflowRootFake) []string {
	t.Helper()
	fake.ephemeral = form.ephemeral
	run := runWorkflowRootQuery(t, form.command, env, fake)
	if run.exit != 0 {
		t.Fatalf("%s: query exited %d; stderr=%s", form.name, run.exit, run.stderr)
	}
	if strings.TrimSpace(run.stdout) == "" {
		return nil
	}
	return run.ids(t)
}

// TestCombinedWorkQueryPassesOverFinishedWorkflowHead covers AC-5: when the
// session's in_progress head is a finished graph.v2 root, no part of the
// combined query prints it, and the query answers with the session's next
// servable work, or none.
func TestCombinedWorkQueryPassesOverFinishedWorkflowHead(t *testing.T) {
	finishedRoot := workflowRootJSON("wf-root-1", ``)
	finishedMembers := finishedMemberShapes("wf-root-1")["finalizer open"]
	liveRoot := `{"id":"wf-root-2","status":"in_progress","assignee":"alias-1","issue_type":"task",` +
		`"metadata":{"gc.kind":"workflow","gc.formula_contract":"graph.v2","gc.routed_to":"hello-world/worker"}}`
	liveMembers := []string{memberJSON("live-a", "wf-root-2", "closed", "task"), memberJSON("live-b", "wf-root-2", "open", "task")}
	members := func(extra ...string) string {
		rows := []string{memberJSON("step-a", "wf-root-1", "closed", "task"), memberJSON("fin", "wf-root-1", "open", "workflow-finalize")}
		return jsonRows(append(rows, extra...)...)
	}
	ephemeralOrigin := map[string]string{"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral"}
	namedOrigin := map[string]string{"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "named"}
	// The same identity's live root behind the finished one, and its ready step.
	ownLiveRoot := workflowRootJSON("wf-root-2", ``)
	ownLiveReadyStep := `{"id":"live-ready","status":"open","metadata":{"gc.kind":"task","gc.root_bead_id":"wf-root-2","gc.routed_to":"hello-world/worker"}}`
	otherStep := `{"id":"other-step","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"task"}}`
	for _, tc := range []struct {
		name string
		env  map[string]string
		fake workflowRootFake
		want []string
	}{
		{
			name: "in_progress step behind the finished head",
			env:  ephemeralOrigin,
			fake: workflowRootFake{
				inProgress: jsonRows(finishedRoot,
					`{"id":"other-step","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"task"}}`),
				members: finishedMembers,
				pool:    jsonRows(`{"id":"pool-1","status":"open","metadata":{"gc.routed_to":"hello-world/worker"}}`),
			},
			want: []string{"other-step"},
		},
		{
			name: "assigned-ready row",
			env:  ephemeralOrigin,
			fake: workflowRootFake{
				inProgress:    jsonRows(finishedRoot),
				members:       finishedMembers,
				assignedReady: jsonRows(`{"id":"ready-1","status":"open","assignee":"sess-1","metadata":{"gc.kind":"task"}}`),
			},
			want: []string{"ready-1"},
		},
		{
			name: "pool demand",
			env:  ephemeralOrigin,
			fake: workflowRootFake{
				inProgress: jsonRows(finishedRoot),
				members:    finishedMembers,
				pool:       jsonRows(`{"id":"pool-1","status":"open","metadata":{"gc.routed_to":"hello-world/worker"}}`),
			},
			want: []string{"pool-1"},
		},
		{
			name: "nothing else, through the fallback",
			env:  ephemeralOrigin,
			fake: workflowRootFake{inProgress: jsonRows(finishedRoot), members: finishedMembers},
		},
		{
			name: "nothing else, through the origin gate",
			env:  namedOrigin,
			fake: workflowRootFake{inProgress: jsonRows(finishedRoot), members: finishedMembers},
		},
		{
			name: "live anchor under the alias, through the fallback",
			env:  map[string]string{"GC_SESSION_ID": "sess-1", "GC_ALIAS": "alias-1", "GC_SESSION_ORIGIN": "ephemeral"},
			fake: workflowRootFake{inProgress: jsonRows(finishedRoot, liveRoot), members: members(liveMembers...)},
			want: []string{"wf-root-2"},
		},
		{
			name: "live anchor under the alias, through the origin gate",
			env:  map[string]string{"GC_SESSION_ID": "sess-1", "GC_ALIAS": "alias-1", "GC_SESSION_ORIGIN": "named"},
			fake: workflowRootFake{inProgress: jsonRows(finishedRoot, liveRoot), members: members(liveMembers...)},
			want: []string{"wf-root-2"},
		},
		{
			name: "live anchor behind it under the same identity, through the fallback",
			env:  ephemeralOrigin,
			fake: workflowRootFake{inProgress: jsonRows(finishedRoot, ownLiveRoot), members: members(liveMembers...)},
			want: []string{"wf-root-2"},
		},
		{
			name: "live anchor behind it under the same identity, through the origin gate",
			env:  namedOrigin,
			fake: workflowRootFake{inProgress: jsonRows(finishedRoot, ownLiveRoot), members: members(liveMembers...)},
			want: []string{"wf-root-2"},
		},
		{
			name: "ready step of the live anchor behind it",
			env:  ephemeralOrigin,
			fake: workflowRootFake{
				inProgress:  jsonRows(finishedRoot, ownLiveRoot),
				members:     members(liveMembers...),
				anchorReady: jsonRows(ownLiveReadyStep),
			},
			want: []string{"live-ready"},
		},
		{
			name: "in_progress step behind a finished root with no contract stamp",
			env:  ephemeralOrigin,
			fake: workflowRootFake{inProgress: jsonRows(contractlessFinishedRoot, otherStep), members: finishedMembers},
			want: []string{"other-step"},
		},
		{
			name: "in_progress step behind a finished root with a padded kind",
			env:  ephemeralOrigin,
			fake: workflowRootFake{inProgress: jsonRows(paddedKindFinishedRoot, otherStep), members: finishedMembers},
			want: []string{"other-step"},
		},
		{
			name: "live anchor behind a finished root with no contract stamp",
			env:  ephemeralOrigin,
			fake: workflowRootFake{inProgress: jsonRows(contractlessFinishedRoot, ownLiveRoot), members: members(liveMembers...)},
			want: []string{"wf-root-2"},
		},
	} {
		for _, form := range combinedQueryForms() {
			t.Run(tc.name+"/"+form.name, func(t *testing.T) {
				ids := runCombinedQueryForm(t, form, tc.env, tc.fake)
				if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("combined query served %v, want %v", ids, tc.want)
				}
			})
		}
	}
}

// TestCombinedWorkQueryKeepsLiveWorkflowRootWithNoReadyStep pins that "no
// ready step" is not "done": a live root whose open step is not ready is still
// emitted by the anchor fallback.
func TestCombinedWorkQueryKeepsLiveWorkflowRootWithNoReadyStep(t *testing.T) {
	fake := workflowRootFake{
		inProgress: jsonRows(workflowRootJSON("wf-root-1", ``)),
		members: jsonRows(memberJSON("step-a", "wf-root-1", "closed", "task"),
			memberJSON("step-b", "wf-root-1", "open", "task")),
	}
	for _, form := range combinedQueryForms() {
		t.Run(form.name, func(t *testing.T) {
			ids := runCombinedQueryForm(t, form, map[string]string{"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral"}, fake)
			if len(ids) != 1 || ids[0] != "wf-root-1" {
				t.Fatalf("live workflow root with no ready step was not emitted by the fallback: %v", ids)
			}
		})
	}
}

// TestInProgressTierLooksBehindAFinishedWorkflowRoot covers the
// assigned-in-progress-only query's look-behind: a head it passes over as a
// finished workflow root does not hide the session's next in_progress row,
// whatever the root's contract stamp or kind padding, in every form of the
// tier. A graph.v2 root behind it is served as this tier serves every root.
func TestInProgressTierLooksBehindAFinishedWorkflowRoot(t *testing.T) {
	otherStep := `{"id":"other-step","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"task"}}`
	finishedMembers := finishedMemberShapes("wf-root-1")["finalizer open"]
	withLiveRoot := jsonRows(memberJSON("step-a", "wf-root-1", "closed", "task"),
		memberJSON("fin", "wf-root-1", "open", "workflow-finalize"),
		memberJSON("live-a", "wf-root-2", "closed", "task"),
		memberJSON("live-b", "wf-root-2", "open", "task"))
	for _, tc := range []struct {
		name, head, behind, members, want string
	}{
		{"graph.v2 root, step behind", workflowRootJSON("wf-root-1", ``), otherStep, finishedMembers, "other-step"},
		{"graph.v2 root, live root behind", workflowRootJSON("wf-root-1", ``), workflowRootJSON("wf-root-2", ``), withLiveRoot, "wf-root-2"},
		{"root with no contract stamp, step behind", contractlessFinishedRoot, otherStep, finishedMembers, "other-step"},
		{"root with a padded kind, step behind", paddedKindFinishedRoot, otherStep, finishedMembers, "other-step"},
	} {
		for _, form := range inProgressTierForms() {
			t.Run(tc.name+"/"+form.name, func(t *testing.T) {
				run := runInProgressTierForm(t, form, workflowRootFake{inProgress: jsonRows(tc.head, tc.behind), members: tc.members})
				if ids := run.ids(t); len(ids) != 1 || ids[0] != tc.want {
					t.Fatalf("tier served %v, want %s behind the finished root (stdout %q)", ids, tc.want, run.stdout)
				}
			})
		}
	}
}

// isClosedMemberRead reports whether a logged member read is the
// closed-evidence read.
func isClosedMemberRead(call string) bool {
	return strings.Contains(call, "--status=closed") || strings.Contains(call, "--status closed")
}

// wantMemberReads is how many member reads a root that no live read shows live
// costs: every live read, then the closed one.
func wantMemberReads(federated bool) int {
	if federated {
		return 4
	}
	return 2
}

// TestWorkQueryReadsWorkflowMembersOnlyWhereTheVerdictDecides pins the cost
// side of the finished-root gate. The combined query reads a remembered
// anchor's members only once the anchor is about to be emitted, so a live root
// whose step a tier serves costs no member read. The reads go live-first, so a
// live root that is emitted costs one, and the closed-evidence read runs last,
// for a root no live read shows live; a root the gate judges is judged once.
func TestWorkQueryReadsWorkflowMembersOnlyWhereTheVerdictDecides(t *testing.T) {
	liveRoot := workflowRootJSON("wf-root-1", ``)
	liveMembers := jsonRows(memberJSON("step-a", "wf-root-1", "closed", "task"), memberJSON("step-b", "wf-root-1", "open", "task"))
	ownStep := `{"id":"own-step","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"task","gc.root_bead_id":"wf-root-1"}}`
	readyStep := `{"id":"ready-step","status":"open","metadata":{"gc.kind":"task","gc.root_bead_id":"wf-root-1"}}`
	otherStep := `{"id":"other-step","status":"in_progress","assignee":"sess-1","metadata":{"gc.kind":"task"}}`
	env := map[string]string{"GC_SESSION_ID": "sess-1", "GC_SESSION_ORIGIN": "ephemeral"}
	for _, tc := range []struct {
		name      string
		fake      workflowRootFake
		want      string
		wantReads func(federated bool) int
	}{
		{
			name:      "the live root's in_progress step is served",
			fake:      workflowRootFake{inProgress: jsonRows(liveRoot, ownStep), members: liveMembers},
			want:      "own-step",
			wantReads: func(bool) int { return 0 },
		},
		{
			name:      "the live root's ready step is served",
			fake:      workflowRootFake{inProgress: jsonRows(liveRoot), members: liveMembers, anchorReady: jsonRows(readyStep)},
			want:      "ready-step",
			wantReads: func(bool) int { return 0 },
		},
		{
			name:      "the live root itself is emitted",
			fake:      workflowRootFake{inProgress: jsonRows(liveRoot), members: liveMembers},
			want:      "wf-root-1",
			wantReads: func(bool) int { return 1 },
		},
		{
			name:      "a finished root with no contract stamp is judged once",
			fake:      workflowRootFake{inProgress: jsonRows(contractlessFinishedRoot, otherStep), members: finishedMemberShapes("wf-root-1")["finalizer open"]},
			want:      "other-step",
			wantReads: wantMemberReads,
		},
	} {
		for _, form := range combinedQueryForms() {
			t.Run(tc.name+"/"+form.name, func(t *testing.T) {
				fake := tc.fake
				fake.ephemeral = form.ephemeral
				run := runWorkflowRootQuery(t, form.command, env, fake)
				if run.exit != 0 {
					t.Fatalf("query exited %d; stderr=%s", run.exit, run.stderr)
				}
				if ids := run.ids(t); len(ids) != 1 || ids[0] != tc.want {
					t.Fatalf("query served %v, want %s", ids, tc.want)
				}
				federated := form.name == "federated"
				reads := run.memberReads()
				if len(reads) != tc.wantReads(federated) {
					t.Fatalf("query made %d member reads, want %d: %v", len(reads), tc.wantReads(federated), reads)
				}
				if tc.wantReads(federated) == 1 && isClosedMemberRead(reads[0]) {
					t.Fatalf("a live root's one member read is the closed-evidence read, want a live read first: %v", reads)
				}
			})
		}
	}
	for _, form := range inProgressTierForms() {
		federated := form.name == "federated"
		t.Run("in_progress tier serves a live root on one live read/"+form.name, func(t *testing.T) {
			run := runInProgressTierForm(t, form, workflowRootFake{inProgress: jsonRows(liveRoot), members: liveMembers})
			if ids := run.ids(t); len(ids) != 1 || ids[0] != "wf-root-1" {
				t.Fatalf("tier served %v, want the live root", ids)
			}
			if reads := run.memberReads(); len(reads) != 1 || isClosedMemberRead(reads[0]) {
				t.Fatalf("member reads %v, want one live read", reads)
			}
		})
		t.Run("in_progress tier reads closed evidence last/"+form.name, func(t *testing.T) {
			run := runInProgressTierForm(t, form, workflowRootFake{
				inProgress: jsonRows(workflowRootJSON("wf-root-1", ``)),
				members:    finishedMemberShapes("wf-root-1")["finalizer open"],
			})
			if ids := run.ids(t); len(ids) != 0 {
				t.Fatalf("tier served %v, want the finished root passed over", ids)
			}
			reads := run.memberReads()
			if len(reads) != wantMemberReads(federated) {
				t.Fatalf("member reads %v, want %d", reads, wantMemberReads(federated))
			}
			for i, read := range reads {
				if isClosedMemberRead(read) != (i == len(reads)-1) {
					t.Fatalf("member reads %v, want every live read before the one closed-evidence read", reads)
				}
			}
		})
	}
}

// TestWorkflowWorkDoneMatchesTheControllerVerdict feeds one member corpus to
// the hook's generated gc_workflow_work_done, through the in_progress tier in
// both reader forms, and to molecule.WorkflowAwaitingFinalize, the controller's
// copy of the verdict. They must agree. A root the controller stops waking its
// holder for must be one the hook passes over, and a root the hook passes over
// must not wake its holder, or the controller wakes a session the hook then
// serves nothing (internal/config/AGENTS.md). Reader bounds are left out: a
// saturated member window reads as live to the hook alone, which only keeps a
// root served that the controller lets sleep.
func TestWorkflowWorkDoneMatchesTheControllerVerdict(t *testing.T) {
	type member struct{ id, root, status, kind string }
	const root = "wf-root-1"
	closedStep := member{"step-a", root, "closed", "task"}
	openFinalizer := member{"fin", root, "open", beadmeta.KindWorkflowFinalize}
	corpus := map[string][]member{
		"all closed":               {closedStep, {"fin", root, "closed", beadmeta.KindWorkflowFinalize}},
		"finalizer open":           {closedStep, openFinalizer},
		"latches open":             {closedStep, openFinalizer, {"scope", root, "open", beadmeta.KindScope}, {"spec", root, "open", beadmeta.KindSpec}},
		"padded finalizer kind":    {closedStep, {"fin", root, "open", " workflow-finalize "}},
		"padded closed step kind":  {{"step-a", root, "closed", " task "}, openFinalizer},
		"open step":                {closedStep, {"step-b", root, "open", "task"}, openFinalizer},
		"in_progress step":         {closedStep, {"step-b", root, "in_progress", "task"}, openFinalizer},
		"blocked step":             {closedStep, {"step-b", root, "blocked", "task"}, openFinalizer},
		"open retry":               {closedStep, {"retry", root, "open", beadmeta.KindRetry}, openFinalizer},
		"open nested workflow":     {closedStep, {"nested", root, "open", beadmeta.KindWorkflow}},
		"root only":                nil,
		"fresh":                    {{"step-a", root, "open", "task"}, openFinalizer},
		"only topology closed":     {{"scope", root, "closed", beadmeta.KindScope}, {"spec", root, "closed", beadmeta.KindSpec}, openFinalizer},
		"another root's open step": {closedStep, openFinalizer, {"other", "wf-root-9", "open", "task"}},
		"the root's own row":       {closedStep, openFinalizer, {root, root, "in_progress", beadmeta.KindWorkflow}},
	}
	rootBead := beads.Bead{ID: root, Status: "in_progress", Assignee: "sess-1", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaContractMetadataKey: "graph.v2",
	}}
	forms := inProgressTierForms()
	for name, members := range corpus {
		rows := make([]string, 0, len(members))
		memberBeads := make([]beads.Bead, 0, len(members))
		for _, m := range members {
			rows = append(rows, memberJSON(m.id, m.root, m.status, m.kind))
			memberBeads = append(memberBeads, beads.Bead{ID: m.id, Status: m.status, Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey: m.kind, beadmeta.RootBeadIDMetadataKey: m.root,
			}})
		}
		awaiting := molecule.WorkflowAwaitingFinalize(rootBead, memberBeads)
		for _, form := range []inProgressTierForm{forms[0], forms[2]} {
			t.Run(name+"/"+form.name, func(t *testing.T) {
				run := runInProgressTierForm(t, form, workflowRootFake{inProgress: jsonRows(workflowRootJSON(root, ``)), members: jsonRows(rows...)})
				ids := run.ids(t)
				served := len(ids) == 1 && ids[0] == root
				if served == awaiting {
					t.Fatalf("hook served the root = %v, but the controller's WorkflowAwaitingFinalize = %v; the two verdicts must agree", served, awaiting)
				}
			})
		}
	}
}
