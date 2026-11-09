package main

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/readywork"
)

// This file pins the `gc ready` half of city-qgqc: the read-only
// dispatchability surface. The classification rules themselves are pinned in
// internal/readywork/dispatchable_test.go; what is tested here is the CLI
// projection — that the filter cuts after the read rather than before it, that
// the census reports the same numbers the filter acts on, and that neither arm
// can be pointed at a non-ready status.

// readyDispatchableCorpus is one row per class, in canonical ready order (the
// reader's order, which postProcess preserves).
func readyDispatchableCorpus() []readywork.ReadyBead {
	return []readywork.ReadyBead{
		{ID: "c-carrier-1", Type: "convoy", Title: "input convoy for gk-a"},
		{ID: "c-carrier-2", Type: "convoy", Title: "input convoy for gk-b"},
		{ID: "c-step", Type: "task", Metadata: map[string]string{beadmeta.StepIDMetadataKey: "mol.x.implement"}},
		{ID: "c-hold", Type: "task", Labels: []string{"hold:mayor"}},
		{ID: "c-routed", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gk/gastown.polecat"}},
		{ID: "c-assigned", Type: "task", Assignee: "gk/gastown.polecat"},
		{ID: "c-free-1", Type: "task", Title: "real work, oldest"},
		{ID: "c-free-2", Type: "bug", Title: "real work, newer"},
	}
}

// TestReadyDispatchableKeepsOnlySupply pins the --dispatchable arm: the rows
// that survive are the dispatchable ones, in the reader's order.
func TestReadyDispatchableKeepsOnlySupply(t *testing.T) {
	kept, census := readyFlags{dispatchable: true}.postProcess(readyDispatchableCorpus())
	if census != nil {
		t.Fatalf("--dispatchable produced a census document; it is the row arm")
	}
	got := readyWireIDs(kept)
	want := []string{"c-free-1", "c-free-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("--dispatchable kept %v, want %v", got, want)
	}
}

// TestReadyDispatchableCutsTheLimitAfterTheFilter is the reason the command
// reads unbounded when the surface is on.
//
// The reader applies --limit last, so a limit pushed into the read would cut the
// RAW top-N. Here the raw top-2 are both convoy carriers, so a pushed-down limit
// would answer with zero rows while dispatchable work exists. The limit must
// therefore be the top N of the filtered answer.
func TestReadyDispatchableCutsTheLimitAfterTheFilter(t *testing.T) {
	rows := readyDispatchableCorpus()
	flags := readyFlags{dispatchable: true, query: readywork.Query{Limit: 2}}

	kept, _ := flags.postProcess(rows)
	if got, want := readyWireIDs(kept), []string{"c-free-1", "c-free-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("--dispatchable --limit 2 kept %v, want %v — the limit must be applied to the dispatchable set, not to the raw one", got, want)
	}

	// And the raw top-2 really is nothing but carriers: if this stops being
	// true the fixture no longer distinguishes push-down from cut-after.
	raw := rows
	if len(raw) > 2 {
		raw = raw[:2]
	}
	if summary := readywork.Summarize(raw); summary.Dispatchable != 0 {
		t.Fatalf("the raw top 2 contain %d dispatchable rows; the fixture no longer proves the ordering rule", summary.Dispatchable)
	}

	// A limit larger than the filtered set is not an error and does not pad.
	flags.query.Limit = 10
	kept, _ = flags.postProcess(rows)
	if got := readyWireIDs(kept); !reflect.DeepEqual(got, []string{"c-free-1", "c-free-2"}) {
		t.Fatalf("--limit 10 over 2 dispatchable rows kept %v, want both", got)
	}
}

// TestReadyCensusReportsTheComplementOfTheFilter proves the two arms answer
// about ONE population: the census's dispatchable count equals the number of
// rows --dispatchable would return, and its classes sum to the total it
// reports.
func TestReadyCensusReportsTheComplementOfTheFilter(t *testing.T) {
	rows := readyDispatchableCorpus()
	kept, _ := readyFlags{dispatchable: true}.postProcess(rows)
	_, census := readyFlags{census: true}.postProcess(rows)
	if census == nil {
		t.Fatal("--census produced no census document")
	}
	if census.Dispatchable != len(kept) {
		t.Fatalf("census says dispatchable=%d while --dispatchable returns %d rows; the two arms must measure the same population", census.Dispatchable, len(kept))
	}
	if census.Total != len(rows) {
		t.Fatalf("census total = %d, want %d", census.Total, len(rows))
	}
	sum := 0
	for _, class := range readywork.DispatchClasses() {
		count, ok := census.ByClass[class]
		if !ok {
			t.Fatalf("census by_class is missing %q; every class must be present so a reader cannot confuse absent with zero", class)
		}
		sum += count
	}
	if sum != census.Total {
		t.Fatalf("by_class sums to %d, not the total %d; the breakdown does not complement the count", sum, census.Total)
	}
	// in_flight is step+routed+assigned: the classes where work exists but is
	// taken.
	if census.InFlight != 3 {
		t.Fatalf("census in_flight = %d, want 3 (1 step + 1 routed + 1 assigned)", census.InFlight)
	}
	if census.InFlight > census.Total-census.Dispatchable {
		t.Fatalf("in_flight %d exceeds the non-dispatchable rows %d", census.InFlight, census.Total-census.Dispatchable)
	}
}

// TestReadyCensusMarshalsItsDocumentedShape pins the JSON keys, because this
// document is what a charter read parses. A rename here is a silent break in a
// contract outside this binary.
func TestReadyCensusMarshalsItsDocumentedShape(t *testing.T) {
	_, census := readyFlags{census: true}.postProcess(readyDispatchableCorpus())
	var buf bytes.Buffer
	if err := writeJSON(&buf, census); err != nil {
		t.Fatalf("marshaling the census: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("census is not a JSON object: %v\n%s", err, buf.String())
	}
	for _, key := range []string{"dispatchable", "total", "in_flight", "by_class"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("census JSON is missing %q: %s", key, buf.String())
		}
	}
	// The output is an OBJECT, not the array every other arm emits. This is the
	// whole reason the flag is named --census and not --explain: a bd-shaped work
	// query must never receive this document.
	if strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Fatalf("census emitted a JSON array; a caller expecting rows would silently read an empty queue: %s", buf.String())
	}
}

// TestReadyCensusDocumentDoesNotPretendToBeRows is the bd-collision pin.
//
// `bd ready` already defines --explain (dependency reasoning, a row-ish answer).
// If `gc ready` reused that spelling, a copied bd invocation would receive the
// census OBJECT where it expects a row ARRAY — decoded as zero rows, which is
// indistinguishable from an empty queue. So --census is a new name and --explain
// must stay rejected, even though bd has it.
func TestReadyCensusDocumentDoesNotPretendToBeRows(t *testing.T) {
	cmd := newReadyCmd(io.Discard, io.Discard)
	if cmd.Flags().Lookup("census") == nil {
		t.Fatal("`gc ready` does not register --census; the dispatchability surface has no census arm")
	}
	if cmd.Flags().Lookup("dispatchable") == nil {
		t.Fatal("`gc ready` does not register --dispatchable")
	}
	if err := cmd.ParseFlags([]string{"--explain"}); err == nil {
		t.Error("`gc ready --explain` parses; bd spends that spelling on dependency reasoning and would get a different answer here")
	}
}

// TestReadyCensusRefusesTheFlagsItWouldHaveToIgnore holds the two silent
// failures the census arm is one careless edit away from.
//
// --limit bounds a row list and --census returns no rows; accepting both would
// emit a document that LOOKS limited and is not. --dispatchable and --census are
// two different payloads for one population; accepting both would silently pick
// one. Either is the honored-looking-flag failure ga-8lyxc names.
func TestReadyCensusRefusesTheFlagsItWouldHaveToIgnore(t *testing.T) {
	if err := (readyFlags{census: true, query: readywork.Query{Limit: 5}}).validate(); err == nil {
		t.Error("--census --limit 5 was accepted; the limit would be silently ignored")
	}
	if err := (readyFlags{census: true, dispatchable: true}).validate(); err == nil {
		t.Error("--census --dispatchable was accepted; one answer would be silently dropped")
	}
	if err := (readyFlags{census: true}).validate(); err != nil {
		t.Fatalf("--census alone was rejected: %v", err)
	}
	if err := (readyFlags{dispatchable: true, query: readywork.Query{Limit: 5}}).validate(); err != nil {
		t.Fatalf("--dispatchable --limit 5 was rejected; the limit is meaningful there: %v", err)
	}
}

// TestReadyDispatchabilitySurfaceRejectsNonReadyStatus keeps the surface from
// describing a population it does not measure. Every non-ready status is
// refused, and the ready spelling is accepted.
func TestReadyDispatchabilitySurfaceRejectsNonReadyStatus(t *testing.T) {
	for _, status := range []string{"closed", "blocked", readywork.StatusInProgress} {
		for _, flags := range []readyFlags{
			{dispatchable: true, query: readywork.Query{Status: status}},
			{census: true, query: readywork.Query{Status: status}},
		} {
			err := flags.validate()
			if err == nil {
				t.Fatalf("--status %s was accepted with the dispatchability surface on; closed/blocked/in_progress rows are not ready work", status)
			}
			if !strings.Contains(err.Error(), "--status") {
				t.Fatalf("rejection does not name the offending flag: %v", err)
			}
		}
	}
	for _, status := range []string{"", readywork.StatusOpen} {
		for _, flags := range []readyFlags{
			{dispatchable: true, query: readywork.Query{Status: status}},
			{census: true, query: readywork.Query{Status: status}},
		} {
			if err := flags.validate(); err != nil {
				t.Fatalf("--status %q was rejected with the dispatchability surface on: %v", status, err)
			}
		}
	}
}

// TestReadyWithoutTheSurfaceIsUntouched is the read-only guarantee, asserted at
// the CLI seam: the default arm returns exactly what the reader returned, in
// order, with nothing dropped and no census.
func TestReadyWithoutTheSurfaceIsUntouched(t *testing.T) {
	rows := readyDispatchableCorpus()
	kept, census := readyFlags{}.postProcess(rows)
	if census != nil {
		t.Fatal("the default arm produced a census document")
	}
	if !reflect.DeepEqual(readyWireIDs(kept), readyWireIDs(rows)) {
		t.Fatalf("the default arm dropped rows: got %v, want every row of %v — this surface must not change what `gc ready` returns", readyWireIDs(kept), readyWireIDs(rows))
	}
	// The limit is still the reader's job when the surface is off.
	flags := readyFlags{query: readywork.Query{Limit: 2}}
	kept, _ = flags.postProcess(rows)
	if len(kept) != len(rows) {
		t.Fatalf("postProcess cut to %d rows with the surface off; the limit belongs to the reader there", len(kept))
	}
}
