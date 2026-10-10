package session

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// rowWriteRecorder records every write that reaches the store underneath a
// session front door. It is a mode-sourced wrapper, so the resolved
// conditional writer is the recorder itself and its UpdateIfMatch calls are
// seen too.
type rowWriteRecorder struct {
	beads.Store
	writes  []string
	ifMatch []beads.UpdateOpts
}

func (r *rowWriteRecorder) ConditionalWritesModeSource() beads.Store { return r.Store }

func (r *rowWriteRecorder) cw() beads.ConditionalWriter {
	w, _ := beads.ConditionalWriterFor(r.Store)
	return w
}

func (r *rowWriteRecorder) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	r.writes = append(r.writes, "UpdateIfMatch")
	r.ifMatch = append(r.ifMatch, opts)
	return r.cw().UpdateIfMatch(id, rev, opts)
}

func (r *rowWriteRecorder) CloseIfMatch(id string, rev int64) error {
	r.writes = append(r.writes, "CloseIfMatch")
	return r.cw().CloseIfMatch(id, rev)
}

func (r *rowWriteRecorder) DeleteIfMatch(id string, rev int64) error {
	r.writes = append(r.writes, "DeleteIfMatch")
	return r.cw().DeleteIfMatch(id, rev)
}

func (r *rowWriteRecorder) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	r.writes = append(r.writes, "CompareAndSetMetadataKey")
	return r.cw().CompareAndSetMetadataKey(id, key, expected, next)
}

func (r *rowWriteRecorder) Update(id string, opts beads.UpdateOpts) error {
	r.writes = append(r.writes, "Update")
	return r.Store.Update(id, opts)
}

func (r *rowWriteRecorder) SetMetadata(id, key, value string) error {
	r.writes = append(r.writes, "SetMetadata")
	return r.Store.SetMetadata(id, key, value)
}

func (r *rowWriteRecorder) SetMetadataBatch(id string, kvs map[string]string) error {
	r.writes = append(r.writes, "SetMetadataBatch")
	return r.Store.SetMetadataBatch(id, kvs)
}

func (r *rowWriteRecorder) Tx(msg string, fn func(beads.Tx) error) error {
	r.writes = append(r.writes, "Tx")
	return r.Store.Tx(msg, fn)
}

// rowFenceBackends are the fenced backends whose UpdateIfMatch guards labels
// (LL1): every fenced patchFenceBackend except the FileStore ones.
func rowFenceBackends() []patchFenceBackend {
	var out []patchFenceBackend
	for _, b := range patchFenceBackends() {
		if b.fenced && !strings.Contains(b.name, "FileStore") {
			out = append(out, b)
		}
	}
	return out
}

// singletonRowPatch is the shape of an M1 canonical-singleton collapse:
// identity metadata, the title, and a phantom slot label swapped out.
func singletonRowPatch() RowPatch {
	title := "mayor"
	return RowPatch{
		Metadata:     MetadataPatch{"alias": "mayor", "pool_slot": ""},
		Title:        &title,
		AddLabels:    []string{"agent:mayor"},
		RemoveLabels: []string{"agent:mayor-2"},
	}
}

func seedRowFenceSession(t *testing.T, store beads.Store, name string) beads.Bead {
	t.Helper()
	b := sessionBeadFixture(name, "open", map[string]string{
		"state":     string(StateActive),
		"pool_slot": "2",
		"__title":   "mayor-2",
	})
	b.Labels = append(b.Labels, "agent:mayor-2")
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func assertRowUntouched(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "mayor-2" || got.Metadata["pool_slot"] != "2" || got.Metadata["alias"] != "" ||
		!slices.Contains(got.Labels, "agent:mayor-2") || slices.Contains(got.Labels, "agent:mayor") {
		t.Fatalf("row written: title=%q meta=%v labels=%v", got.Title, got.Metadata, got.Labels)
	}
}

// TestUpdateRowFencedOneCAS proves metadata, title and labels go out in one
// UpdateIfMatch and land together. Kills a split write (metadata by CAS, then
// labels or title by a second write).
func TestUpdateRowFencedOneCAS(t *testing.T) {
	for _, backend := range rowFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedRowFenceSession(t, store, "s-row")
			rec := &rowWriteRecorder{Store: store}
			front := NewStore(beads.SessionStore{Store: rec})

			written, err := front.UpdateRowFenced(created.ID, 3, func(Info) (RowPatch, bool) {
				return singletonRowPatch(), true
			})
			if err != nil || !written {
				t.Fatalf("UpdateRowFenced = (%v, %v), want (true, nil)", written, err)
			}
			if !slices.Equal(rec.writes, []string{"UpdateIfMatch"}) {
				t.Fatalf("writes = %v, want one UpdateIfMatch", rec.writes)
			}
			opts := rec.ifMatch[0]
			if opts.Title == nil || *opts.Title != "mayor" || opts.Metadata["alias"] != "mayor" ||
				!slices.Equal(opts.Labels, []string{"agent:mayor"}) || !slices.Equal(opts.RemoveLabels, []string{"agent:mayor-2"}) {
				t.Fatalf("UpdateIfMatch opts = %+v, want the whole patch", opts)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Title != "mayor" || got.Metadata["alias"] != "mayor" || got.Metadata["pool_slot"] != "" ||
				!slices.Contains(got.Labels, "agent:mayor") || slices.Contains(got.Labels, "agent:mayor-2") {
				t.Fatalf("row after write: title=%q meta=%v labels=%v", got.Title, got.Metadata, got.Labels)
			}
		})
	}
}

// TestUpdateRowFencedRedecidesOnLostCAS proves a writer landing between the
// read and the CAS makes the write re-read and re-decide on the fresh row.
// Kills retrying at the stale revision (every attempt loses), deciding once
// and replaying the stale patch, and returning on the first lost CAS.
func TestUpdateRowFencedRedecidesOnLostCAS(t *testing.T) {
	for _, backend := range rowFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedRowFenceSession(t, store, "s-redecide")
			front := NewStore(beads.SessionStore{Store: store})
			var seen []string

			written, err := front.UpdateRowFenced(created.ID, 3, func(current Info) (RowPatch, bool) {
				seen = append(seen, current.Alias)
				if len(seen) == 1 {
					if err := backing.SetMetadataBatch(created.ID, map[string]string{"alias": "operator-pick"}); err != nil {
						t.Fatalf("concurrent write: %v", err)
					}
				}
				patch := singletonRowPatch()
				if current.Alias != "" {
					delete(patch.Metadata, "alias")
				}
				return patch, true
			})
			if err != nil || !written {
				t.Fatalf("UpdateRowFenced = (%v, %v), want (true, nil)", written, err)
			}
			if !slices.Equal(seen, []string{"", "operator-pick"}) {
				t.Fatalf("decide saw aliases %q, want a second decision on the fresh row", seen)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Metadata["alias"] != "operator-pick" || got.Title != "mayor" || !slices.Contains(got.Labels, "agent:mayor") {
				t.Fatalf("row after redecide: title=%q meta=%v labels=%v", got.Title, got.Metadata, got.Labels)
			}
		})
	}
}

// TestUpdateRowFencedRefusesWithoutWriter proves every call refuses, and
// writes nothing, when no conditional writer resolves: conditional writes off,
// auto degraded on an incapable store, and require on an incapable store.
// Kills UpdateMetadataFenced's blind fallback being copied, and a resolution
// cached from an earlier capable call.
func TestUpdateRowFencedRefusesWithoutWriter(t *testing.T) {
	stamped := func(mode gate.Mode) func(t *testing.T) beads.Store {
		return func(t *testing.T) beads.Store {
			mem := beads.NewMemStore()
			store := stampedMemStore(t, mode, mem)
			mem.DisableConditionalWrites = true
			return store
		}
	}
	noWriter := func(err error) bool { return errors.Is(err, beads.ErrConditionalWriteUnsupported) }
	cases := []struct {
		name string
		open func(t *testing.T) beads.Store
		want func(error) bool
	}{
		{"off", func(*testing.T) beads.Store { return beads.NewMemStore() }, noWriter},
		{"auto-degraded", stamped(gate.Auto), noWriter},
		{"require-incapable", stamped(gate.Require), beads.IsConditionalWritesRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			created := seedRowFenceSession(t, store, "s-blind")
			front := NewStore(beads.SessionStore{Store: store})
			for call := 0; call < 2; call++ {
				written, err := front.UpdateRowFenced(created.ID, 3, func(Info) (RowPatch, bool) {
					t.Fatal("decide ran without a conditional writer")
					return RowPatch{}, false
				})
				if written || !tc.want(err) {
					t.Fatalf("call %d: UpdateRowFenced = (%v, %v), want the no-writer refusal", call, written, err)
				}
			}
			assertRowUntouched(t, store, created.ID)
		})
	}
}

// TestUpdateRowFencedLabelsRefusedWritesNothing proves a store that cannot
// guard labels returns its refusal and writes no half of the patch. Kills a
// fallback that drops the labels and writes the rest, or writes the labels
// apart.
func TestUpdateRowFencedLabelsRefusedWritesNothing(t *testing.T) {
	for _, backend := range patchFenceBackends() {
		if !strings.Contains(backend.name, "FileStore") {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created := seedRowFenceSession(t, store, "s-labels")
			front := NewStore(beads.SessionStore{Store: store})

			written, err := front.UpdateRowFenced(created.ID, 3, func(Info) (RowPatch, bool) {
				return singletonRowPatch(), true
			})
			var unsupported *beads.ConditionalUpdateFieldUnsupportedError
			if written || !errors.As(err, &unsupported) {
				t.Fatalf("UpdateRowFenced = (%v, %v), want the labels refusal", written, err)
			}
			assertRowUntouched(t, backing, created.ID)
		})
	}
}

// TestUpdateRowFencedDecideRefusalWritesNothing proves decide's refusal, and
// an empty patch, write nothing. Kills ignoring decide's bool and sending an
// empty UpdateIfMatch.
func TestUpdateRowFencedDecideRefusalWritesNothing(t *testing.T) {
	for _, decided := range []struct {
		name  string
		patch RowPatch
		ok    bool
	}{{"refused", singletonRowPatch(), false}, {"empty", RowPatch{}, true}} {
		t.Run(decided.name, func(t *testing.T) {
			store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
			created := seedRowFenceSession(t, store, "s-noop")
			rec := &rowWriteRecorder{Store: store}
			front := NewStore(beads.SessionStore{Store: rec})

			written, err := front.UpdateRowFenced(created.ID, 3, func(Info) (RowPatch, bool) {
				return decided.patch, decided.ok
			})
			if written || err != nil || len(rec.writes) != 0 {
				t.Fatalf("UpdateRowFenced = (%v, %v) with writes %v, want nothing written", written, err, rec.writes)
			}
			assertRowUntouched(t, store, created.ID)
		})
	}
}
