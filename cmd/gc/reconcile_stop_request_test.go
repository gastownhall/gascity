package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// The stop request's tests (CONTRACT v5 D1, I6, I14, I15).

// drainRow is row gc-1: active, generation 3, token tok-3, with meta.
func drainRow(meta ...string) beads.Bead {
	base := []string{"template", "worker", "session_name", "s-gc-1", "state", "active", "generation", "3", "instance_token", "tok-3"}
	return sessionRow("gc-1", append(base, meta...)...)
}

// intentAt is a controller half of reason at incarnation inc, begun a
// minute before gatherNow.
func intentAt(reason, inc string) []string {
	return []string{session.DrainIntentReasonKey, reason, session.DrainIntentAtKey, rowAt(-time.Minute), session.DrainIntentIncarnationKey, inc}
}

// ackAt is a request half at incarnation inc.
func ackAt(inc string) []string {
	return []string{session.DrainAckIncarnationKey, inc, session.DrainAckAtKey, rowAt(-time.Second)}
}

var (
	suspendedRow = []string{"state", "suspended", "sleep_intent", "user-hold", "held_until", rowAt(time.Hour)}
	stopPending  = []string{"state", "draining", "state_reason", session.DrainAckStopPendingReason, "drain_at", rowAt(-time.Second)}
)

func censusRowOf(t *testing.T, b beads.Bead) censusRow {
	t.Helper()
	w, _ := rowWorld(t, b)
	return w.Census.Rows[rowKeyOf(b.ID)]
}

// TestActiveStopTableOverNineRowShapes (I14). Kills a rule dropped or out
// of order: a stale half honored (rule 1 after 4), a suspended row's
// request kept (rule 2), a bare stop-pending row ignored (rule 3), an ack
// read as a plain request or stop-pending as an ack (3 and 4 swapped).
func TestActiveStopTableOverNineRowShapes(t *testing.T) {
	cases := []struct {
		name        string
		meta        [][]string
		ok          bool
		phase       stopPhase
		reason      string
		synthesized bool
	}{
		{name: "no keys"},
		{name: "current intent", meta: [][]string{intentAt("orphaned", "3")}, ok: true, phase: stopRequested, reason: "orphaned"},
		{name: "stale intent", meta: [][]string{intentAt("orphaned", "2")}},
		{name: "current ack", meta: [][]string{ackAt("3")}, ok: true, phase: stopAcked},
		{name: "stale ack", meta: [][]string{ackAt("2")}},
		{name: "intent and ack", meta: [][]string{intentAt("idle", "3"), ackAt("3")}, ok: true, phase: stopAcked, reason: "idle"},
		{name: "suspended row", meta: [][]string{intentAt("orphaned", "3"), ackAt("3"), suspendedRow}},
		{name: "bare stop-pending", meta: [][]string{stopPending, intentAt("orphaned", "2")}, ok: true, phase: stopSignaled, synthesized: true},
		{name: "stop-pending with intent and ack", meta: [][]string{stopPending, intentAt("config-drift", "3"), ackAt("3")}, ok: true, phase: stopSignaled, reason: "config-drift"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := activeStop(censusRowOf(t, drainRow(slices.Concat(tc.meta...)...)))
			if ok != tc.ok || req.Phase != tc.phase || req.Reason != tc.reason || req.Synthesized != tc.synthesized {
				t.Fatalf("activeStop = %+v, %v; want phase %d reason %q synthesized %v, %v", req, ok, tc.phase, tc.reason, tc.synthesized, tc.ok)
			}
			if tc.reason != "" && !req.At.Equal(gatherNow.Add(-time.Minute)) {
				t.Fatalf("At = %v, want the intent's time", req.At)
			}
		})
	}
}

// TestDrainIntentIncarnationIsGeneration. Kills an intent keyed by the
// token (a rekey would void it) or never re-keyed by the generation (a
// PreWake would keep it), and an empty generation pinned undrained: the
// begin writes the generation, a rekey keeps the request, a generation bump
// ends it, and an empty generation is legacy's 0.
func TestDrainIntentIncarnationIsGeneration(t *testing.T) {
	row := censusRowOf(t, drainRow())
	patch := stopBeginPatch(row, "orphaned", gatherNow)
	if patch[session.DrainIntentIncarnationKey] != "3" || patch[session.DrainIntentAtKey] != gatherNow.Format(time.RFC3339) {
		t.Fatalf("begin patch %v, want incarnation 3 (the generation) at gatherNow", patch)
	}
	var meta []string
	for k, v := range patch {
		meta = append(meta, k, v)
	}
	if _, ok := activeStop(censusRowOf(t, drainRow(append(meta, "instance_token", "tok-rekeyed")...))); !ok {
		t.Fatal("a rekey (token only) ended the request")
	}
	if _, ok := activeStop(censusRowOf(t, drainRow(append(meta, "generation", "4")...))); ok {
		t.Fatal("a generation bump kept the request")
	}
	empty := censusRowOf(t, drainRow("generation", ""))
	if inc := stopBeginPatch(empty, "orphaned", gatherNow)[session.DrainIntentIncarnationKey]; inc != "0" {
		t.Fatalf("an empty generation's incarnation = %q, want legacy's 0", inc)
	}
	if _, ok := activeStop(censusRowOf(t, drainRow(append([]string{"generation", ""}, intentAt("orphaned", "0")...)...))); !ok {
		t.Fatal("a request at generation 0 is not read back")
	}
}

// TestStopRequestSurvivesNoOperatorDormantState (I15). Kills a transition
// that writes over an operator's suspend or kill (the suspend-revert
// scenario): such a row has no request, and its residue is voided by a
// write of the stop keys alone. C6a2 adds the begin's half.
func TestStopRequestSurvivesNoOperatorDormantState(t *testing.T) {
	keys := slices.Concat(intentAt("orphaned", "3"), ackAt("3"))
	for _, dormant := range [][]string{suspendedRow, killPending} {
		w, a := rowWorld(t, drainRow(append(slices.Clone(keys), dormant...)...))
		e := a.Snapshot.Entries[rowKeyOf("gc-1")]
		e.Desired, e.DrainReason = desireDrain, drainOrphaned
		it, _ := decideRow(w, a, rowKeyOf("gc-1"))
		if dormant[1] != "suspended" { // the kill fence (A2) owns a killed row
			if it.Kind != "" {
				t.Fatalf("killed row: %+v, want A2's hold", it)
			}
			continue
		}
		if it.Kind != intentDrainVoid || it.Reason != decideStopResidue {
			t.Fatalf("suspended row: %+v, want the residue void", it)
		}
		for k := range it.Patch {
			if !slices.Contains(keys, k) {
				t.Fatalf("the void writes %q, an operator-owned key", k)
			}
		}
	}
}

// stopKeyFields are rawStopKeys' field names.
func stopKeyFields() []string {
	typ := reflect.TypeOf(rawStopKeys{})
	names := make([]string, typ.NumField())
	for i := range names {
		names[i] = typ.Field(i).Name
	}
	return names
}

// stopKeyNames are what TestStopRequestLintBansDirectKeyReads bans in
// effect and step files: the key constants, the census field and its
// projection, and rawStopKeys' fields.
func stopKeyNames() []string {
	return append([]string{
		"DrainIntentReasonKey", "DrainIntentAtKey", "DrainIntentIncarnationKey", "DrainAckIncarnationKey",
		"DrainAckAtKey", "StopKeys", "readStopKeys",
	}, stopKeyFields()...)
}

// lintStopKeys returns one "file:line: name" per stop-request key named or
// spelled in src.
func lintStopKeys(t *testing.T, path string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := stopKeyNames()
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		var hit string
		switch n := n.(type) {
		case *ast.Ident:
			if slices.Contains(names, n.Name) {
				hit = n.Name
			}
		case *ast.BasicLit:
			if v, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING && slices.Contains(stopKeys, v) {
				hit = v
			}
		}
		if hit != "" {
			out = append(out, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), hit))
		}
		return true
	})
	return out
}

// TestStopRequestLintBansDirectKeyReads (I6, I14). Kills a second reader
// of the stop request: a seeded effect naming each key, field and spelling
// trips the lint; no effect or step file trips it; and no production file
// but the accessor's and E3's CLI names a key, a
// spelling or a rawStopKeys field.
func TestStopRequestLintBansDirectKeyReads(t *testing.T) {
	var src strings.Builder
	src.WriteString("package main\n\nfunc seeded(row censusRow, meta map[string]string) {\n")
	for _, n := range stopKeyNames() {
		fmt.Fprintf(&src, "\t_ = row.%s\n", n)
	}
	for _, v := range stopKeys {
		fmt.Fprintf(&src, "\t_ = meta[%q]\n", v)
	}
	src.WriteString("}\n")
	if got, want := len(lintStopKeys(t, "reconcile_effect_seeded.go", src.String())), len(stopKeyNames())+len(stopKeys); got != want {
		t.Fatalf("the seeded effect: %d findings, want %d", got, want)
	}
	for _, f := range effectLintFiles(t) {
		for _, finding := range lintStopKeys(t, f, nil) {
			t.Errorf("an effect or step reads the stop request directly: %s", finding)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	owners := []string{"reconcile_stop_request.go", "cmd_runtime_drain.go"}
	plumbing := []string{"StopKeys", "readStopKeys"} // the census's projection, outside effects
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || slices.Contains(owners, f) {
			continue
		}
		for _, finding := range lintStopKeys(t, f, nil) {
			if name := finding[strings.LastIndex(finding, " ")+1:]; !slices.Contains(plumbing, name) {
				t.Errorf("a second reader of the stop request: %s", finding)
			}
		}
	}
}

// TestResidueVoidSurvivesSameGenerationResume (ORCH E3: Manager.ensureRunning
// resumes at the same generation and token). Kills a residue void that
// leaves either ack key: after the void lands on a suspended row, a resume
// at the same generation reads no request.
func TestResidueVoidSurvivesSameGenerationResume(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow(slices.Concat(intentAt(drainOrphaned, "3"), ackAt("3"), suspendedRow)...))
	if err != nil {
		t.Fatal(err)
	}
	k := rowKey{Leg: rowLeg, ID: b.ID}
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), LegStores: map[string]beads.Store{rowLeg: store}}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: {Key: k, Liveness: livenessAlive}}}}
	it, _ := decideRow(w, a, k)
	if s := runTx(context.Background(), newEffectPass(w, a), it, effectSpecs[it.Kind], nil); it.Reason != decideStopResidue || s.Outcome != settledLanded {
		t.Fatalf("void %+v settled %+v, want the residue void landed", it, s)
	}
	got, _ := store.Get(b.ID)
	for _, key := range stopKeys {
		if got.Metadata[key] != "" {
			t.Errorf("%s = %q after the void", key, got.Metadata[key])
		}
	}
	if err := store.SetMetadataBatch(b.ID, map[string]string{"state": "active", "sleep_intent": "", "held_until": ""}); err != nil {
		t.Fatal(err)
	}
	if req, ok := activeStop(readCensus(t, gatherNow, censusLegs(rowLeg, store)).Rows[k]); ok || req.Residue {
		t.Fatalf("after a same-generation resume: %+v, %v; want no request", req, ok)
	}
}

// TestRereadKeepsStopKeysWithoutMetadata. Kills a re-decide that drops a
// request on a path with no persisted metadata: the re-read keeps the
// census's stop keys then, and takes the fresh ones otherwise.
func TestRereadKeepsStopKeysWithoutMetadata(t *testing.T) {
	w, _ := rowWorld(t, drainRow(intentAt(drainOrphaned, "3")...))
	k := rowKeyOf("gc-1")
	info := w.Census.Rows[k].Info
	if _, ok := activeStop(w.Census.reread(k, info, nil)); !ok {
		t.Fatal("a nil-metadata re-read dropped the request")
	}
	if _, ok := activeStop(w.Census.reread(k, info, map[string]string{})); ok {
		t.Fatal("a re-read kept keys its fresh metadata no longer holds")
	}
}
