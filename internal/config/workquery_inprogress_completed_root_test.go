package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// members holds every row stamped gc.root_bead_id=<root>, whatever status.
	members string
	// memberRead is "" for a normal member read, "fail" for one that exits 1,
	// and "garbage" for one that prints output jq cannot parse.
	memberRead string
	// anchorReady answers the anchor-ready probe (a ready read on
	// gc.root_bead_id without --status).
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
statuses=""; limit=""; assignee=""; prev=""
for a in "$@"; do
  case "$prev" in --status) statuses="$a" ;; --limit) limit="$a" ;; esac
  case "$a" in
    --status=*) statuses="${a#--status=}" ;;
    --limit=*) limit="${a#--limit=}" ;;
    --assignee=*) assignee="${a#--assignee=}" ;;
  esac
  prev="$a"
done
filter_rows() {
  printf '%s' "$1" | jq -c --arg s "$statuses" --arg l "$limit" --arg a "$assignee" '
    [.[] | select(if $s == "" then .status != "closed" else (.status as $st | $s | split(",") | index($st)) != null end)
         | select($a == "" or .assignee == $a)]
    | if ($l | length) > 0 and ($l | tonumber) > 0 then .[:($l | tonumber)] else . end'
}
case "$*" in
  *gc.root_bead_id=*)
    if [ "$1" = list ] || [ -n "$statuses" ]; then
      ` + memberRead + `
    else
      printf '%s' "$anchor_ready"
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
