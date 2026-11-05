package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestParseControlReadyQueryRecognizesGeneratedQuery(t *testing.T) {
	// Dir+Name shaped so QualifiedName is a rig-scoped, binding-qualified
	// name ("fixture/core.control-dispatcher"): this is the only shape that
	// produces a non-empty bare-route alias (see TestControlDispatcherBareRoute).
	query := workflowServeControlReadyQuery(config.Agent{Name: "core.control-dispatcher", Dir: "fixture"}, "gascity--control-dispatcher")
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized: %q", query)
	}
	if parsed.target != "fixture/core.control-dispatcher" {
		t.Errorf("target = %q, want %q", parsed.target, "fixture/core.control-dispatcher")
	}
	if parsed.controlSessionName != "gascity--control-dispatcher" {
		t.Errorf("controlSessionName = %q, want %q", parsed.controlSessionName, "gascity--control-dispatcher")
	}
	if parsed.bareTarget != "fixture/control-dispatcher" {
		t.Errorf("bareTarget = %q, want %q", parsed.bareTarget, "fixture/control-dispatcher")
	}
}

func TestParseControlReadyQueryRejectsNonControlQuery(t *testing.T) {
	for _, q := range []string{
		"",
		"bd ready --json --limit=20",
		"GC_CONTROL_TARGET=core.control-dispatcher sh -c 'bd ready'", // missing the BD_EXPORT_AUTO=false marker prefix
	} {
		if _, ok := parseControlReadyQuery(q); ok {
			t.Errorf("parseControlReadyQuery(%q) = ok, want not recognized", q)
		}
	}
}

func TestControlReadyCandidatesPrecedenceDedupAndLegacyExpansion(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}

	got := controlReadyCandidates(parsed, envList)
	want := []string{
		"gascity--control-dispatcher",
		"gascity--workflow-control",
		"gascity/control-dispatcher",
		"gascity/workflow-control",
	}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("controlReadyCandidates = %#v, want %#v", got, want)
	}
}

func TestControlReadyCandidatesSkipsEmptySlots(t *testing.T) {
	// "control-dispatcher" itself ends in the literal suffix "control-dispatcher",
	// so it also produces the bare "workflow-control" legacy variant.
	parsed := parsedControlReadyQuery{target: "control-dispatcher"}
	got := controlReadyCandidates(parsed, nil)
	want := []string{"control-dispatcher", "workflow-control"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("controlReadyCandidates = %#v, want %#v", got, want)
	}
}

func TestControlReadyRoutesFiltersEmptyAliases(t *testing.T) {
	parsed := parsedControlReadyQuery{target: "core.control-dispatcher", bareTarget: "control-dispatcher"}
	got := controlReadyRoutes(parsed)
	want := []string{"core.control-dispatcher", "control-dispatcher"}
	if !stringSlicesEqual(got, want) {
		t.Fatalf("controlReadyRoutes = %#v, want %#v", got, want)
	}
}

func TestFilterReadyByAssigneeExcludesEpicAndOtherAssignees(t *testing.T) {
	ready := []beads.Bead{
		{ID: "ga-epic-leak", Assignee: "cand", Type: "epic"},
		{ID: "ga-ready", Assignee: "cand", Type: "task"},
		{ID: "ga-other", Assignee: "someone-else", Type: "task"},
	}
	got := filterReadyByAssignee(ready, "cand", workflowServeScanLimit)
	if len(got) != 1 || got[0].ID != "ga-ready" {
		t.Fatalf("filterReadyByAssignee = %#v, want only ga-ready", got)
	}
}

func TestFilterReadyByAssigneeRespectsLimit(t *testing.T) {
	ready := make([]beads.Bead, 0, 5)
	for i := 0; i < 5; i++ {
		ready = append(ready, beads.Bead{ID: strings.Repeat("z", i+1), Assignee: "cand", Type: "task"})
	}
	got := filterReadyByAssignee(ready, "cand", 2)
	if len(got) != 2 {
		t.Fatalf("filterReadyByAssignee len = %d, want 2", len(got))
	}
}

func TestFilterReadyByRouteRequiresUnassignedAndSortsOldestFirst(t *testing.T) {
	newer := time.Unix(200, 0)
	older := time.Unix(100, 0)
	ready := []beads.Bead{
		{ID: "ga-assigned-routed", CreatedAt: older, Assignee: "someone", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-newer", CreatedAt: newer, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-older", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-epic-routed", CreatedAt: older, Type: "epic", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-other-route", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "other"}},
	}
	got := filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, "core/control-dispatcher")
	want := []string{"ga-older", "ga-newer"}
	if !stringSlicesEqual(beadIDs(got), want) {
		t.Fatalf("filterReadyByRoute = %#v, want %#v", beadIDs(got), want)
	}
}

func TestMergeControlReadyGroupsDedupsPreservingFirstOccurrence(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "ga-z-assigned"},
		{ID: "ga-dup", Metadata: map[string]string{"source": "assigned"}},
	}
	runTargetRouted := []beads.Bead{
		{ID: "ga-a-routed"},
		{ID: "ga-route-dup", Metadata: map[string]string{"source": "run-target"}},
	}
	routedToRouted := []beads.Bead{
		{ID: "ga-route-dup", Metadata: map[string]string{"source": "routed-to"}},
	}

	got := mergeControlReadyGroups(assigned, runTargetRouted, routedToRouted)
	wantIDs := []string{"ga-z-assigned", "ga-dup", "ga-a-routed", "ga-route-dup"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("mergeControlReadyGroups ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
	for _, b := range got {
		if b.ID == "ga-route-dup" && b.Metadata["source"] != "run-target" {
			t.Fatalf("ga-route-dup source = %q, want first-seen %q", b.Metadata["source"], "run-target")
		}
	}
}

func TestMergeControlReadyGroupsSkipsInstantiatingWithoutMarkingSeen(t *testing.T) {
	assigned := []beads.Bead{
		{ID: "ga-instantiating-assigned", Metadata: map[string]string{beadmeta.InstantiatingMetadataKey: "true"}},
		{ID: "ga-assigned", Metadata: map[string]string{"gc.kind": "retry"}},
	}
	runTargetRouted := []beads.Bead{
		{ID: "ga-instantiating-routed", Metadata: map[string]string{beadmeta.InstantiatingMetadataKey: "true"}},
		{ID: "ga-routed", Metadata: map[string]string{"gc.kind": "scope-check"}},
	}
	// A later group re-surfacing the SAME id without the instantiating tag
	// must still be admitted -- the shell's jq reduce never marks an
	// instantiating occurrence as "seen".
	laterNonInstantiating := []beads.Bead{
		{ID: "ga-instantiating-assigned", Metadata: map[string]string{"gc.kind": "now-real"}},
	}

	got := mergeControlReadyGroups(assigned, runTargetRouted, laterNonInstantiating)
	wantIDs := []string{"ga-assigned", "ga-routed", "ga-instantiating-assigned"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("mergeControlReadyGroups ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
}

// TestEvaluateControlReadyMatchesShellQueryPriority ports
// TestWorkflowServeControlReadyQueryPreservesQueryPriorityWhenMerging's
// scenario (cmd_convoy_dispatch_test.go) at the Go level: given the same
// parsed query + env, and a ready set shaped like what the per-leg Ready
// reads or the batched fallback would return, evaluateControlReady must merge candidates
// before routes and drop later ID duplicates exactly like the shell's jq
// reduce does.
func TestEvaluateControlReadyMatchesShellQueryPriority(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}
	ready := []beads.Bead{
		{ID: "ga-z-assigned", Assignee: "gascity--control-dispatcher"},
		{ID: "ga-dup", Assignee: "gascity--control-dispatcher", Metadata: map[string]string{"source": "assigned"}},
		{ID: "ga-a-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher"}},
		{ID: "ga-route-dup", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher", "source": "run-target"}},
		{ID: "ga-route-dup-2", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher"}},
	}
	// ga-route-dup also appears as a routed_to match with different content;
	// the run_target occurrence (checked first) must win.
	ready = append(ready, beads.Bead{ID: "ga-route-dup", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gascity/control-dispatcher", "source": "routed-to"}})

	got := evaluateControlReady(ready, parsed, envList)
	wantIDs := []string{"ga-z-assigned", "ga-dup", "ga-a-routed", "ga-route-dup", "ga-route-dup-2"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("evaluateControlReady ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
	for _, b := range got {
		if b.ID == "ga-route-dup" && b.Metadata["source"] != "run-target" {
			t.Fatalf("ga-route-dup source = %q, want first-seen %q", b.Metadata["source"], "run-target")
		}
	}
}

func TestEvaluateControlReadyExcludesEpicAndInstantiating(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: not recognized")
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}
	ready := []beads.Bead{
		{ID: "ga-epic-leak", Assignee: "gascity--control-dispatcher", Type: "epic"},
		{ID: "ga-ready", Assignee: "gascity--control-dispatcher", Type: "task"},
		{ID: "ga-instantiating-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher", beadmeta.InstantiatingMetadataKey: "true"}},
		{ID: "ga-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher", "gc.kind": "scope-check"}},
	}

	got := evaluateControlReady(ready, parsed, envList)
	wantIDs := []string{"ga-ready", "ga-routed"}
	if !stringSlicesEqual(beadIDs(got), wantIDs) {
		t.Fatalf("evaluateControlReady ids = %#v, want %#v", beadIDs(got), wantIDs)
	}
}

func beadIDs(items []beads.Bead) []string {
	out := make([]string, len(items))
	for i, b := range items {
		out[i] = b.ID
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- End-to-end: nextWorkflowServeBeads wiring (per-leg Ready + fallback) ---

// setUpControlReadyFileStoreCity builds a scope-local FileStore-backed city
// so tryControlReadyScan's leg read answers in-process without any bd/dolt
// process at all, and returns the opened
// store for seeding fixture beads directly.
func setUpControlReadyFileStoreCity(t *testing.T) (cityDir string, store *beads.FileStore) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir = t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore: %v", err)
	}
	return cityDir, store
}

// noBDOnPathForTest ensures no bd (or bd stub) is reachable via PATH, so a
// test can prove a code path made zero subprocess calls: any shell-out would
// fail with "command not found" rather than silently succeeding.
func noBDOnPathForTest(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestTryControlReadyScanAnswersFileLegWithZeroSubprocessCalls(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	target := "gascity/control-dispatcher"
	ready, err := store.Create(beads.Bead{Assignee: target, Type: "task"})
	if err != nil {
		t.Fatalf("create ready bead: %v", err)
	}
	epic, err := store.Create(beads.Bead{Assignee: target, Type: "epic"})
	if err != nil {
		t.Fatalf("create epic bead: %v", err)
	}
	routed, err := store.Create(beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target}})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	queue, handled, err := tryControlReadyScan(query, cityDir, nil)
	if err != nil {
		t.Fatalf("tryControlReadyScan: %v", err)
	}
	if !handled {
		t.Fatalf("tryControlReadyScan: handled = false, want true for a control-ready query")
	}

	var gotIDs []string
	for _, b := range queue {
		gotIDs = append(gotIDs, b.ID)
	}
	wantIDs := []string{ready.ID, routed.ID}
	if !stringSlicesEqual(gotIDs, wantIDs) {
		t.Fatalf("queue ids = %#v, want %#v (epic bead %s must be excluded)", gotIDs, wantIDs, epic.ID)
	}
}

func TestTryControlReadyScanReturnsUnhandledForNonControlQuery(t *testing.T) {
	cityDir := t.TempDir()
	_, handled, err := tryControlReadyScan("bd ready --json --limit=20", cityDir, nil)
	if handled {
		t.Fatalf("handled = true, want false for a non-control-ready query")
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestTryControlReadyScanMakesOneBDCallPerBdLeg pins the scan's cost on a
// bd-backed scope (ga-vnycm2.19): the whole tick is one `bd ready` call -- not
// the shell script's N per-candidate/route calls, and not the four-list prime
// plus ready projection the snapshot it replaced paid on every scan.
func TestTryControlReadyScanMakesOneBDCallPerBdLeg(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "bd.log")
	bdPath := filepath.Join(tmp, "bd")
	target := "gascity/control-dispatcher"
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> "%s"
case "$1" in
  ready)
    printf '[{"id":"ga-ready","status":"open","issue_type":"task","assignee":"%s"}]'
    ;;
  *)
    printf '[]'
    ;;
esac
`, logPath, target)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	queue, handled, err := tryControlReadyScan(query, cityDir, nil)
	if err != nil {
		t.Fatalf("tryControlReadyScan: %v", err)
	}
	if !handled {
		t.Fatalf("handled = false, want true")
	}
	if len(queue) != 1 || queue[0].ID != "ga-ready" {
		t.Fatalf("queue = %#v, want single ga-ready bead", queue)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "ready ") || !strings.Contains(calls[0], "--include-ephemeral") {
		t.Fatalf("bd calls = %q, want exactly one `bd ready --include-ephemeral` call per scan", calls)
	}
}

// TestControlReadyLegReadyLogsWhenResultHitsLimit is ga-bbj6wv Finding 1: a
// leg that comes back at exactly controlReadyScanLimit is a truncation signal
// (some candidate/route may have been starved of ready beads that exist but
// didn't fit) and must be observable, not silent.
func TestControlReadyLegReadyLogsWhenResultHitsLimit(t *testing.T) {
	store := beads.NewMemStore()
	for i := 0; i < controlReadyScanLimit; i++ {
		if _, err := store.Create(beads.Bead{Title: fmt.Sprintf("ready-%d", i), Type: "task"}); err != nil {
			t.Fatalf("seed ready bead %d: %v", i, err)
		}
	}

	var logBuf bytes.Buffer
	restore := captureLogOutput(&logBuf)
	defer restore()

	dir := t.TempDir()
	result, err := controlReadyLegReady(dir, store)
	if err != nil {
		t.Fatalf("controlReadyLegReady: %v", err)
	}
	if len(result) != controlReadyScanLimit {
		t.Fatalf("len(result) = %d, want %d", len(result), controlReadyScanLimit)
	}
	if !strings.Contains(logBuf.String(), "may be truncated") {
		t.Fatalf("expected a truncation warning in log output, got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), dir) {
		t.Fatalf("expected log to name the dir %q, got: %q", dir, logBuf.String())
	}
}

// TestControlReadyLegReadyNoWarningBelowLimit is the negative case: a leg below
// the limit is a complete result, not a truncation signal, and must not log
// anything.
func TestControlReadyLegReadyNoWarningBelowLimit(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "only", Type: "task"}); err != nil {
		t.Fatalf("seed ready bead: %v", err)
	}

	var logBuf bytes.Buffer
	restore := captureLogOutput(&logBuf)
	defer restore()

	dir := t.TempDir()
	result, err := controlReadyLegReady(dir, store)
	if err != nil {
		t.Fatalf("controlReadyLegReady: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if logBuf.Len() != 0 {
		t.Fatalf("expected no log output below the limit, got: %q", logBuf.String())
	}
}

func TestNextWorkflowServeBeadsNonControlQueryUsesOriginalShellPath(t *testing.T) {
	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(`#!/bin/sh
printf '[{"id":"ga-plain"}]'
`), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := nextWorkflowServeBeads("bd ready --json --limit=20", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("nextWorkflowServeBeads: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ga-plain" {
		t.Fatalf("nextWorkflowServeBeads = %#v, want [{ga-plain}]", got)
	}
}

// installRecordingFakeBd puts a `bd` on PATH that appends one line per
// invocation to the returned log and answers an empty ready set. A control-ready
// scan never shells out to bd, so the log must stay empty.
func installRecordingFakeBd(t *testing.T) (invocations func() []string) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "bd.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nprintf '[]'\n", logPath)
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")
	return func() []string {
		data, err := os.ReadFile(logPath)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("read fake bd log: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// TestControlReadyScanUnopenableStoreFailsLoudWithoutShelling pins the ruling
// that deleted the shell reader: a scope whose ledger legs cannot be opened
// fails the scan loudly. It must not degrade to a `bd ready` shell-out, which
// is blind to the gc.work_outcome=blocked veto and so would release a control
// bead whose blocker closed blocked.
func TestControlReadyScanUnopenableStoreFailsLoudWithoutShelling(t *testing.T) {
	invocations := installRecordingFakeBd(t)
	openErr := errors.New("ledger cannot be opened")
	installControlReadyLegSourcesFn(t, func(string, string, *config.City) ([]beads.Store, []beads.Store, error) {
		return nil, nil, openErr
	})
	dir := t.TempDir()
	query := workflowServeControlReadyQueryForBeads(config.Agent{Name: config.ControlDispatcherAgentName}, config.BeadsConfig{})

	queue, handled, err := tryControlReadyScan(query, dir, nil)
	if !handled || len(queue) != 0 || !errors.Is(err, openErr) || !strings.Contains(err.Error(), dir) {
		t.Errorf("tryControlReadyScan = (%d item(s), handled %t, err %v), want (0, true, an error wrapping the open error and naming %s)", len(queue), handled, err, dir)
	}

	got, err := nextWorkflowServeBeads(query, dir, nil)
	if len(got) != 0 || !errors.Is(err, openErr) || !strings.Contains(err.Error(), dir) {
		t.Errorf("nextWorkflowServeBeads = (%d item(s), err %v), want (0, an error wrapping the open error and naming %s)", len(got), err, dir)
	}

	if calls := invocations(); len(calls) != 0 {
		t.Errorf("bd was invoked %d time(s): %q; the scan must never shell out to bd", len(calls), calls)
	}
}

// TestControlReadyScanMalformedMarkerQueryFailsLoud pins that a query carrying
// the control-ready marker but no target is an error, not a query to hand to
// the shell: the scan owns every marker-prefixed work query, so one it cannot
// parse must fail loudly instead of running its bd script.
func TestControlReadyScanMalformedMarkerQueryFailsLoud(t *testing.T) {
	invocations := installRecordingFakeBd(t)
	dir := t.TempDir()
	query := controlReadyQueryMarkerPrefix + " sh -c 'bd ready --json'"

	queue, handled, err := tryControlReadyScan(query, dir, nil)
	if queue != nil || !handled || err == nil {
		t.Errorf("tryControlReadyScan = (%v, handled %t, err %v), want (nil, true, an error)", queue, handled, err)
	}

	got, err := nextWorkflowServeBeads(query, dir, nil)
	if got != nil || err == nil {
		t.Errorf("nextWorkflowServeBeads = (%v, err %v), want (nil, an error)", got, err)
	}

	if calls := invocations(); len(calls) != 0 {
		t.Errorf("bd was invoked %d time(s): %q; a malformed marker query must not reach the shell", len(calls), calls)
	}
}

// TestControlReadyScanHonorsTheWorkOutcomeVeto is the scan-level pin for the
// gc.work_outcome=blocked veto: a control bead routed to the dispatcher whose
// blocker closed blocked is not in the queue, while the same blocker closed
// with another outcome releases it.
func TestControlReadyScanHonorsTheWorkOutcomeVeto(t *testing.T) {
	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	route := agentCfg.QualifiedName()

	for _, tc := range []struct {
		name        string
		workOutcome string
		wantQueued  bool
	}{
		{name: "blocked blocker vetoes its dependent", workOutcome: beadmeta.WorkOutcomeBlocked, wantQueued: false},
		{name: "shipped blocker releases its dependent", workOutcome: beadmeta.WorkOutcomeShipped, wantQueued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(store))

			root, err := store.Create(beads.Bead{
				Title:    "workflow",
				Type:     "task",
				Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
			})
			if err != nil {
				t.Fatalf("create workflow root: %v", err)
			}
			blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
			if err != nil {
				t.Fatalf("create blocker: %v", err)
			}
			dependent := newRoutedControlBead(t, store, root.ID, route)
			if err := store.DepAdd(dependent.ID, blocker.ID, "blocks"); err != nil {
				t.Fatalf("add dependency: %v", err)
			}
			if err := store.SetMetadataBatch(blocker.ID, map[string]string{beadmeta.WorkOutcomeMetadataKey: tc.workOutcome}); err != nil {
				t.Fatalf("set work outcome: %v", err)
			}
			if err := store.Close(blocker.ID); err != nil {
				t.Fatalf("close blocker: %v", err)
			}

			queued := false
			for _, id := range controlReadyScan(t, cityPath, agentCfg, config.BeadsConfig{}) {
				if id == dependent.ID {
					queued = true
				}
			}
			if queued != tc.wantQueued {
				t.Errorf("dependent queued = %t, want %t with the blocker closed gc.work_outcome=%s", queued, tc.wantQueued, tc.workOutcome)
			}
		})
	}
}
