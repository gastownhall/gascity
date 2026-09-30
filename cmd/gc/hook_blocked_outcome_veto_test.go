package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// ga-id9fqz (ruling ga-mmmczu, option A): the hook's serve path adopts the veto
// the controller's demand side already enforces. A work-query candidate whose
// ready-blocking dependency is CLOSED with gc.work_outcome=blocked ("committed,
// not merged") is not servable — on the discovery door and on the claim door.
//
// These tests pin the decision, and also what the decision COSTS: the hook runs
// on every agent turn, and the store-side veto's read (List{IDs, Status:closed})
// scans the whole closed population — 118 MB, 3.3 s and 730 MB RSS on a 25k
// closed-bead store, against 80 KB and 0.4 s for one batched `bd show`. The
// veto must make exactly one batched by-ID read, and only when a candidate
// actually carries a ready-blocking edge.

// vetoBlocker is what the fake bd answers for one blocker bead.
type vetoBlocker struct {
	status  string
	outcome string
}

// vetoBd is the bd CLI behind a BdStore, reduced to the one read the veto may
// make. It answers `bd show --json <ids...>` from blockers and records every
// invocation, so a test asserts what the veto COSTS (which commands, how many,
// for which ids) and not only what it decides.
type vetoBd struct {
	blockers map[string]vetoBlocker
	showErr  error
	calls    [][]string
}

func (f *vetoBd) store() beads.ExactBatchGetter {
	return beads.NewBdStore("/city", f.run)
}

func (f *vetoBd) run(_, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 0 || args[0] != "show" {
		return nil, fmt.Errorf("unexpected bd call: %s %s", name, strings.Join(args, " "))
	}
	if f.showErr != nil {
		return nil, f.showErr
	}
	var issues []string
	for _, id := range args[1:] {
		if strings.HasPrefix(id, "-") {
			continue
		}
		b, ok := f.blockers[id]
		if !ok {
			continue
		}
		meta := ""
		if b.outcome != "" {
			meta = fmt.Sprintf(`,"metadata":{%q:%q}`, beadmeta.WorkOutcomeMetadataKey, b.outcome)
		}
		issues = append(issues, fmt.Sprintf(`{"id":%q,"title":"blocker","status":%q,"issue_type":"task","created_at":"2026-09-26T18:00:00Z"%s}`, id, b.status, meta))
	}
	return []byte("[" + strings.Join(issues, ",") + "]"), nil
}

// showedIDs returns the ids of every `bd show` call, one slice per call.
func (f *vetoBd) showedIDs() [][]string {
	var out [][]string
	for _, call := range f.calls {
		if len(call) < 2 || call[1] != "show" {
			continue
		}
		var ids []string
		for _, a := range call[2:] {
			if !strings.HasPrefix(a, "-") {
				ids = append(ids, a)
			}
		}
		out = append(out, ids)
	}
	return out
}

// nonShowCalls returns every bd call that was not a `bd show`.
func (f *vetoBd) nonShowCalls() [][]string {
	var out [][]string
	for _, call := range f.calls {
		if len(call) < 2 || call[1] != "show" {
			out = append(out, call)
		}
	}
	return out
}

type vetoEdge struct{ target, typ string }

// vetoRow renders one candidate the way live `bd ready --json` does: blocked_by
// is null (bd lists no OPEN blocker on a ready row) and the closed blocker rides
// in the typed `dependencies` array.
func vetoRow(id, status string, priority int, edges ...vetoEdge) string {
	deps := make([]string, 0, len(edges))
	for _, e := range edges {
		deps = append(deps, fmt.Sprintf(`{"issue_id":%q,"depends_on_id":%q,"type":%q,"metadata":"{}"}`, id, e.target, e.typ))
	}
	return fmt.Sprintf(`{"id":%q,"title":"dependent %s","status":%q,"issue_type":"task","priority":%d,"blocked_by":null,"dependency_count":%d,"dependencies":[%s]}`,
		id, id, status, priority, len(edges), strings.Join(deps, ","))
}

func vetoRows(rows ...string) string { return "[" + strings.Join(rows, ",") + "]" }

func vetoSurvivorIDs(t *testing.T, out string) []string {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a JSON array of objects: %v\n%s", err, out)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestFilterBlockedOutcomeHookCandidatesVetoDecision(t *testing.T) {
	blocked := vetoBlocker{"closed", beadmeta.WorkOutcomeBlocked}
	cases := []struct {
		name     string
		status   string
		edgeType string
		blocker  vetoBlocker
		wantKept bool
	}{
		{"blocks edge, closed, blocked outcome", "open", "blocks", blocked, false},
		{"waits-for edge, closed, blocked outcome", "open", "waits-for", blocked, false},
		{"conditional-blocks edge, closed, blocked outcome", "open", "conditional-blocks", blocked, false},
		{"closed, shipped outcome", "open", "blocks", vetoBlocker{"closed", beadmeta.WorkOutcomeShipped}, true},
		{"closed, legacy close with no typed outcome", "open", "blocks", vetoBlocker{"closed", ""}, true},
		{"closed, unrecognized future outcome fails open", "open", "blocks", vetoBlocker{"closed", "future-outcome"}, true},
		{"blocker still open is bd's verdict, not this veto's", "open", "blocks", vetoBlocker{"open", beadmeta.WorkOutcomeBlocked}, true},
		{"blocker in_progress is bd's verdict, not this veto's", "open", "blocks", vetoBlocker{"in_progress", beadmeta.WorkOutcomeBlocked}, true},
		{"parent-child edge is not ready-blocking", "open", "parent-child", blocked, true},
		{"related edge is not ready-blocking", "open", "related", blocked, true},
		{"discovered-from edge is not ready-blocking", "open", "discovered-from", blocked, true},
		{"tracks edge is not ready-blocking", "open", "tracks", blocked, true},
		{"in_progress candidate is a resume and is never vetoed", "in_progress", "blocks", blocked, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bd := &vetoBd{blockers: map[string]vetoBlocker{"ga-blocker": tc.blocker}}
			in := vetoRows(vetoRow("ga-dep", tc.status, 2, vetoEdge{"ga-blocker", tc.edgeType}))

			out, err := filterBlockedOutcomeHookCandidates(in, bd.store())
			if err != nil {
				t.Fatalf("filterBlockedOutcomeHookCandidates: %v", err)
			}
			kept := len(vetoSurvivorIDs(t, out)) == 1
			if kept != tc.wantKept {
				t.Fatalf("candidate kept = %v, want %v\nout: %s", kept, tc.wantKept, out)
			}
		})
	}
}

func TestFilterBlockedOutcomeHookCandidatesMakesOneBatchedReadKeyedByDistinctBlocker(t *testing.T) {
	bd := &vetoBd{blockers: map[string]vetoBlocker{
		"ga-b1": {"closed", beadmeta.WorkOutcomeShipped},
		"ga-b2": {"closed", beadmeta.WorkOutcomeBlocked},
	}}
	in := vetoRows(
		vetoRow("ga-a", "open", 2, vetoEdge{"ga-b1", "blocks"}, vetoEdge{"ga-b2", "waits-for"}),
		vetoRow("ga-b", "open", 2, vetoEdge{"ga-b1", "blocks"}),
		vetoRow("ga-c", "open", 2, vetoEdge{"ga-b9", "parent-child"}),
		vetoRow("ga-d", "in_progress", 2, vetoEdge{"ga-b7", "blocks"}),
	)

	out, err := filterBlockedOutcomeHookCandidates(in, bd.store())
	if err != nil {
		t.Fatalf("filterBlockedOutcomeHookCandidates: %v", err)
	}

	shows := bd.showedIDs()
	if len(shows) != 1 {
		t.Fatalf("bd show calls = %d, want exactly 1 batched read; calls: %v", len(shows), bd.calls)
	}
	got := append([]string(nil), shows[0]...)
	sort.Strings(got)
	if want := []string{"ga-b1", "ga-b2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batched ids = %v, want the distinct ready-blocking blockers of open candidates %v", got, want)
	}
	if other := bd.nonShowCalls(); len(other) != 0 {
		t.Fatalf("the veto issued non-show bd calls %v; a list/query would scan the whole closed population on every hook call", other)
	}
	if want := []string{"ga-b", "ga-c", "ga-d"}; !reflect.DeepEqual(vetoSurvivorIDs(t, out), want) {
		t.Fatalf("survivors = %v, want %v (ga-a waits on a handed-off blocker)", vetoSurvivorIDs(t, out), want)
	}
}

func TestFilterBlockedOutcomeHookCandidatesTouchesNoStoreWithoutReadyBlockingEdges(t *testing.T) {
	for name, in := range map[string]string{
		"no dependencies at all":         vetoRows(`{"id":"ga-a","status":"open","issue_type":"task"}`),
		"only a parent-child edge":       vetoRows(vetoRow("ga-a", "open", 2, vetoEdge{"ga-p", "parent-child"})),
		"only an in_progress candidate":  vetoRows(vetoRow("ga-a", "in_progress", 2, vetoEdge{"ga-b", "blocks"})),
		"an empty array":                 "[]",
		"a mix of the above":             vetoRows(vetoRow("ga-a", "open", 2, vetoEdge{"ga-p", "parent-child"}), vetoRow("ga-b", "in_progress", 2, vetoEdge{"ga-x", "blocks"})),
		"a non-ready-blocking edge kind": vetoRows(vetoRow("ga-a", "open", 2, vetoEdge{"ga-x", "related"})),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := filterBlockedOutcomeHookCandidates(in, nil)
			if err != nil {
				t.Fatalf("filterBlockedOutcomeHookCandidates: %v", err)
			}
			if out != in {
				t.Fatalf("output changed with nothing to veto:\n got %s\nwant %s", out, in)
			}
		})
	}
}

func TestFilterBlockedOutcomeHookCandidatesPassesThroughUndecodableInput(t *testing.T) {
	for name, in := range map[string]string{
		"empty":               "",
		"bd's no-work banner": "No ready work found",
		"not json":            "not json at all",
		"a lone object":       `{"id":"ga-a","status":"open"}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := filterBlockedOutcomeHookCandidates(in, nil)
			if err != nil {
				t.Fatalf("filterBlockedOutcomeHookCandidates: %v", err)
			}
			if out != in {
				t.Fatalf("output changed:\n got %q\nwant %q", out, in)
			}
		})
	}
}

// A blocker bd cannot answer for is no evidence of a handed-off close. The veto
// is a narrow override on bd's own verdict — it never re-blocks a row bd already
// offered on the strength of a dependency it could not read.
func TestFilterBlockedOutcomeHookCandidatesKeepsRowWhoseBlockerIsUnresolved(t *testing.T) {
	bd := &vetoBd{blockers: map[string]vetoBlocker{}}
	in := vetoRows(vetoRow("ga-a", "open", 2, vetoEdge{"ga-gone", "blocks"}))

	out, err := filterBlockedOutcomeHookCandidates(in, bd.store())
	if err != nil {
		t.Fatalf("filterBlockedOutcomeHookCandidates: %v", err)
	}
	if got := vetoSurvivorIDs(t, out); !reflect.DeepEqual(got, []string{"ga-a"}) {
		t.Fatalf("survivors = %v, want ga-a kept", got)
	}
}

// Failing open keeps a store hiccup from idling every seat in the city, but it
// must not be silent: the error comes back so the caller can report it.
func TestFilterBlockedOutcomeHookCandidatesFailsOpenAndReportsWhenBlockerReadFails(t *testing.T) {
	bd := &vetoBd{showErr: errors.New("dolt is down")}
	in := vetoRows(vetoRow("ga-a", "open", 2, vetoEdge{"ga-b", "blocks"}))

	out, err := filterBlockedOutcomeHookCandidates(in, bd.store())
	if err == nil {
		t.Fatal("blocker read failed but no error was returned")
	}
	if !strings.Contains(err.Error(), "dolt is down") {
		t.Fatalf("error %q does not carry the cause", err)
	}
	if out != in {
		t.Fatalf("output must be the input unchanged on a failed read:\n got %s\nwant %s", out, in)
	}
}

func TestFilterBlockedOutcomeHookCandidatesPreservesSurvivorsAndTheirOrder(t *testing.T) {
	bd := &vetoBd{blockers: map[string]vetoBlocker{"ga-b": {"closed", beadmeta.WorkOutcomeBlocked}}}
	survivor := `{"id":"ga-keep","status":"open","priority":1,"title":"keeps every field","labels":["ready-to-build"],"metadata":{"gc.routed_to":"gascity/builder"},"custom":{"nested":[1,2,3]}}`
	in := vetoRows(
		survivor,
		vetoRow("ga-drop", "open", 2, vetoEdge{"ga-b", "blocks"}),
		vetoRow("ga-tail", "open", 2),
	)

	out, err := filterBlockedOutcomeHookCandidates(in, bd.store())
	if err != nil {
		t.Fatalf("filterBlockedOutcomeHookCandidates: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got) != 2 || got[0]["id"] != "ga-keep" || got[1]["id"] != "ga-tail" {
		t.Fatalf("survivors = %v, want [ga-keep ga-tail] in input order", vetoSurvivorIDs(t, out))
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(survivor), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("survivor lost fields:\n got %v\nwant %v", got[0], want)
	}
}

// vetoCity is a city with one rig whose bead ids carry the ga- prefix and a city
// store (gm-), each a separate bd workspace. ownedStores fakes those workspaces:
// each answers only for the blockers it owns, so a read that reaches the wrong
// store is visible as a show call for ids that store does not own.
type vetoCity struct {
	path   string
	rigDir string
	cfg    *config.City
}

func newVetoCity(t *testing.T) vetoCity {
	t.Helper()
	path := t.TempDir()
	rigDir := filepath.Join(path, "gascity")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return vetoCity{
		path:   path,
		rigDir: rigDir,
		cfg:    &config.City{Workspace: config.Workspace{Name: "gc"}, Rigs: []config.Rig{{Name: "gascity", Path: rigDir, Prefix: "ga"}}},
	}
}

// legs is the fan-out a rig-scoped agent gets: its rig store, its own (also
// rig-scoped) env, and the city store.
func (c vetoCity) legs() []hookStore {
	return []hookStore{
		{dir: c.rigDir, env: []string{"BEADS_DIR=rig"}},
		{dir: c.rigDir, env: []string{"BEADS_DIR=own"}},
		{dir: c.path, env: []string{"BEADS_DIR=city"}},
	}
}

type ownedStores struct {
	byDir     map[string]*vetoBd
	opened    []string
	unbounded bool
}

func (o *ownedStores) open(ctx context.Context, dir string, _ []string) beads.ExactBatchGetter {
	o.opened = append(o.opened, dir)
	if _, ok := ctx.Deadline(); !ok {
		o.unbounded = true
	}
	bd, ok := o.byDir[dir]
	if !ok {
		bd = &vetoBd{}
	}
	return bd.store()
}

func (o *ownedStores) shows(dir string) [][]string {
	if bd, ok := o.byDir[dir]; ok {
		return bd.showedIDs()
	}
	return nil
}

func (c vetoCity) stores(rig, city map[string]vetoBlocker) *ownedStores {
	return &ownedStores{byDir: map[string]*vetoBd{
		c.rigDir: {blockers: rig},
		c.path:   {blockers: city},
	}}
}

func TestHookBlockerReaderReadsEachBlockerFromTheStoreThatOwnsIt(t *testing.T) {
	city := newVetoCity(t)
	handedOff := vetoBlocker{"closed", beadmeta.WorkOutcomeBlocked}
	stores := city.stores(
		map[string]vetoBlocker{"ga-1": handedOff, "ga-2": {"closed", ""}},
		map[string]vetoBlocker{"gm-9": handedOff},
	)
	reader := newHookBlockerReader(city.path, city.cfg, city.legs(), stores.open)

	found, unresolved, err := reader.GetExactBatch([]string{"ga-1", "gm-9", "ga-2"})
	if err != nil {
		t.Fatalf("GetExactBatch: %v", err)
	}
	if len(unresolved) != 0 || len(found) != 3 {
		t.Fatalf("found %d unresolved %v, want all three answered", len(found), unresolved)
	}
	rigShows, cityShows := stores.shows(city.rigDir), stores.shows(city.path)
	if len(rigShows) != 1 || !reflect.DeepEqual(sortedCopy(rigShows[0]), []string{"ga-1", "ga-2"}) {
		t.Fatalf("rig store shows = %v, want one batched read of the ga- blockers", rigShows)
	}
	if len(cityShows) != 1 || !reflect.DeepEqual(cityShows[0], []string{"gm-9"}) {
		t.Fatalf("city store shows = %v, want one read of the gm- blocker", cityShows)
	}
}

// The three legs of a rig-scoped agent run the same work query and, when it
// names its store (`gc bd --rig gascity ready`), return the same rows whatever
// each leg's own env points at. The blockers must still be read from the store
// that owns them, once, not once per leg from whichever store the leg names.
func TestHookBlockerReaderReadsASharedBatchOnceAcrossLegs(t *testing.T) {
	city := newVetoCity(t)
	stores := city.stores(map[string]vetoBlocker{"ga-1": {"closed", beadmeta.WorkOutcomeBlocked}}, nil)
	reader := newHookBlockerReader(city.path, city.cfg, city.legs(), stores.open)

	for i := 0; i < 3; i++ {
		found, _, err := reader.GetExactBatch([]string{"ga-1"})
		if err != nil || len(found) != 1 {
			t.Fatalf("call %d: found %d err %v", i, len(found), err)
		}
	}
	if shows := stores.shows(city.rigDir); len(shows) != 1 {
		t.Fatalf("rig store shows = %v, want the shared batch read exactly once across three legs", shows)
	}
	if shows := stores.shows(city.path); len(shows) != 0 {
		t.Fatalf("city store was asked for ga- blockers it does not own: %v", shows)
	}
}

func TestHookBlockerReaderRemembersAnAbsentBlocker(t *testing.T) {
	city := newVetoCity(t)
	stores := city.stores(map[string]vetoBlocker{}, nil)
	reader := newHookBlockerReader(city.path, city.cfg, city.legs(), stores.open)

	for i := 0; i < 2; i++ {
		found, unresolved, err := reader.GetExactBatch([]string{"ga-gone"})
		if err != nil || len(found) != 0 || !reflect.DeepEqual(unresolved, []string{"ga-gone"}) {
			t.Fatalf("call %d: found %v unresolved %v err %v, want ga-gone unresolved", i, found, unresolved, err)
		}
	}
	if shows := stores.shows(city.rigDir); len(shows) != 1 {
		t.Fatalf("rig store shows = %v, want the absent blocker read once (a failed lookup costs seconds)", shows)
	}
}

func TestHookBlockerReaderDoesNotReadABlockerNoLegOwns(t *testing.T) {
	city := newVetoCity(t)
	stores := city.stores(map[string]vetoBlocker{"ga-1": {"closed", ""}}, nil)
	reader := newHookBlockerReader(city.path, city.cfg, city.legs()[:1], stores.open)

	found, unresolved, err := reader.GetExactBatch([]string{"gm-9"})
	if err != nil || len(found) != 0 || !reflect.DeepEqual(unresolved, []string{"gm-9"}) {
		t.Fatalf("found %v unresolved %v err %v, want gm-9 unresolved", found, unresolved, err)
	}
	if len(stores.opened) != 0 {
		t.Fatalf("opened %v for a blocker whose store is not in the fan-out; a wrong-store read is a slow failure with no evidence", stores.opened)
	}
}

func TestHookBlockerReaderBoundsEveryReadWithATimeout(t *testing.T) {
	city := newVetoCity(t)
	stores := city.stores(map[string]vetoBlocker{"ga-1": {"closed", ""}}, nil)
	reader := newHookBlockerReader(city.path, city.cfg, city.legs(), stores.open)

	if _, _, err := reader.GetExactBatch([]string{"ga-1"}); err != nil {
		t.Fatal(err)
	}
	if stores.unbounded {
		t.Fatal("the blocker read ran on an unbounded context; a wedged bd would hang every hook call")
	}
}

// A failed read is not an answer: it must not be remembered as "absent", or one
// transient store error would blind the veto for the rest of the invocation.
func TestHookBlockerReaderRetriesAfterAFailedRead(t *testing.T) {
	city := newVetoCity(t)
	stores := city.stores(map[string]vetoBlocker{"ga-1": {"closed", beadmeta.WorkOutcomeBlocked}}, nil)
	stores.byDir[city.rigDir].showErr = errors.New("dolt is down")
	reader := newHookBlockerReader(city.path, city.cfg, city.legs(), stores.open)

	if _, _, err := reader.GetExactBatch([]string{"ga-1"}); err == nil || !strings.Contains(err.Error(), "dolt is down") {
		t.Fatalf("err = %v, want the store's failure", err)
	}
	stores.byDir[city.rigDir].showErr = nil
	found, _, err := reader.GetExactBatch([]string{"ga-1"})
	if err != nil || len(found) != 1 {
		t.Fatalf("after recovery: found %d err %v, want the blocker read", len(found), err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestWithHookBlockedOutcomeVetoRemovesVetoedRowsFromTheLegsOutput(t *testing.T) {
	bd := &vetoBd{blockers: map[string]vetoBlocker{"ga-b": {"closed", beadmeta.WorkOutcomeBlocked}}}
	raw := vetoRows(
		vetoRow("ga-drop", "open", 2, vetoEdge{"ga-b", "blocks"}),
		vetoRow("ga-keep", "open", 2),
	)
	base := func(_, _ string, _ []string) (string, error) { return raw, nil }

	out, err := withHookBlockedOutcomeVeto(base, bd.store(), io.Discard)("bd ready --json", "/rig", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := vetoSurvivorIDs(t, out); !reflect.DeepEqual(got, []string{"ga-keep"}) {
		t.Fatalf("survivors = %v, want [ga-keep]", got)
	}
}

func TestWithHookBlockedOutcomeVetoLeavesAnUnvetoedOutputByteIdentical(t *testing.T) {
	raw := "  [ {\"id\":\"ga-a\",\"status\":\"open\",\"priority\":1,\"unknown_field\":true} ]\n"
	bd := &vetoBd{}
	base := func(_, _ string, _ []string) (string, error) { return raw, nil }

	out, err := withHookBlockedOutcomeVeto(base, bd.store(), io.Discard)("wq", "/rig", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != raw {
		t.Fatalf("an output with nothing to veto must come back untouched:\n got %q\nwant %q", out, raw)
	}
}

func TestWithHookBlockedOutcomeVetoPropagatesARunnerErrorUntouched(t *testing.T) {
	boom := errors.New("work query timed out")
	bd := &vetoBd{}
	base := func(_, _ string, _ []string) (string, error) { return "partial", boom }

	out, err := withHookBlockedOutcomeVeto(base, bd.store(), io.Discard)("wq", "/rig", nil)
	if !errors.Is(err, boom) || out != "partial" {
		t.Fatalf("got (%q, %v), want (\"partial\", %v)", out, err, boom)
	}
	if len(bd.calls) != 0 {
		t.Fatalf("read blockers %v for a failed work query", bd.calls)
	}
}

func TestWithHookBlockedOutcomeVetoFailsOpenAndReportsOnStderr(t *testing.T) {
	bd := &vetoBd{showErr: errors.New("dolt is down")}
	raw := vetoRows(vetoRow("ga-a", "open", 2, vetoEdge{"ga-b", "blocks"}))
	base := func(_, _ string, _ []string) (string, error) { return raw, nil }
	var stderr bytes.Buffer

	out, err := withHookBlockedOutcomeVeto(base, bd.store(), &stderr)("wq", "/rig", nil)
	if err != nil {
		t.Fatalf("a failed veto read must not fail the work query: %v", err)
	}
	if out != raw {
		t.Fatalf("must serve the unvetoed rows when the veto cannot run:\n got %s\nwant %s", out, raw)
	}
	if msg := stderr.String(); !strings.Contains(msg, "blocked-outcome veto unavailable") || !strings.Contains(msg, "dolt is down") {
		t.Fatalf("stderr %q must name the unavailable veto and its cause", msg)
	}
}

// Cross-store: each leg's rows are vetoed against that leg's own store, BEFORE
// the fan-out ranks the legs. Leg A holds the more urgent row, but its only row
// waits on a handed-off blocker; leg B's less urgent row is the only servable
// work. Selecting leg A would hand a worker a row the controller never counted.
func TestBestStoreWithWorkSelectsTheLegWithServableWork(t *testing.T) {
	legA, legB := hookStore{dir: "/leg-a"}, hookStore{dir: "/leg-b"}
	stores := []hookStore{legA, legB}
	outs := map[string]string{
		"/leg-a": vetoRows(vetoRow("ga-vetoed", "open", 0, vetoEdge{"ga-b", "blocks"})),
		"/leg-b": vetoRows(vetoRow("gb-servable", "open", 3)),
	}
	base := func(_, dir string, _ []string) (string, error) { return outs[dir], nil }
	bd := &vetoBd{blockers: map[string]vetoBlocker{"ga-b": {"closed", beadmeta.WorkOutcomeBlocked}}}

	if _, control, err := bestStoreWithWork("wq", stores, legA, base); err != nil || control.dir != "/leg-a" {
		t.Fatalf("control: without the veto leg A's more urgent row wins; got (%q, %v)", control.dir, err)
	}

	out, selected, err := bestStoreWithWork("wq", stores, legA, withHookBlockedOutcomeVeto(base, bd.store(), io.Discard))
	if err != nil {
		t.Fatalf("bestStoreWithWork: %v", err)
	}
	if selected.dir != "/leg-b" {
		t.Fatalf("selected leg %q, want /leg-b: leg A's only row is vetoed", selected.dir)
	}
	if got := vetoSurvivorIDs(t, out); !reflect.DeepEqual(got, []string{"gb-servable"}) {
		t.Fatalf("selected output rows = %v, want [gb-servable]", got)
	}
}

// The claim door re-validates the selected leg before committing to it, and must
// fall back to a later leg when a veto has emptied it since discovery.
func TestClaimStoreWithFallbackMovesOffALegWhoseRowsAreAllVetoed(t *testing.T) {
	legA, legB := hookStore{dir: "/leg-a"}, hookStore{dir: "/leg-b"}
	outs := map[string]string{
		"/leg-a": vetoRows(vetoRow("ga-vetoed", "open", 0, vetoEdge{"ga-b", "blocks"})),
		"/leg-b": vetoRows(vetoRow("gb-servable", "open", 3)),
	}
	base := func(_, dir string, _ []string) (string, error) { return outs[dir], nil }
	bd := &vetoBd{blockers: map[string]vetoBlocker{"ga-b": {"closed", beadmeta.WorkOutcomeBlocked}}}
	run := withHookBlockedOutcomeVeto(base, bd.store(), io.Discard)

	_, claimStore, err := claimStoreWithFallback("wq", []hookStore{legA, legB}, legA, legA, outs["/leg-a"], run)
	if err != nil {
		t.Fatalf("claimStoreWithFallback: %v", err)
	}
	if claimStore.dir != "/leg-b" {
		t.Fatalf("claim store = %q, want /leg-b: the selected leg's only row is vetoed", claimStore.dir)
	}
}

// The live failure a per-leg fake hid. A rig-scoped agent's fan-out is [rig, own,
// city], and its work_query names the store (`gc bd --rig gascity ready`), so all
// three legs return the rig's rows whatever each leg's own env points at. Reading
// blockers through the leg that produced the rows opened the CITY store for ga-
// ids on the third leg: a multi-second failed read ("no issue found") that reads
// as "no evidence", so the veto silently did nothing there — and the shared batch
// was read once per leg. The veto must read from the store that owns the blockers,
// once.
func TestBestStoreWithWorkVetoesRowsFromAQueryThatIgnoresTheLegsStore(t *testing.T) {
	city := newVetoCity(t)
	legs := city.legs()
	rows := vetoRows(
		vetoRow("ga-vetoed", "open", 0, vetoEdge{"ga-b", "blocks"}),
		vetoRow("ga-servable", "open", 3),
	)
	base := func(_, _ string, _ []string) (string, error) { return rows, nil }
	stores := city.stores(map[string]vetoBlocker{"ga-b": {"closed", beadmeta.WorkOutcomeBlocked}}, nil)
	reader := newHookBlockerReader(city.path, city.cfg, legs, stores.open)

	out, _, err := bestStoreWithWork("wq", legs, legs[0], withHookBlockedOutcomeVeto(base, reader, io.Discard))
	if err != nil {
		t.Fatalf("bestStoreWithWork: %v", err)
	}
	if got := vetoSurvivorIDs(t, out); !reflect.DeepEqual(got, []string{"ga-servable"}) {
		t.Fatalf("selected rows = %v, want [ga-servable]: ga-vetoed's blocker closed handed-off", got)
	}
	if shows := stores.shows(city.rigDir); len(shows) != 1 {
		t.Fatalf("rig store shows = %v, want one batched read shared by all three legs", shows)
	}
	if shows := stores.shows(city.path); len(shows) != 0 {
		t.Fatalf("city store was asked for ga- blockers it does not own: %v", shows)
	}
}

// Every serving entry point must read through the veto. The veto lives in the
// leg runner, so a production call that hands bestStoreWithWork or
// claimHookWorkWithRunner the bare shellWorkQueryWithEnv would silently serve
// rows the controller never counted. Passing the caller's own `run` parameter
// through, as the internal helpers do, is fine.
func TestNoHookServeEntryPointBypassesTheBlockedOutcomeVeto(t *testing.T) {
	runnerArg := map[string]int{"bestStoreWithWork": 3, "claimHookWorkWithRunner": 6}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	fset := token.NewFileSet()
	var entryPoints int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			idx, watched := runnerArg[fn.Name]
			if !watched || idx >= len(call.Args) {
				return true
			}
			if arg, ok := call.Args[idx].(*ast.Ident); ok && arg.Name == "shellWorkQueryWithEnv" {
				t.Errorf("%s calls %s with the bare shellWorkQueryWithEnv; wrap it with hookServeRunner so the blocked-outcome veto applies",
					fset.Position(call.Pos()), fn.Name)
			}
			if arg, ok := call.Args[idx].(*ast.CallExpr); ok {
				if id, ok := arg.Fun.(*ast.Ident); ok && id.Name == "hookServeRunner" {
					entryPoints++
				}
			}
			return true
		})
	}
	if entryPoints != 2 {
		t.Errorf("found %d production serve entry points reading through hookServeRunner, want 2 (discovery in cmdHookWithOptions, claim in claimHookWork)", entryPoints)
	}
}
