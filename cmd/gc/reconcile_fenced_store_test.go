package main

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// The fenced capabilities' tests (CONTRACT v5 R3, C0.7).

// blindWriteReadAllowlist is v5 R3's read allowlist: every other beads.Store
// method must refuse.
var blindWriteReadAllowlist = []string{
	"Get", "List", "ListOpen", "Ready", "Children", "ListByLabel", "ListByAssignee", "ListByMetadata",
	"GetLocalString", "Ping", "DepList",
}

// stampedMem is a MemStore stamped with mode, seeded with one open session
// row, whose ID it returns.
func stampedMem(t *testing.T, mode gate.Mode) (*beads.MemStore, string) {
	t.Helper()
	m := beads.NewMemStore()
	if mode != gate.ModeUnset {
		if err := beads.StampOpenedStore(m, "MemStore", mode, nil, nil); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}
	b, err := m.Create(sessionRow("seed", "template", "worker", "session_name", "s-1", "state", "asleep", "generation", "3", txKeyA, "0"))
	if err != nil {
		t.Fatal(err)
	}
	return m, b.ID
}

// Kills the unconditional fallback (S-2): every fencedWriter method resolves
// the writer on its own call, so a store whose writer resolved for the first
// write and resolves none for the next is refused, with nothing written, not
// written blind by the session.Store helpers.
func TestFencedWriterRefusesWhenWriterStopsResolving(t *testing.T) {
	m, id := stampedMem(t, gate.Auto)
	w := fencedWriter{store: m}
	set := func(v string) func(session.Info, session.PersistedResponse) session.MetadataPatch {
		return func(session.Info, session.PersistedResponse) session.MetadataPatch {
			return session.MetadataPatch{txKeyA: v}
		}
	}
	if wrote, err := w.updateMetadataFenced(id, 1, set("1")); err != nil || !wrote {
		t.Fatalf("first write = (%v, %v), want it to land while the writer resolves", wrote, err)
	}
	m.DisableConditionalWrites = true // auto now degrades: no writer resolves
	row, err := sessionFrontDoor(m).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"updateMetadataFenced": func() error { _, err := w.updateMetadataFenced(id, 1, set("2")); return err },
		"updateRowFenced": func() error {
			_, err := w.updateRowFenced(id, 1, func(session.Info) (session.RowPatch, bool) {
				return session.RowPatch{Metadata: session.MetadataPatch{txKeyA: "2"}}, true
			})
			return err
		},
		"closePremise": func() error { _, err := w.closePremise(row, "orphaned", gatherNow); return err },
		"closeWithTerminalPatch": func() error {
			_, err := w.closeWithTerminalPatch(row, session.MetadataPatch{txKeyA: "2"}, "close", gatherNow)
			return err
		},
		"rollbackPendingCreate": func() error {
			_, _, err := w.rollbackPendingCreate(row, session.MetadataPatch{txKeyA: "2"}, nil)
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, errNoConditionalWriter) {
			t.Errorf("%s: %v, want errNoConditionalWriter", name, err)
		}
	}
	if b, _ := m.Get(id); b.Status != "open" || b.Metadata[txKeyA] != "1" {
		t.Fatalf("row status=%s a=%q, want it open and untouched since the first write", b.Status, b.Metadata[txKeyA])
	}
}

// Kills a forgotten mutator (S-3): Tx, Reopen, CloseAll, DepAdd,
// SetLocalString or any beads.Store method added later and not classified
// here. Every method off the allowlist refuses; every allowlisted read
// reaches the inner store.
func TestBlindWriteRefusingStoreRefusesEveryNonReadMethod(t *testing.T) {
	m, id := stampedMem(t, gate.ModeUnset)
	var store beads.Store = blindWriteRefusingStore{inner: m}
	iface := reflect.TypeOf((*beads.Store)(nil)).Elem()
	v := reflect.ValueOf(store)
	errType := reflect.TypeOf((*error)(nil)).Elem()
	for i := range iface.NumMethod() {
		name := iface.Method(i).Name
		method := v.MethodByName(name)
		args := make([]reflect.Value, method.Type().NumIn())
		for j := range args {
			args[j] = reflect.Zero(method.Type().In(j))
		}
		var out []reflect.Value
		if method.Type().IsVariadic() {
			out = method.CallSlice(args)
		} else {
			out = method.Call(args)
		}
		last := out[len(out)-1]
		if last.Type() != errType {
			t.Fatalf("%s returns no error; classify it", name)
		}
		err, _ := last.Interface().(error)
		if allowed := slices.Contains(blindWriteReadAllowlist, name); allowed == errors.Is(err, errBlindWriteRefused) {
			t.Errorf("%s: err %v; allowlisted=%v", name, err, allowed)
		}
	}
	for _, name := range blindWriteReadAllowlist {
		if _, ok := iface.MethodByName(name); !ok {
			t.Errorf("allowlisted %s is not a beads.Store method", name)
		}
	}
	if b, err := store.Get(id); err != nil || b.ID != id {
		t.Fatalf("Get = (%v, %v), want the inner row", b.ID, err)
	}
}

// Kills masking that would make C0.7 refuse every city, or let a refusing
// store resolve a writer its inner store does not: the wrapper resolves the
// conditional writer, its diagnostic and error, the atomic closer and the
// writer handle exactly as the inner store does, in every mode, on MemStore,
// SQLite and a CachingStore.
func TestBlindWriteRefusingStoreKeepsCapabilities(t *testing.T) {
	type inner struct {
		name  string
		store func(t *testing.T) beads.Store
	}
	mem := func(mode gate.Mode, disabled bool) func(t *testing.T) beads.Store {
		return func(t *testing.T) beads.Store {
			m, _ := stampedMem(t, mode)
			m.DisableConditionalWrites = disabled
			return m
		}
	}
	cases := []inner{
		{"unstamped", mem(gate.ModeUnset, false)},
		{"off", mem(gate.Off, false)},
		{"auto", mem(gate.Auto, false)},
		{"auto-degraded", mem(gate.Auto, true)},
		{"require", mem(gate.Require, false)},
		{"require-incapable", mem(gate.Require, true)},
		{"sqlite-require", func(t *testing.T) beads.Store { return stampedSQLite(t, gate.Require) }},
		{"cached-sqlite", func(t *testing.T) beads.Store {
			return beads.NewCachingStoreForTest(stampedSQLite(t, gate.Require), nil)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.store(t)
			out := blindWriteRefusingStore{inner: in}
			wantW, wantDiag, wantErr := beads.ResolveConditionalWriter(in)
			gotW, gotDiag, gotErr := beads.ResolveConditionalWriter(out)
			if gotW != wantW || (gotDiag == nil) != (wantDiag == nil) || (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("resolve = (%v, %v, %v), want the inner store's (%v, %v, %v)", gotW, gotDiag, gotErr, wantW, wantDiag, wantErr)
			}
			_, wantCloser := beads.AtomicConditionalCloserFor(in)
			if _, got := beads.AtomicConditionalCloserFor(out); got != wantCloser {
				t.Fatalf("atomic closer %v, want the inner store's %v", got, wantCloser)
			}
			_, wantHandle := beads.ConditionalWriterForTarget(in)
			if _, got := beads.ConditionalWriterFor(out); got != wantHandle {
				t.Fatalf("writer handle %v, want the inner store's %v", got, wantHandle)
			}
		})
	}
}

// Kills a refusing store that reads its own (missing) stamp, or resolves to
// the bare engine under a mode-sourced wrapper (W-F2a): over the CLI's
// emitting class store, whose mode comes from its stamped engine, the
// resolved writer is the emitting wrapper, exactly as without the refusing
// store, so fenced writes stay fenced and still emit.
func TestBlindWriteRefusingStoreHonorsModeSource(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
		engine, _ := stampedMem(t, mode)
		emitter := &emittingClassStore{Store: engine, cityPath: t.TempDir()}
		want, _, err := beads.ResolveConditionalWriter(emitter)
		if err != nil || want != beads.ConditionalWriter(emitter) {
			t.Fatalf("%s: the emitter resolves (%v, %v), want itself", mode, want, err)
		}
		got, _, err := beads.ResolveConditionalWriter(blindWriteRefusingStore{inner: emitter})
		if err != nil || got != want {
			t.Fatalf("%s: through the refusing store (%v, %v), want the emitter", mode, got, err)
		}
	}
}

// stampedSQLite is a real SQLite store with the revision layout, stamped.
func stampedSQLite(t *testing.T, mode gate.Mode) beads.Store {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	s := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	if err := beads.StampOpenedStore(s, "SQLiteStore", mode, nil, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	return s
}

// Kills a close verb that trusts the writer alone: on a plain MemStore,
// which resolves a conditional writer but no atomic conditional closer, the
// session.Store closes would fall back to a blind metadata write and close.
// Every close verb refuses and the row stays open; the metadata verb still
// writes.
func TestFencedWriterClosesRequireTheAtomicCloser(t *testing.T) {
	m, id := stampedMem(t, gate.Require)
	w := fencedWriter{store: m}
	row, err := sessionFrontDoor(m).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if closed, err := w.closePremise(row, "orphaned", gatherNow); closed || !errors.Is(err, errNoConditionalWriter) {
		t.Errorf("closePremise = (%v, %v), want errNoConditionalWriter", closed, err)
	}
	if closed, err := w.closeWithTerminalPatch(row, session.MetadataPatch{txKeyA: "2"}, "close", gatherNow); closed || !errors.Is(err, errNoConditionalWriter) {
		t.Errorf("closeWithTerminalPatch = (%v, %v), want errNoConditionalWriter", closed, err)
	}
	if closed, post, err := w.rollbackPendingCreate(row, session.MetadataPatch{txKeyA: "2"}, nil); closed || post || !errors.Is(err, errNoConditionalWriter) {
		t.Errorf("rollbackPendingCreate = (%v, %v, %v), want errNoConditionalWriter", closed, post, err)
	}
	if b, _ := m.Get(id); b.Status != "open" || b.Metadata[txKeyA] != "0" {
		t.Fatalf("row status=%s a=%q, want it open and untouched", b.Status, b.Metadata[txKeyA])
	}
	wrote, err := w.updateMetadataFenced(id, 1, func(session.Info, session.PersistedResponse) session.MetadataPatch {
		return session.MetadataPatch{txKeyA: "1"}
	})
	if err != nil || !wrote {
		t.Fatalf("updateMetadataFenced = (%v, %v), want it to land", wrote, err)
	}
}

// callTimeRefusingCloser advertises the atomic conditional closer and a
// conditional writer, whose mode comes from its stamped MemStore, but its
// closer refuses at call time with ErrConditionalWriteUnsupported, as a
// legacy SQLite layout does. session.Store's closes then fall back to a
// blind metadata write and close.
type callTimeRefusingCloser struct{ *beads.MemStore }

func (s callTimeRefusingCloser) ConditionalWritesModeSource() beads.Store { return s.MemStore }

func (callTimeRefusingCloser) CloseWithMetadataIfMatch(string, int64, map[string]string) (beads.Bead, error) {
	return beads.Bead{}, beads.ErrConditionalWriteUnsupported
}

// Kills the refusing layer under fencedWriter: when the writer and the
// closer both resolve but the closer refuses at call time, the session
// front door alone closes the row blind, and fencedWriter's close refuses
// with the row left open.
func TestFencedWriterRefusesACloserThatRefusesAtCallTime(t *testing.T) {
	for _, raw := range []bool{true, false} {
		m, id := stampedMem(t, gate.Require)
		store := callTimeRefusingCloser{m}
		row, err := sessionFrontDoor(store).Get(id)
		if err != nil {
			t.Fatal(err)
		}
		var closed bool
		if raw {
			closed, err = sessionFrontDoor(store).Close(row, "orphaned", gatherNow)
		} else {
			closed, err = fencedWriter{store: store}.closePremise(row, "orphaned", gatherNow)
		}
		b, _ := m.Get(id)
		switch {
		case raw && (err != nil || !closed || b.Status != "closed"):
			t.Fatalf("the raw front door = (%v, %v), status %s; want the blind fallback to close the row (the fixture is wrong)", closed, err, b.Status)
		case !raw && (closed || err == nil || b.Status != "open"):
			t.Fatalf("fencedWriter = (%v, %v), status %s; want a refusal with the row open", closed, err, b.Status)
		}
	}
}
