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

			written, err := front.UpdateRowFenced(created.ID, 3, func(Info, PersistedResponse) (RowPatch, bool) {
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

			written, err := front.UpdateRowFenced(created.ID, 3, func(current Info, _ PersistedResponse) (RowPatch, bool) {
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
				written, err := front.UpdateRowFenced(created.ID, 3, func(Info, PersistedResponse) (RowPatch, bool) {
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

			written, err := front.UpdateRowFenced(created.ID, 3, func(Info, PersistedResponse) (RowPatch, bool) {
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

			written, err := front.UpdateRowFenced(created.ID, 3, func(Info, PersistedResponse) (RowPatch, bool) {
				return decided.patch, decided.ok
			})
			if written || err != nil || len(rec.writes) != 0 {
				t.Fatalf("UpdateRowFenced = (%v, %v) with writes %v, want nothing written", written, err, rec.writes)
			}
			assertRowUntouched(t, store, created.ID)
		})
	}
}

// TestUpdateRowFencedRepairsTypeInTheCAS proves an empty session type is
// repaired in the same UpdateIfMatch as the metadata (CONTRACT v5 M1,
// SESS-701). Kills a dropped Type, a type-only patch read as empty, and a
// separate RepairType write.
func TestUpdateRowFencedRepairsTypeInTheCAS(t *testing.T) {
	for _, backend := range rowFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			b := sessionBeadFixture("s-typeless", "open", map[string]string{"state": string(StateAsleep)})
			b.Labels = append(b.Labels, LabelSession) // what admits a typeless row (IsSessionBeadOrRepairable)
			created, err := store.Create(b)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			empty := ""
			if err := store.Update(created.ID, beads.UpdateOpts{Type: &empty}); err != nil {
				t.Fatalf("clearing the type: %v", err)
			}
			if got, _ := backing.Get(created.ID); got.Type != "" {
				t.Skipf("%s stamps a type on create (%q)", backend.name, got.Type)
			}
			rec := &rowWriteRecorder{Store: store}
			front := NewStore(beads.SessionStore{Store: rec})
			typ := BeadType
			written, err := front.UpdateRowFenced(created.ID, 3, func(Info, PersistedResponse) (RowPatch, bool) {
				return RowPatch{Type: &typ}, true
			})
			if err != nil || !written || !slices.Equal(rec.writes, []string{"UpdateIfMatch"}) {
				t.Fatalf("UpdateRowFenced = (%v, %v), writes %v; want one UpdateIfMatch", written, err, rec.writes)
			}
			if got, err := backing.Get(created.ID); err != nil || got.Type != BeadType {
				t.Fatalf("type %q (%v), want %q", got.Type, err, BeadType)
			}
		})
	}
}

// Copilot capture is an identity command, not an unfenced marker write, and it
// records a transcript identity for usage attribution: never the launch key.
func TestCaptureCopilotTranscriptLifecycle(t *testing.T) {
	for _, backend := range rowFenceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			b := seedRowFenceSession(t, backing, "copilot-seat")
			if err := backing.SetMetadataBatch(b.ID, map[string]string{"provider_kind": "copilot", "session_name": "copilot-seat", "generation": "2", "instance_token": "fixture-token"}); err != nil {
				t.Fatal(err)
			}
			rowMeta := func() map[string]string {
				t.Helper()
				got, err := backing.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got.Metadata
			}
			front := NewStore(beads.SessionStore{Store: store})
			capture := func(key string) (bool, error) {
				return front.CaptureCopilotTranscript(b.ID, "copilot-seat", "2", "fixture-token", key)
			}
			if wrote, err := capture("provider-a"); err != nil || !wrote {
				t.Fatalf("startup = (%v,%v)", wrote, err)
			}
			if meta := rowMeta(); meta[MetadataKeyCopilotTranscript] != "2:provider-a" || meta["session_key"] != "" {
				t.Fatalf("after startup copilot_transcript=%q session_key=%q; want the identity and no launch key", meta[MetadataKeyCopilotTranscript], meta["session_key"])
			}
			if wrote, err := capture("provider-a"); err != nil || wrote {
				t.Fatalf("repeat = (%v,%v)", wrote, err)
			}
			if wrote, err := capture("provider-b"); err == nil || wrote {
				t.Fatalf("conflict = (%v,%v)", wrote, err)
			}
			if got := CopilotTranscriptID(rowMeta()); got != "provider-a" {
				t.Fatalf("identity after conflict = %q, want provider-a", got)
			}
			// A fresh incarnation mints a new generation and token. The identity
			// the earlier one captured is not readable by it, and the earlier
			// incarnation cannot capture again.
			if err := backing.SetMetadataBatch(b.ID, map[string]string{"generation": "3", "instance_token": "fresh-token"}); err != nil {
				t.Fatal(err)
			}
			if got := CopilotTranscriptID(rowMeta()); got != "" {
				t.Fatalf("fresh incarnation reads the earlier identity %q", got)
			}
			if wrote, err := capture("provider-a"); err == nil || wrote {
				t.Fatalf("stale capture = (%v,%v)", wrote, err)
			}
			front = NewStore(beads.SessionStore{Store: backing})
			if wrote, err := front.CaptureCopilotTranscript(b.ID, "copilot-seat", "3", "fresh-token", "provider-b"); err != nil || !wrote {
				t.Fatalf("fresh incarnation = (%v,%v)", wrote, err)
			}
			if meta := rowMeta(); CopilotTranscriptID(meta) != "provider-b" || meta["session_key"] != "" {
				t.Fatalf("after fresh incarnation id=%q session_key=%q", CopilotTranscriptID(meta), meta["session_key"])
			}
		})
	}
}

func TestCaptureCopilotTranscriptRejectsUnprovenIdentity(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"name", "session_name", "another-seat"}, {"provider", "provider_kind", "codex"},
		{"state", "state", "suspended"}, {"closed", "__status", "closed"},
		{"generation", "generation", "3"}, {"zero-generation", "generation", "0"},
		{"token", "instance_token", "other-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := rowFenceBackends()[0].open(t)
			meta := map[string]string{"session_name": "copilot-seat", "provider_kind": "copilot", "state": "active", "generation": "2", "instance_token": "fixture-token"}
			status := "open"
			if tc.key == "__status" {
				status = tc.value
			} else {
				meta[tc.key] = tc.value
			}
			b, err := store.Create(sessionBeadFixture("copilot-seat", status, meta))
			if err != nil {
				t.Fatal(err)
			}
			if status == "closed" {
				if err := store.Close(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			front := NewStore(beads.SessionStore{Store: store})
			wrote, err := front.CaptureCopilotTranscript(b.ID, "copilot-seat", "2", "fixture-token", "provider-a")
			if err == nil || wrote {
				t.Fatalf("capture = (%v,%v)", wrote, err)
			}
			got, _ := store.Get(b.ID)
			if got.Metadata[MetadataKeyCopilotTranscript] != "" {
				t.Fatal("rejected capture wrote an identity")
			}
		})
	}
	for _, generation := range []string{"", "0", "-1", "bad"} {
		store, _ := rowFenceBackends()[0].open(t)
		if wrote, err := NewStore(beads.SessionStore{Store: store}).CaptureCopilotTranscript("absent", "copilot-seat", generation, "fixture-token", "provider-a"); err == nil || wrote {
			t.Fatalf("invalid epoch %q = (%v,%v)", generation, wrote, err)
		}
	}
	// An id that is the session's own, or could leave the transcript directory,
	// names no conversation.
	store, _ := rowFenceBackends()[0].open(t)
	b, err := store.Create(sessionBeadFixture("copilot-seat", "open", map[string]string{"session_name": "copilot-seat", "provider_kind": "copilot", "state": "active", "generation": "2", "instance_token": "fixture-token"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, conversation := range []string{"", b.ID, "a/b", `a\b`, "../x", "a b"} {
		if wrote, err := NewStore(beads.SessionStore{Store: store}).CaptureCopilotTranscript(b.ID, "copilot-seat", "2", "fixture-token", conversation); err == nil || wrote {
			t.Fatalf("conversation %q = (%v,%v)", conversation, wrote, err)
		}
	}
	// Even an ordinary store with conditional writes off must never write blind.
	mem := beads.NewMemStore()
	m, err := mem.Create(sessionBeadFixture("copilot-seat", "open", map[string]string{"session_name": "copilot-seat", "provider_kind": "copilot", "state": "active", "generation": "2", "instance_token": "fixture-token"}))
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := NewStore(beads.SessionStore{Store: mem}).CaptureCopilotTranscript(m.ID, "copilot-seat", "2", "fixture-token", "provider-a")
	if wrote || !beads.IsConditionalWriteUnsupported(err) {
		t.Fatalf("unfenced backend = (%v,%v)", wrote, err)
	}
}

type copilotCaptureInterleaving struct {
	*rowWriteRecorder
	before func()
}

func (s *copilotCaptureInterleaving) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if s.before != nil {
		fn := s.before
		s.before = nil
		fn()
	}
	return s.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

func TestCaptureCopilotTranscriptLostCAS(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "competing-first-capture", true: "fresh-restart"}[restart], func(t *testing.T) {
			raw, _ := rowFenceBackends()[0].open(t)
			b, err := raw.Create(sessionBeadFixture("copilot-seat", "open", map[string]string{"session_name": "copilot-seat", "provider_kind": "copilot", "state": "active", "generation": "2", "instance_token": "fixture-token"}))
			if err != nil {
				t.Fatal(err)
			}
			interleaved := &copilotCaptureInterleaving{rowWriteRecorder: &rowWriteRecorder{Store: raw}}
			interleaved.before = func() {
				if restart {
					if err := raw.SetMetadataBatch(b.ID, map[string]string{"generation": "3", "instance_token": "fresh-token"}); err != nil {
						t.Fatal(err)
					}
					return
				}
				// Another process captures first, by its own conditional write.
				if wrote, err := NewStore(beads.SessionStore{Store: raw}).CaptureCopilotTranscript(b.ID, "copilot-seat", "2", "fixture-token", "winner"); err != nil || !wrote {
					t.Fatalf("competing capture = (%v,%v)", wrote, err)
				}
			}
			wrote, err := NewStore(beads.SessionStore{Store: interleaved}).CaptureCopilotTranscript(b.ID, "copilot-seat", "2", "fixture-token", "loser")
			if wrote || err == nil {
				t.Fatalf("lost CAS = (%v,%v)", wrote, err)
			}
			got, _ := raw.Get(b.ID)
			want := "2:winner"
			if restart {
				want = ""
			}
			if got.Metadata[MetadataKeyCopilotTranscript] != want {
				t.Fatalf("identity after the lost CAS = %q, want %q", got.Metadata[MetadataKeyCopilotTranscript], want)
			}
		})
	}
}

func TestCaptureCopilotTranscriptPermittedStates(t *testing.T) {
	for _, state := range []State{StateActive, StateAwake, StateCreating, StateStartPending} {
		t.Run(string(state), func(t *testing.T) {
			store, _ := rowFenceBackends()[0].open(t)
			b, err := store.Create(sessionBeadFixture("copilot-seat", "open", map[string]string{"session_name": "copilot-seat", "provider_kind": "copilot", "state": string(state), "generation": "2", "instance_token": "fixture-token"}))
			if err != nil {
				t.Fatal(err)
			}
			if wrote, err := NewStore(beads.SessionStore{Store: store}).CaptureCopilotTranscript(b.ID, "copilot-seat", "2", "fixture-token", "provider-a"); err != nil || !wrote {
				t.Fatalf("capture = %v,%v", wrote, err)
			}
		})
	}
}

// Attribution reads an identity only for the generation that captured it.
func TestCopilotTranscriptIDRequiresCurrentGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, transcript, generation, want string
	}{
		{"current", "2:conv-a", "2", "conv-a"},
		{"padded", " 2:conv-a ", " 2 ", "conv-a"},
		{"later generation", "2:conv-a", "3", ""},
		{"no generation", "2:conv-a", "", ""},
		{"no identity", "", "2", ""},
		{"unscoped identity", "conv-a", "2", ""},
		{"zero generation", "0:conv-a", "0", ""},
		{"escaping id", "2:../conv-a", "2", ""},
		{"nested id", "2:a/b", "2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := map[string]string{MetadataKeyCopilotTranscript: tc.transcript, "generation": tc.generation}
			if got := CopilotTranscriptID(meta); got != tc.want {
				t.Fatalf("CopilotTranscriptID = %q, want %q", got, tc.want)
			}
		})
	}
}
