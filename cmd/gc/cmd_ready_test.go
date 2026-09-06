package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/spf13/pflag"
)

// This file pins `gc ready` against the four defects the pre-merge branch
// version carried, plus the two contract rules it got wrong. Each test names the
// defect it forbids, because every one of them was invisible in production: the
// generated work_query happens to exercise only the shapes the bugs did not
// reach.

// readyPriority is a pointer to a bead priority, for the seeds whose whole
// point is the priority term of the canonical ready order.
func readyPriority(v int) *int { return &v }

// readyTestLeg wraps a store as a federation leg for the pure-function reader.
func readyTestLeg(label string, store beads.Store) readyLeg {
	return readyLeg{label: label, store: store}
}

// mustCreateReadyBead creates a bead and fails the test on error.
func mustCreateReadyBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.Title, err)
	}
	return created
}

// mustReadyLegs assembles the reader's legs through the production seam, over
// an explicitly stated binding. The leg list is a Plan(RoutedWork) projection
// now, so it can fail — but only on a standing storage refusal, which no
// fixture here stages.
func mustReadyLegs(t *testing.T, cityName string, cityStore beads.Store, rigStores map[string]beads.Store, binding beads.Store) []readyLeg {
	t.Helper()
	legs, err := readyFederationLegsOverBinding(cityName, cityStore, rigStores, binding)
	if err != nil {
		t.Fatalf("readyFederationLegsOverBinding: %v", err)
	}
	return legs
}

// TestReadyReaderEscalatesThePlansPartialLegs pins the ONE place this reader
// overrides the resolver's per-leg policy, and pins that it really is an
// override.
//
// The plan marks a rig leg PartialDegrade — a scope reporting a hole, which the
// API turns into a Partial 200 naming the rig. A CLI work query has no field for
// that: its whole output is the array, and a short array is indistinguishable
// from "no work". So this reader escalates every leg failure to fatal.
//
// Asserting the plan's verdict as well as the reader's behavior is what keeps
// this honest. If the resolver ever made rig legs Fatal, this reader would agree
// with it by accident and the escalation comment would become a lie nobody
// notices.
func TestReadyReaderEscalatesThePlansPartialLegs(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStoreFrom(1000, nil, nil)
	legs := mustReadyLegs(t, "mycity", city, map[string]beads.Store{"alpha": rig}, nil)
	if len(legs) != 2 {
		t.Fatalf("got %d legs, want the city and the rig", len(legs))
	}
	if legs[1].onError != storeref.PolicyPartialDegrade {
		t.Fatalf("the rig leg's plan policy is %v, want PartialDegrade — this reader's escalation is only meaningful if there is something to escalate", legs[1].onError)
	}

	// And the reader fails loud on it anyway.
	failing := []readyLeg{legs[0], {label: legs[1].label, store: listFailStore{Store: beads.NewMemStore()}, onError: legs[1].onError}}
	if _, err := readyBeadsForOpts(failing, readyOpts{status: readyStatusInProgress}); err == nil {
		t.Fatal("a degraded rig leg produced a clean answer; a short array here is indistinguishable from \"no work\"")
	}
	// Control: the same call over healthy legs succeeds, so the error above is
	// the leg failure and not the fixture.
	if _, err := readyBeadsForOpts(legs, readyOpts{status: readyStatusInProgress}); err != nil {
		t.Fatalf("healthy legs errored: %v", err)
	}
}

func readyWireIDs(rows []readyBead) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestReadyBareMetadataFieldMatchesBeadsThatCarryTheKey forbids DEFECT 1, the
// INVERTED metadata filter.
//
// The branch mapped a bare `--metadata-field key` onto an equality test against
// the empty string, so it matched every bead MISSING the key and rejected every
// bead carrying it — the exact opposite of the filter. It never fired in
// production because the generated work_query always passes key=value.
func TestReadyBareMetadataFieldMatchesBeadsThatCarryTheKey(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	carries := mustCreateReadyBead(t, store, beads.Bead{
		Title:    "routed work",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "rig-A/executor"},
	})
	lacks := mustCreateReadyBead(t, store, beads.Bead{Title: "unrouted work", Type: "task"})

	rows, err := readyBeadsForOpts(
		[]readyLeg{readyTestLeg("city", store)},
		readyOpts{metadataFields: []string{"gc.routed_to"}},
	)
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	got := readyWireIDs(rows)
	if len(got) != 1 || got[0] != carries.ID {
		t.Fatalf("--metadata-field gc.routed_to returned %v, want exactly [%s]; %s carries no such key and must not match (inverted metadata filter)", got, carries.ID, lacks.ID)
	}
}

// TestReadyMetadataFieldValueFormsAreDistinct pins the three --metadata-field
// forms apart: bare key (present, non-empty), key=value (exact), and key= (the
// key present and empty). Collapsing any pair is what produced DEFECT 1.
func TestReadyMetadataFieldValueFormsAreDistinct(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	routed := mustCreateReadyBead(t, store, beads.Bead{
		Title:    "routed",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "rig-A/executor"},
	})
	emptyValue := mustCreateReadyBead(t, store, beads.Bead{
		Title:    "key present but empty",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": ""},
	})
	absent := mustCreateReadyBead(t, store, beads.Bead{Title: "no key at all", Type: "task"})

	tests := []struct {
		name  string
		field string
		want  []string
	}{
		{"bare key requires a non-empty value", "gc.routed_to", []string{routed.ID}},
		{"key=value is exact", "gc.routed_to=rig-A/executor", []string{routed.ID}},
		{"key=value rejects a different value", "gc.routed_to=rig-B/executor", nil},
		{"key= requires the key present and empty", "gc.routed_to=", []string{emptyValue.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := readyBeadsForOpts(
				[]readyLeg{readyTestLeg("city", store)},
				readyOpts{metadataFields: []string{tt.field}},
			)
			if err != nil {
				t.Fatalf("gc ready: %v", err)
			}
			got := readyWireIDs(rows)
			if len(got) != len(tt.want) || (len(got) > 0 && !reflect.DeepEqual(got, tt.want)) {
				t.Fatalf("--metadata-field %q returned %v, want %v (bead without the key is %s)", tt.field, got, tt.want, absent.ID)
			}
		})
	}
}

// TestReadyMetadataFieldWithoutAKeyIsRejected keeps a malformed filter from
// silently degrading into "match everything".
func TestReadyMetadataFieldWithoutAKeyIsRejected(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	mustCreateReadyBead(t, store, beads.Bead{Title: "work", Type: "task"})

	_, err := readyBeadsForOpts(
		[]readyLeg{readyTestLeg("city", store)},
		readyOpts{metadataFields: []string{"=value"}},
	)
	if err == nil || !strings.Contains(err.Error(), "has no key") {
		t.Fatalf("readyBeadsForOpts error = %v, want a rejection naming the missing key", err)
	}
}

// TestReadyStatusSelectsThatStatus forbids DEFECT 2, the silently misread
// --status.
//
// The branch special-cased only in_progress and returned READY work for every
// other value, so `--status closed` answered with open beads — an answer that
// looks like data and is about a different question entirely.
func TestReadyStatusSelectsThatStatus(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	open := mustCreateReadyBead(t, store, beads.Bead{Title: "open work", Type: "task"})
	done := mustCreateReadyBead(t, store, beads.Bead{Title: "finished work", Type: "task"})
	if err := store.Close(done.ID); err != nil {
		t.Fatalf("close %s: %v", done.ID, err)
	}
	claimed := mustCreateReadyBead(t, store, beads.Bead{Title: "claimed work", Type: "task"})
	inProgress, assignee := readyStatusInProgress, "worker-1"
	if err := store.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claim %s: %v", claimed.ID, err)
	}

	tests := []struct {
		status string
		want   []string
	}{
		{"closed", []string{done.ID}},
		{readyStatusInProgress, []string{claimed.ID}},
		{"open", []string{open.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			rows, err := readyBeadsForOpts(
				[]readyLeg{readyTestLeg("city", store)},
				readyOpts{status: tt.status},
			)
			if err != nil {
				t.Fatalf("gc ready --status %s: %v", tt.status, err)
			}
			got := readyWireIDs(rows)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("--status %s returned %v, want %v — a status flag that answers with ready work is answering a different question", tt.status, got, tt.want)
			}
		})
	}
}

// TestReadyUnknownStatusIsRejected keeps a typo from being answered with an
// empty array, which is indistinguishable from "there is no such work".
func TestReadyUnknownStatusIsRejected(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	mustCreateReadyBead(t, store, beads.Bead{Title: "work", Type: "task"})

	_, err := readyBeadsForOpts(
		[]readyLeg{readyTestLeg("city", store)},
		readyOpts{status: "in-progress"},
	)
	if err == nil || !strings.Contains(err.Error(), "unknown --status") {
		t.Fatalf("readyBeadsForOpts error = %v, want a rejection naming the unknown status", err)
	}
}

// TestReadyLimitCutsTheMergedTopN forbids DEFECT 3, the limit push-down.
//
// The branch passed q.Limit into EVERY leg and then re-limited the merge, so
// each store truncated in its own read order before the merged order was known.
// The seeded city store returns rows in insertion order, and the highest-value
// bead is inserted last, so a pushed-down limit=2 drops it at the store and no
// amount of re-sorting afterwards can bring it back.
//
// Masked in production only because no caller sets --limit yet; the pool-demand
// work_query this command is a drop-in for sets --limit 20.
func TestReadyLimitCutsTheMergedTopN(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	// Insertion order is created_at order, and it is deliberately NOT the
	// canonical ready order: the low-priority bead is created first.
	low := mustCreateReadyBead(t, store, beads.Bead{Title: "low priority", Type: "task", Priority: readyPriority(2)})
	firstHigh := mustCreateReadyBead(t, store, beads.Bead{Title: "high priority, older", Type: "task", Priority: readyPriority(0)})
	secondHigh := mustCreateReadyBead(t, store, beads.Bead{Title: "high priority, newer", Type: "task", Priority: readyPriority(0)})

	rows, err := readyBeadsForOpts(
		[]readyLeg{readyTestLeg("city", store)},
		readyOpts{limit: 2},
	)
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	got := readyWireIDs(rows)
	want := []string{firstHigh.ID, secondHigh.ID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("--limit 2 returned %v, want %v — %s is lower priority than both and must not survive a bounded read (limit pushed into the leg?)", got, want, low.ID)
	}
}

// TestReadyLimitIsAppliedAfterTheRequestedSort pins the same rule for the
// --sort arm, which is the shape the pool-demand work_query actually builds
// (`--sort oldest --limit 20`).
//
// The leg here answers the way a production work store does — CANONICAL ready
// order, and its own LIMIT applied to that order — so a bound pushed into it
// cuts the priority-ordered prefix and throws away the oldest row before
// --sort oldest ever runs. Nothing downstream can recover it.
func TestReadyLimitIsAppliedAfterTheRequestedSort(t *testing.T) {
	store := readyCanonicalLegStore{Store: splittest.NewWorkStore(t, "gc")}
	oldest := mustCreateReadyBead(t, store, beads.Bead{Title: "oldest but low priority", Type: "task", Priority: readyPriority(3)})
	middle := mustCreateReadyBead(t, store, beads.Bead{Title: "middle", Type: "task", Priority: readyPriority(0)})
	mustCreateReadyBead(t, store, beads.Bead{Title: "newest", Type: "task", Priority: readyPriority(0)})

	rows, err := readyBeadsForOpts(
		[]readyLeg{readyTestLeg("city", store)},
		readyOpts{sortOrder: readySortOldest, limit: 2},
	)
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	got := readyWireIDs(rows)
	want := []string{oldest.ID, middle.ID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("--sort oldest --limit 2 returned %v, want %v — the oldest row is last in the leg's own canonical order, so a bound pushed into the leg drops it", got, want)
	}
}

// readyCanonicalLegStore models the production work leg: its ready read comes
// back in canonical (priority, created_at, id) order and honors the query's own
// LIMIT against that order, which is what a caching-wrapped bd/Dolt or SQLite
// store does. The in-memory kit leaf answers in insertion order instead, which
// would hide a bound pushed into the leg.
type readyCanonicalLegStore struct {
	beads.Store
}

func (s readyCanonicalLegStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	rows, err := s.Store.Ready()
	if err != nil {
		return nil, err
	}
	beads.SortBeadsReadyOrder(rows)
	if len(query) > 0 && query[0].Limit > 0 && len(rows) > query[0].Limit {
		rows = rows[:query[0].Limit]
	}
	return rows, nil
}

// TestReadyEmitsADedicatedWireTypeNotTheDomainBead forbids DEFECT 4, marshaling
// []beads.Bead straight onto an external contract.
//
// beads.Bead is the domain model: it changes for internal reasons, and every one
// of those changes would be published as a silent break in an array other
// programs parse. The reader therefore projects onto a type this package owns.
func TestReadyEmitsADedicatedWireTypeNotTheDomainBead(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	mustCreateReadyBead(t, store, beads.Bead{Title: "work", Type: "task"})

	rows, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, readyOpts{})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	elem := reflect.TypeOf(rows).Elem()
	if pkg := elem.PkgPath(); !strings.HasSuffix(pkg, "/cmd/gc") {
		t.Fatalf("gc ready emits []%s from %q; the external bd-compatible array must be a wire type this package owns, not a domain type whose fields move for internal reasons", elem.Name(), pkg)
	}
}

// TestReadyWireFieldSetIsPinnedToTheHTTPBeadShape states the wire-contract
// decision explicitly so a change to beads.Bead is a decision rather than a
// side effect.
//
// The decision: `gc ready` publishes the SAME field set the HTTP Bead schema
// publishes, under bd's wire tags. Two names are load-bearing and were measured
// to differ across gc's own JSON surfaces — `issue_type` (not `type`) and
// `parent` (not the `parent_id` that `gc bd dep tree --json` emits and that
// bdStoreBridgeBead carries) — because consumers decode this array straight into
// []beads.Bead.
//
// When beads.Bead gains or loses a JSON field this test goes red. That is the
// point: the external array follows only when someone says it should.
//
// # The one deliberate divergence
//
// `blocked_by` is emitted by `gc ready` and is NOT a beads.Bead field, and that
// was decided rather than drifted into. It is a COMPUTED projection, not a
// column: the crash-recovery arm (--status in_progress) resolves each row's
// blocking dependencies through the leg that served the row, because the
// consumer of that arm — the hook's work query — needs the blocker's STATUS to
// decide whether a resumed holder's own bead is currently gated, and
// `dependencies` carries edges without statuses. bd's own `bd ready --json`
// emits exactly this field in exactly this shape, so adding it makes the
// drop-in MORE faithful, not less. It stays out of beads.Bead because it is
// derived per-read, not stored.
func TestReadyWireFieldSetIsPinnedToTheHTTPBeadShape(t *testing.T) {
	// computedReadyWireFields are emitted by gc ready but deliberately absent
	// from beads.Bead. Every entry needs a stated reason above; the set is small
	// on purpose.
	computedReadyWireFields := map[string]bool{"blocked_by": true}

	want := []string{
		"acceptance_criteria", "assignee", "blocked_by", "created_at", "defer_until", "dependencies",
		"description", "ephemeral", "external_ref", "from", "id", "is_blocked", "issue_type",
		"labels", "metadata", "needs", "no_history", "parent", "priority",
		"ref", "status", "title", "updated_at",
	}
	got := jsonFieldNames(reflect.TypeOf(readyBead{}))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readyBead JSON fields = %v, want %v", got, want)
	}
	stored := make([]string, 0, len(got))
	for _, field := range got {
		if !computedReadyWireFields[field] {
			stored = append(stored, field)
		}
	}
	domain := jsonFieldNames(reflect.TypeOf(beads.Bead{}))
	if !reflect.DeepEqual(stored, domain) {
		t.Fatalf("readyBead's stored JSON fields = %v but beads.Bead publishes %v; the external array's field set diverged from the HTTP Bead shape — decide deliberately whether the wire follows, then update this pin", stored, domain)
	}
}

// TestReadyRowMarshalsUnderBdWireTags pins the two names that differ across
// gc's JSON surfaces, at the bytes.
func TestReadyRowMarshalsUnderBdWireTags(t *testing.T) {
	created := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	row := toReadyBead(beads.Bead{
		ID:                 "gcg-1",
		Title:              "graph step",
		Status:             "open",
		Type:               "task",
		ParentID:           "gcg-root",
		CreatedAt:          created,
		AcceptanceCriteria: "exact readback",
		ExternalRef:        "https://github.com/opsime-space/product/issues/42",
	})
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["issue_type"] != "task" {
		t.Fatalf("row = %s, want issue_type=task (bd's tag, not \"type\")", data)
	}
	if decoded["parent"] != "gcg-root" {
		t.Fatalf("row = %s, want parent=gcg-root (bd's tag, not the \"parent_id\" gc bd dep tree emits)", data)
	}
	if decoded["acceptance_criteria"] != "exact readback" {
		t.Fatalf("row = %s, want acceptance_criteria preserved", data)
	}
	if decoded["external_ref"] != "https://github.com/opsime-space/product/issues/42" {
		t.Fatalf("row = %s, want external_ref preserved", data)
	}
	if _, ok := decoded["parent_id"]; ok {
		t.Fatalf("row = %s, must not carry parent_id: consumers decode this array into []beads.Bead, where the tag is \"parent\"", data)
	}
	// Round-tripping into the domain type is the drop-in claim, so assert it.
	var back beads.Bead
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("decode into beads.Bead: %v", err)
	}
	if back.Type != "task" || back.ParentID != "gcg-root" ||
		back.AcceptanceCriteria != "exact readback" ||
		back.ExternalRef != "https://github.com/opsime-space/product/issues/42" {
		t.Fatalf("decoded beads.Bead = %+v, want type, parent, acceptance criteria, and external ref preserved", back)
	}
}

// jsonFieldNames returns the sorted JSON field names a struct marshals, skipping
// fields tagged json:"-".
func jsonFieldNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestReadyEmptyResultIsAnEmptyArray keeps the drop-in claim honest: the hook's
// work-query decode path treats a JSON array as the contract, and `null` is not
// one.
func TestReadyEmptyResultIsAnEmptyArray(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	rows, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, readyOpts{})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("empty result marshaled to %s, want []", data)
	}
}

// TestReadyDedupeIsFirstLegWins pins the dedupe rule against the API's, which is
// what makes CLI == API assertable.
//
// A migrated city holds the same infrastructure row in BOTH the work store and
// the binding: `gc storage migrate` preserves ids and never deletes the source.
// The graph leg runs LAST and the first leg to return an id wins, so both
// surfaces resolve that pair to the work store's row. Diverging here would make
// the CLI and the API disagree about a bead's fields while agreeing about its
// existence, which is worse than either answer alone.
func TestReadyDedupeIsFirstLegWins(t *testing.T) {
	work, graph := splittest.NewSplitStores(t)
	// The co-resident row is what `gc storage migrate` leaves behind: it PRESERVES
	// the id, so the copy in the binding keeps its work-era prefix, and the
	// migration never deletes the source.
	workCopy := mustCreateReadyBead(t, work, beads.Bead{Title: "retained work copy", Type: "task"})
	forced, ok := graph.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("class store %T cannot model the migration's forced foreign-id copy", graph)
	}
	if _, err := forced.CreateWithForeignID(beads.Bead{ID: workCopy.ID, Title: "migrated graph copy", Type: "task"}); err != nil {
		t.Fatalf("copy %s into the class store: %v", workCopy.ID, err)
	}

	rows, err := readyBeadsForOpts(
		mustReadyLegs(t, "mycity", work, nil, graph),
		readyOpts{},
	)
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("federated ready returned %d rows for one co-resident id, want 1: %v", len(rows), readyWireIDs(rows))
	}
	if rows[0].Title != "retained work copy" {
		t.Fatalf("co-resident %s resolved to %q, want the work store's row — the API's first-leg-wins rule puts the graph leg last, and CLI == API depends on matching it", workCopy.ID, rows[0].Title)
	}
}

// TestReadyFederationLegOrderMatchesTheAPIContract pins the leg SEQUENCE:
// city, rigs by name ascending, graph last.
func TestReadyFederationLegOrderMatchesTheAPIContract(t *testing.T) {
	city := splittest.NewWorkStore(t, "gc")
	rigB := splittest.NewWorkStore(t, "rb")
	rigA := splittest.NewWorkStore(t, "ra")
	graph := splittest.NewClassStore(t, config.BeadClassGraph)

	legs := mustReadyLegs(t, "mycity", city, map[string]beads.Store{
		"rig-B":  rigB,
		"rig-A":  rigA,
		"mycity": splittest.NewWorkStore(t, "shadow"),
	}, graph)

	var labels []string
	for _, leg := range legs {
		labels = append(labels, leg.label)
	}
	want := []string{"city", "rig rig-A", "rig rig-B", "graph"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("leg order = %v, want %v — a rig keyed under the city name is the alias State.BeadStores() creates, and the API's rig loop skips it", labels, want)
	}
}

// TestReadySingleStoreCityFederatesOneLeg is the byte-identity claim: a city
// that relocates nothing gets no second leg, so its answer is exactly the one it
// had before the federation existed.
//
// The mutation that proves it is not vacuous: hand the same call a graph store,
// and the answer changes.
func TestReadySingleStoreCityFederatesOneLeg(t *testing.T) {
	city := splittest.NewWorkStore(t, "gc")
	work := mustCreateReadyBead(t, city, beads.Bead{Title: "city work", Type: "task"})
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	step := mustCreateReadyBead(t, graph, beads.Bead{Title: "graph step", Type: "task"})

	legacy, err := readyBeadsForOpts(mustReadyLegs(t, "mycity", city, nil, nil), readyOpts{})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	if got := readyWireIDs(legacy); !reflect.DeepEqual(got, []string{work.ID}) {
		t.Fatalf("single-store ready = %v, want exactly [%s]; a legacy city must not gain a leg", got, work.ID)
	}
	split, err := readyBeadsForOpts(mustReadyLegs(t, "mycity", city, nil, graph), readyOpts{})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	if len(split) != 2 || !containsReadyID(split, step.ID) {
		t.Fatalf("split-city ready = %v, want both %s and the graph step %s; the mutation that should change the answer did not, so the single-store assertion above is vacuous", readyWireIDs(split), work.ID, step.ID)
	}
}

func containsReadyID(rows []readyBead, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestRelocatedGraphLegIsGatedOnStoreIdentity pins the gate that keeps a legacy
// city out of the federation: a binding that resolved back to the work store is
// not a second store, however the routes describe it.
func TestRelocatedGraphLegIsGatedOnStoreIdentity(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	graph := splittest.NewClassStore(t, config.BeadClassGraph)

	if got := relocatedGraphLegFrom(nil, false, work); got != nil {
		t.Fatal("routes that relocate nothing produced a graph leg")
	}
	if got := relocatedGraphLegFrom(work, true, work); got != nil {
		t.Fatal("a binding identical to the work store produced a second leg; that is the same store twice")
	}
	if got := relocatedGraphLegFrom(graph, true, work); got != graph {
		t.Fatalf("relocated binding = %v, want the graph store", got)
	}
}

// TestReadyFailsLoudWhenALegErrors is the anti-fail-open rule. A CLI work query
// has no `partial_errors` field: its whole output is the array, and a short
// array reads as "no work". So a broken leg must be an error, never a degraded
// answer assembled from the legs that happened to answer.
//
// A leg can break in TWO places, and the rule has to hold at both. The read half
// is federateBeadLegs. The OPEN half is earlier and was the hole: the leg list is
// built before a single read runs, so a rig whose store cannot be opened was
// dropped by the builder and never reached the reader at all — exit 0, a
// valid-looking short array, and the failure only on stderr, which no work query
// parses.
func TestReadyFailsLoudWhenALegErrors(t *testing.T) {
	t.Run("a leg that fails on READ", func(t *testing.T) {
		city := splittest.NewWorkStore(t, "gc")
		mustCreateReadyBead(t, city, beads.Bead{Title: "city work", Type: "task"})
		broken := readyFailingStore{err: errors.New("database is locked")}

		_, err := readyBeadsForOpts([]readyLeg{
			readyTestLeg("city", city),
			readyTestLeg("graph", broken),
		}, readyOpts{})
		if err == nil {
			t.Fatal("a dead leg produced a successful answer; a short array is indistinguishable from \"no work\"")
		}
		if !strings.Contains(err.Error(), "graph store") || !strings.Contains(err.Error(), "database is locked") {
			t.Fatalf("error = %v, want it to name the failing leg and the cause", err)
		}
	})

	t.Run("a rig leg that cannot be OPENED", func(t *testing.T) {
		cityDir := newReadyCityWithBrokenRig(t)
		cityStore, err := openStoreAtForCity(cityDir, cityDir)
		if err != nil {
			t.Fatalf("open city store: %v", err)
		}
		mustCreateReadyBead(t, cityStore, beads.Bead{Title: "city work", Type: "task"})

		var stdout, stderr bytes.Buffer
		code := cmdReady(readyOpts{}, &stdout, &stderr)
		if code == 0 {
			t.Fatalf("gc ready exited 0 with the %q rig leg unopened; stdout=%s — a short array is indistinguishable from \"no work\", and stderr is not part of the answer any work query parses", readyBrokenRigName, stdout.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("gc ready emitted %s alongside the failure; a caller that reads stdout on a non-zero exit must not find a plausible answer there", stdout.String())
		}
		if !strings.Contains(stderr.String(), readyBrokenRigName) {
			t.Fatalf("gc ready stderr = %q, want it to name the %q rig whose store could not be opened", stderr.String(), readyBrokenRigName)
		}
		if strings.Contains(stderr.String(), "gc supervisor") {
			t.Fatalf("gc ready stderr = %q, but it reports under another command's name; an operator greps for the command they ran", stderr.String())
		}
	})
}

// TestRigStoreOpenPolicyDiffersByCaller pins the two policies apart over the
// SAME broken city, because they now share one opener and a future edit to that
// opener would otherwise silently move one of them.
//
// The controller degrades: a rig it cannot open is a rig it cannot supervise,
// and stopping a whole city over one unmounted rig is worse than running with
// the rest, so it warns on a stream an operator is already reading. `gc ready`
// fails: its whole output is a JSON array with nowhere to say it is short, and a
// caller cannot tell a dropped leg from an empty one.
func TestRigStoreOpenPolicyDiffersByCaller(t *testing.T) {
	cityDir := newReadyCityWithBrokenRig(t)
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}

	var warnings bytes.Buffer
	stores := buildStandaloneRigStores(cfg, cityDir, &warnings)
	if _, ok := stores["good"]; !ok {
		t.Fatalf("the controller's opener returned %v; it must keep supervising the rigs it COULD open", stores)
	}
	if _, ok := stores[readyBrokenRigName]; ok {
		t.Fatalf("the controller's opener returned a store for the unopenable %q rig", readyBrokenRigName)
	}
	if !strings.Contains(warnings.String(), "gc supervisor: rig bead store \""+readyBrokenRigName+"\"") {
		t.Fatalf("controller warnings = %q, want the unchanged supervisor warning naming %q", warnings.String(), readyBrokenRigName)
	}

	legs, err := readyRigLegStores(cfg, cityDir)
	if err == nil {
		t.Fatalf("gc ready's opener returned %v and no error; a leg it silently dropped is claimable work missing from an array that has nowhere to say so", legs)
	}
	if legs != nil {
		t.Fatalf("gc ready's opener returned stores alongside the failure (%v); a partial leg set is the short answer under another name", legs)
	}
	if want := "rig \"" + readyBrokenRigName + "\" store:"; !strings.Contains(err.Error(), want) {
		t.Fatalf("gc ready's opener error = %v, want it to contain %q", err, want)
	}
}

// TestReadyUnboundRigIsSkippedOnBothSurfaces states the decision for the rig
// declared in city.toml with NO .gc/site.toml binding, which is a different
// shape from a rig that failed to open and is deliberately NOT promoted to an
// error here.
//
// It is skipped, and it is skipped on both surfaces. An unbound rig has no store
// at all, so unlike a dead leg there is no claimable row hiding behind it — the
// fail-open this federation closes is "a store we could not read", not "a rig
// that was never bound". Erroring would also be a fresh CLI-vs-API divergence in
// the slice whose acceptance criterion is CLI == API. If an unbound rig should
// stop a work query, that is a product decision that has to land on both
// surfaces at once, and this test is what makes it fail until it does.
func TestReadyUnboundRigIsSkippedOnBothSurfaces(t *testing.T) {
	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring file store: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "unboundcity", Prefix: "gc"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Rigs:      []config.Rig{{Name: "unbound"}},
	}

	stores, err := readyRigLegStores(cfg, cityDir)
	if err != nil {
		t.Fatalf("gc ready rejected an unbound rig: %v; it is a rig with no store, not a store the city failed to reach", err)
	}
	if _, ok := stores["unbound"]; ok {
		t.Fatalf("gc ready opened a leg for the unbound rig (%v); an empty path resolves to the CITY scope, which would federate the city store twice under a rig's name", stores)
	}

	cs := &controllerState{cfg: cfg, cityName: cfg.Workspace.Name, cityPath: cityDir, cacheCtx: context.Background()}
	if apiStores := cs.buildStores(cfg); len(apiStores) != 0 {
		t.Fatalf("the API built %v for the unbound rig but gc ready built %v; the two surfaces must skip it identically or this slice's CLI == API criterion is false on the arm it did not change", apiStores, stores)
	}
}

// readyBrokenRigName is the rig whose bead store cannot be opened in the
// open-failure fixture.
const readyBrokenRigName = "broken"

// newReadyCityWithBrokenRig writes a real on-disk file-provider city with two
// registered rigs, one of which cannot be opened: its `.gc` is a regular FILE, so
// resolving the scope's beads.json fails with ENOTDIR the way a wrong mount,
// a half-deleted rig or a permissions change does in the field.
//
// The city is left ambient (GC_CITY) so cmdReady resolves it the way the command
// does in production, builder included.
func newReadyCityWithBrokenRig(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	healthyRig := filepath.Join(cityDir, "rigs", "good")
	brokenRig := filepath.Join(cityDir, "rigs", readyBrokenRigName)
	for _, dir := range []string{healthyRig, brokenRig} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating rig dir %s: %v", dir, err)
		}
	}
	cityToml := "[workspace]\nname = \"readybroken\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"good\"\npath = " + strconv.Quote(healthyRig) + "\n\n" +
		"[[rigs]]\nname = " + strconv.Quote(readyBrokenRigName) + "\npath = " + strconv.Quote(brokenRig) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	for _, scope := range []string{cityDir, healthyRig} {
		if err := ensurePersistedScopeLocalFileStore(scope); err != nil {
			t.Fatalf("ensuring file store at %s: %v", scope, err)
		}
	}
	// The break itself: `.gc` as a regular file, so every path under it is
	// ENOTDIR.
	if err := os.WriteFile(filepath.Join(brokenRig, ".gc"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("breaking the %q rig: %v", readyBrokenRigName, err)
	}

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCityFlag, prevRigFlag })
	t.Setenv("GC_CITY", cityDir)
	return cityDir
}

// TestReadyFiltersAreAppliedOverTheMergedSet covers the remaining work_query
// predicates: --unassigned, --assignee, --exclude-type and --exclude-label.
func TestReadyFiltersAreAppliedOverTheMergedSet(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	plain := mustCreateReadyBead(t, store, beads.Bead{Title: "plain", Type: "task"})
	held := mustCreateReadyBead(t, store, beads.Bead{Title: "held", Type: "task", Labels: []string{"hold:mayor"}})
	epic := mustCreateReadyBead(t, store, beads.Bead{Title: "epic", Type: "epic"})
	assigned := mustCreateReadyBead(t, store, beads.Bead{Title: "assigned", Type: "task"})
	owner := "worker-1"
	if err := store.Update(assigned.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
		t.Fatalf("assign %s: %v", assigned.ID, err)
	}

	tests := []struct {
		name string
		opts readyOpts
		want []string
	}{
		{"unassigned drops claimed work", readyOpts{unassigned: true}, []string{plain.ID, held.ID, epic.ID}},
		{"assignee keeps only that identity", readyOpts{assignee: owner}, []string{assigned.ID}},
		{"exclude-type drops epics", readyOpts{excludeTypes: []string{"epic"}}, []string{plain.ID, held.ID, assigned.ID}},
		{"exclude-label drops held work", readyOpts{excludeLabels: []string{"hold:mayor"}}, []string{plain.ID, epic.ID, assigned.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, tt.opts)
			if err != nil {
				t.Fatalf("gc ready: %v", err)
			}
			got := readyWireIDs(rows)
			slices.Sort(got)
			want := append([]string(nil), tt.want...)
			slices.Sort(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: got %v, want %v", tt.name, got, want)
			}
		})
	}
}

// TestReadyUnknownSortIsRejected keeps a mistyped order from silently falling
// back to a different one, which is how a bounded read serves the wrong prefix.
func TestReadyUnknownSortIsRejected(t *testing.T) {
	store := splittest.NewWorkStore(t, "gc")
	mustCreateReadyBead(t, store, beads.Bead{Title: "work", Type: "task"})

	_, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, readyOpts{sortOrder: "priority"})
	if err == nil || !strings.Contains(err.Error(), "unknown --sort") {
		t.Fatalf("readyBeadsForOpts error = %v, want a rejection naming the unknown order", err)
	}
}

// TestReadyDefaultOrderIsTheCanonicalReadyOrder pins that the ordering law is
// TAKEN from internal/beads rather than re-derived here: the merged set must
// come out in exactly the order beads.SortBeadsReadyOrder imposes.
func TestReadyDefaultOrderIsTheCanonicalReadyOrder(t *testing.T) {
	city := splittest.NewWorkStore(t, "gc")
	graph := splittest.NewClassStore(t, config.BeadClassGraph)
	var seeded []beads.Bead
	// A bead with NO priority is seeded on purpose: the canonical order sorts it
	// as 2, matching the SQL readers' COALESCE(priority, 2). A re-derived copy of
	// the law that picked any other default would order it differently, which is
	// the drift this pin exists to catch.
	seeded = append(seeded, mustCreateReadyBead(t, city, beads.Bead{Title: "city, no priority", Type: "task"}))
	seeded = append(seeded, mustCreateReadyBead(t, graph, beads.Bead{Title: "graph urgent", Type: "task", Priority: readyPriority(1)}))
	seeded = append(seeded, mustCreateReadyBead(t, city, beads.Bead{Title: "city backlog", Type: "task", Priority: readyPriority(3)}))

	rows, err := readyBeadsForOpts(mustReadyLegs(t, "mycity", city, nil, graph), readyOpts{})
	if err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	beads.SortBeadsReadyOrder(seeded)
	want := make([]string, 0, len(seeded))
	for _, b := range seeded {
		want = append(want, b.ID)
	}
	if got := readyWireIDs(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("merged order = %v, want %v (beads.SortBeadsReadyOrder over the same rows)", got, want)
	}
}

// readyFailingStore is a leg whose every read fails.
type readyFailingStore struct {
	beads.Store
	err error
}

func (s readyFailingStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, s.err
}

func (s readyFailingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, s.err
}

// TestReadyDeclaresJSONSupport keeps `gc ready --json` from tripping the CLI's
// JSON contract.
//
// The generated work_query this command is a drop-in for carries `--json`
// verbatim, so a rewrite that swaps `bd ready` for `gc ready` must not have to
// strip the flag. The contract is satisfied by DECLARING the payload
// (schemas/ready/result.schema.json) rather than by exempting the command from
// the contract: the array shape other programs parse is now published, not
// undeclared.
func TestReadyDeclaresJSONSupport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)

	handled, code := handleJSONContractRequest(root, []string{"ready", "--json"}, &stdout, &stderr)
	if handled || code != 0 {
		t.Fatalf("gc ready --json was rejected by the JSON contract: handled=%v code=%d stdout=%q stderr=%q", handled, code, stdout.String(), stderr.String())
	}
}

// TestReadyResultSchemaDescribesTheEmittedArray checks the published schema
// against real output, so the declaration cannot drift from the bytes.
func TestReadyResultSchemaDescribesTheEmittedArray(t *testing.T) {
	schema, err := readBuiltinSchema([]string{"ready"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read schemas/ready/result.schema.json: %v", err)
	}
	var declared struct {
		Type    string `json:"type"`
		RawJSON bool   `json:"x-gc-raw-json"`
		Items   struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		} `json:"items"`
	}
	if err := json.Unmarshal(schema, &declared); err != nil {
		t.Fatalf("decode result schema: %v", err)
	}
	if declared.Type != "array" {
		t.Fatalf("result schema type = %q, want array — gc ready emits bd's bare array, not the gc object envelope", declared.Type)
	}
	// The bare array is a documented exception to the ok:true result envelope,
	// taken through the repo's own escape hatch rather than by exempting the
	// command from the JSON contract: x-gc-raw-json plus the explicit path in
	// TestJSONResultSchemasRequireSuccessDiscriminator, so no other raw schema
	// can follow silently.
	if !declared.RawJSON {
		t.Fatal("result schema does not declare x-gc-raw-json; a non-envelope payload must say so where the contract test can see it")
	}
	if declared.Items.AdditionalProperties == nil || *declared.Items.AdditionalProperties {
		t.Fatal("result schema allows additional row properties; the published field set must be closed or it stops being a contract")
	}
	got := make([]string, 0, len(declared.Items.Properties))
	for name := range declared.Items.Properties {
		got = append(got, name)
	}
	slices.Sort(got)
	if want := jsonFieldNames(reflect.TypeOf(readyBead{})); !reflect.DeepEqual(got, want) {
		t.Fatalf("schemas/ready/result.schema.json declares %v, but gc ready emits %v", got, want)
	}
}

// TestCmdReadyOnALegacyCityFederatesCityAndRigStores exercises the command's own
// wiring end to end — city resolution, config load, store opens, leg assembly,
// JSON emit — on a real on-disk city that relocates no coordination class.
//
// That city is the one the byte-identity claim is about: the one-shot storage
// funnel relocates nothing, so no graph leg is resolved and the answer is the
// work stores' own.
//
// It also pins first-leg-wins on real stores, using a property of legacy file
// mode rather than a contrivance: the file provider mints "gc-<n>" per scope
// regardless of a rig's configured prefix, so the city and the rig really do
// alias each other's ids there. internal/api's ready arm names the same
// aliasing. One id is one bead, and the city leg runs first, so the city's row
// is the one served.
func TestCmdReadyOnALegacyCityFederatesCityAndRigStores(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	cityToml := "[workspace]\nname = \"readytest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"frontend\"\npath = " + strconv.Quote(rigDir) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	for _, scope := range []string{cityDir, rigDir} {
		if err := ensurePersistedScopeLocalFileStore(scope); err != nil {
			t.Fatalf("ensuring file store at %s: %v", scope, err)
		}
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() { cityFlag, rigFlag = prevCityFlag, prevRigFlag })
	t.Setenv("GC_CITY", cityDir)

	// The rig leg alone: nothing is in the city store yet, so anything served
	// came from the rig.
	rigBead := mustCreateReadyBead(t, rigStore, beads.Bead{Title: "rig work", Type: "task"})
	rows := runCmdReady(t, readyOpts{})
	if got := readyWireIDs(rows); !reflect.DeepEqual(got, []string{rigBead.ID}) {
		t.Fatalf("gc ready = %v, want the rig bead [%s] — the rig leg is not being federated", got, rigBead.ID)
	}

	// Now the city store, whose first bead aliases the rig's id.
	cityBead := mustCreateReadyBead(t, cityStore, beads.Bead{Title: "city work", Type: "task"})
	if cityBead.ID != rigBead.ID {
		t.Fatalf("city bead %s did not alias the rig bead %s; legacy file mode was expected to mint the same id per scope", cityBead.ID, rigBead.ID)
	}
	second := mustCreateReadyBead(t, cityStore, beads.Bead{Title: "more city work", Type: "task"})

	rows = runCmdReady(t, readyOpts{})
	got := readyWireIDs(rows)
	slices.Sort(got)
	want := []string{cityBead.ID, second.ID}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gc ready = %v, want %v", got, want)
	}
	for _, row := range rows {
		if row.ID == cityBead.ID && row.Title != "city work" {
			t.Fatalf("aliased id %s resolved to %q, want the city leg's row — the city leg runs first and first leg wins", row.ID, row.Title)
		}
	}
}

// runCmdReady runs the command against the ambient city and decodes its array,
// asserting the payload is the bd contract: it must decode BOTH as the wire type
// and as []beads.Bead, which is the whole drop-in claim.
func runCmdReady(t *testing.T, opts readyOpts) []readyBead {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdReady(opts, &stdout, &stderr); code != 0 {
		t.Fatalf("gc ready = %d, stderr:\n%s", code, stderr.String())
	}
	var rows []readyBead
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("gc ready emitted %q, which does not decode as the bd array: %v", stdout.String(), err)
	}
	var domain []beads.Bead
	if err := json.Unmarshal(stdout.Bytes(), &domain); err != nil {
		t.Fatalf("gc ready output does not decode into []beads.Bead: %v", err)
	}
	if len(domain) != len(rows) {
		t.Fatalf("decoded %d beads from %d rows", len(domain), len(rows))
	}
	return rows
}

// TestGcReadySteerDescribesTheFlagsItActuallyAccepts is the pin for the
// remediation half of the frontier refusal, which is the half an operator
// spends.
//
// `gc bd ready` is refused on a split city and the only thing the refusal can
// offer is `gc ready`. It used to offer it as "flag-compatible with `bd ready`",
// which is false: `gc ready` registers ten flags and `bd ready` carries roughly
// twenty-five, so `gc bd ready --label gcg-abc123` — an argv the refusal's own
// test enumerates — is refused and its replacement answers `unknown flag:
// --label`. Steering an operator into a dead end is the same failure as the
// silent empty the guard exists for: a confident answer to a question that was
// not asked.
//
// This derives both sides from code rather than restating them: the accepted
// set from cobra, bd's set from bdflags, and the message is split at its own
// "rejects the rest" marker so a flag can only be advertised on the side it
// actually belongs to.
func TestGcReadySteerDescribesTheFlagsItActuallyAccepts(t *testing.T) {
	msg := beads.RelocatedClassFrontierRefusal("bd ready", []beads.RelocatedClass{
		{Class: "graph", IDPrefix: "gcg", Location: `the "infra" storage binding`},
	}).Error()

	const rejectsMarker = "rejects the rest of bd's ready surface ("
	advertised, rejected, split := strings.Cut(msg, rejectsMarker)
	if !split {
		t.Fatalf("the steer no longer separates what `gc ready` takes from what it rejects; this pin cannot tell one from the other: %q", msg)
	}
	if !strings.Contains(msg, "NOT with all of `bd ready`") {
		t.Errorf("the steer does not disclaim blanket `bd ready` compatibility, which is the false claim it exists to correct: %q", msg)
	}

	registered := map[string]bool{}
	newReadyCmd(io.Discard, io.Discard).Flags().VisitAll(func(f *pflag.Flag) {
		registered["--"+f.Name] = true
	})

	bdReadyFlags := map[string]bool{}
	for flag := range bdflags.ValueFlags("ready") {
		bdReadyFlags[flag] = true
	}
	for flag := range bdflags.BoolFlags("ready") {
		bdReadyFlags[flag] = true
	}
	for flag := range bdflags.GlobalValueFlags() {
		delete(bdReadyFlags, flag)
	}
	for flag := range bdflags.GlobalBoolFlags() {
		delete(bdReadyFlags, flag)
	}
	if len(bdReadyFlags) == 0 {
		t.Fatal("bdflags reports no `ready` flags; this pin is asserting nothing")
	}

	// Long flags are named individually, so each one has to sit on the side it
	// belongs to. A shorthand cannot be matched as a substring (`-u` is inside
	// `--unassigned`), so those are checked as the family the steer names them
	// as, below.
	for flag := range bdReadyFlags {
		if !strings.HasPrefix(flag, "--") {
			continue
		}
		if registered[flag] {
			if !strings.Contains(advertised, flag) {
				t.Errorf("`gc ready` accepts %s but the steer does not name it, so an operator reads a narrower escape than they have", flag)
			}
			continue
		}
		// Not accepted: prove it, and make sure the steer does not advertise it.
		if err := newReadyCmd(io.Discard, io.Discard).ParseFlags([]string{flag}); err == nil {
			t.Errorf("`gc ready %s` parses; the steer names it as rejected", flag)
		}
		if strings.Contains(advertised, flag) {
			t.Errorf("the steer advertises %s as a flag `gc ready` takes, and it does not: %q", flag, advertised)
		}
	}
	// The shorthands are named as a family rather than enumerated; the family
	// claim has to be true of all of them. `bd ready -u -n 1` is the form the
	// tutorials use.
	for flag := range bdReadyFlags {
		if strings.HasPrefix(flag, "--") {
			continue
		}
		if err := newReadyCmd(io.Discard, io.Discard).ParseFlags([]string{flag}); err == nil {
			t.Errorf("`gc ready %s` parses, but the steer says every single-letter shorthand is rejected", flag)
		}
	}
	if !strings.Contains(rejected, "single-letter shorthand") {
		t.Errorf("the steer does not name the shorthand gap: %q", rejected)
	}

	// --sort is shared by NAME and incompatible by VALUE: bd defaults to
	// "priority", which `gc ready` rejects. A steer that named --sort without
	// that caveat would still send an operator to a dead end.
	if _, err := readySortOrder("priority"); err == nil {
		t.Error("`gc ready --sort priority` is accepted; the steer says the orders are oldest|newest")
	}
	if !strings.Contains(msg, "oldest|newest") {
		t.Errorf("the steer names --sort without its accepted values: %q", msg)
	}
}

// readyTierRecordingStore records the storage tier every read it serves was
// asked for, so a test can assert what the federation ASKED rather than only
// what it got back.
//
// The distinction is the whole of ga-8lyxc. Every leg could serve every tier;
// none of them failed, and none of them had a tier to refuse — the federation
// never stated one. The work legs' bead-policy layer then rewrote the zero value
// to TierBoth and the unwrapped class leg took it literally, so the merged answer
// was two different questions and nothing on any path could say so.
type readyTierRecordingStore struct {
	beads.Store
	readyTiers *[]beads.TierMode
	listTiers  *[]beads.TierMode
}

func (s readyTierRecordingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	var q beads.ReadyQuery
	if len(query) > 0 {
		q = query[0]
	}
	*s.readyTiers = append(*s.readyTiers, q.TierMode)
	return s.Store.Ready(query...)
}

func (s readyTierRecordingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	*s.listTiers = append(*s.listTiers, query.TierMode)
	return s.Store.List(query)
}

// TestReadyStatesTheSameTierOnEveryLeg is the structural guard behind ga-8lyxc,
// and it is deliberately about the QUESTION rather than the rows.
//
// A row-level assertion only catches the tier hole on a fixture whose legs are
// wrapped differently, and the wrapping is exactly what a refactor is free to
// change. This asserts the invariant directly: every leg of the federation is
// read at one explicit tier, and never at the zero value — which is not a
// neutral default here but a narrower question the policy-wrapped legs silently
// rewrite. Both arms are covered, because they build different query types.
func TestReadyStatesTheSameTierOnEveryLeg(t *testing.T) {
	var readyTiers, listTiers []beads.TierMode
	legs := []readyLeg{
		readyTestLeg("city", readyTierRecordingStore{
			Store: splittest.NewWorkStore(t, "gc"), readyTiers: &readyTiers, listTiers: &listTiers,
		}),
		readyTestLeg("rig frontend", readyTierRecordingStore{
			Store: splittest.NewWorkStore(t, "ra"), readyTiers: &readyTiers, listTiers: &listTiers,
		}),
		readyTestLeg("graph", readyTierRecordingStore{
			Store: splittest.NewClassStore(t, config.BeadClassGraph), readyTiers: &readyTiers, listTiers: &listTiers,
		}),
	}

	if _, err := readyBeadsForOpts(legs, readyOpts{}); err != nil {
		t.Fatalf("gc ready: %v", err)
	}
	assertEveryLegAskedForTheFederatedTier(t, "gc ready", len(legs), readyTiers)

	readyTiers, listTiers = nil, nil
	if _, err := readyBeadsForOpts(legs, readyOpts{status: readyStatusInProgress}); err != nil {
		t.Fatalf("gc ready --status in_progress: %v", err)
	}
	assertEveryLegAskedForTheFederatedTier(t, "gc ready --status in_progress", len(legs), listTiers)
}

// assertEveryLegAskedForTheFederatedTier asserts one read per leg, each at
// beads.FederatedReadTier, and none at the zero value.
func assertEveryLegAskedForTheFederatedTier(t *testing.T, surface string, legs int, got []beads.TierMode) {
	t.Helper()
	if len(got) != legs {
		t.Fatalf("%s issued %d leg reads over %d legs; the recorder is not seeing the federation", surface, len(got), legs)
	}
	for i, tier := range got {
		if tier == beads.TierIssues {
			t.Errorf("%s read leg %d at the ZERO-VALUE tier. That is not a neutral default across these legs: a policy-wrapped work store rewrites it to TierBoth and an unwrapped relocated class store does not, so the merged answer is two different questions and the class store's whole ephemeral tier drops out with no error (ga-8lyxc)", surface, i)
			continue
		}
		if tier != beads.FederatedReadTier {
			t.Errorf("%s read leg %d at tier %v, want beads.FederatedReadTier (%v); legs that answer at different tiers cannot be merged into one answer", surface, i, tier, beads.FederatedReadTier)
		}
	}
}
