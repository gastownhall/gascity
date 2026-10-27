package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	restartFenceToken      = "tok-current"
	restartFenceOtherToken = "tok-successor"
)

// seedRestartFenceSession creates a live session row of incarnation
// (generation 3, restartFenceToken) plus extra metadata.
func seedRestartFenceSession(t *testing.T, store beads.Store, extra map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{
		"state":          string(StateActive),
		"generation":     "3",
		"instance_token": restartFenceToken,
	}
	for k, v := range extra {
		meta[k] = v
	}
	created, err := store.Create(beads.Bead{Title: "session", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func assertRestartRequestKeys(t *testing.T, store beads.Store, id, want string) beads.Bead {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["restart_requested"] != want || got.Metadata["continuation_reset_pending"] != want {
		t.Fatalf("row keys = restart_requested %q, continuation_reset_pending %q; want %q for both",
			got.Metadata["restart_requested"], got.Metadata["continuation_reset_pending"], want)
	}
	return got
}

// TestDecideRestartRequest pins the fence's decision on one read.
func TestDecideRestartRequest(t *testing.T) {
	both := MetadataPatch{"restart_requested": "true", "continuation_reset_pending": "true"}
	cases := []struct {
		name      string
		info      Info
		wantPatch MetadataPatch
		wantErr   error
	}{
		{"current incarnation", Info{InstanceToken: restartFenceToken}, both, nil},
		{"token padded on the row", Info{InstanceToken: " " + restartFenceToken + "\n"}, both, nil},
		{"another incarnation", Info{InstanceToken: restartFenceOtherToken}, nil, ErrStaleRestartRequest},
		{"row with no token", Info{}, nil, ErrStaleRestartRequest},
		{"closed row", Info{Closed: true, InstanceToken: restartFenceToken}, nil, ErrSessionClosed},
		{"both keys already set", Info{InstanceToken: restartFenceToken, RestartRequested: "true", ContinuationResetPending: "true"}, nil, nil},
		{"only the marker set", Info{InstanceToken: restartFenceToken, RestartRequested: "true"}, both, nil},
		{"only the reset set", Info{InstanceToken: restartFenceToken, ContinuationResetPending: "true"}, both, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := decideRestartRequest(tc.info, restartFenceToken)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(patch) != len(tc.wantPatch) {
				t.Fatalf("patch = %v, want %v", patch, tc.wantPatch)
			}
			for k, v := range tc.wantPatch {
				if patch[k] != v {
					t.Fatalf("patch = %v, want %v", patch, tc.wantPatch)
				}
			}
		})
	}
}

// TestRequestFreshRestartFencedRecords proves the current incarnation's
// request lands as one CAS carrying both keys.
func TestRequestFreshRestartFencedRecords(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedRestartFenceSession(t, store, nil)
	rec := &rowWriteRecorder{Store: store}
	m := newTestManager(t, rec, runtime.NewFake())

	if err := m.RequestFreshRestartFenced(created.ID, restartFenceToken); err != nil {
		t.Fatalf("RequestFreshRestartFenced: %v", err)
	}
	got := assertRestartRequestKeys(t, store, created.ID, "true")
	if len(rec.writes) != 1 || rec.writes[0] != "UpdateIfMatch" {
		t.Fatalf("writes = %v, want one UpdateIfMatch", rec.writes)
	}
	if got.Revision != created.Revision+1 {
		t.Fatalf("revision = %d, want %d (one write)", got.Revision, created.Revision+1)
	}
}

// casHookWriter runs before on every UpdateIfMatch, then delegates. A hook
// that writes the backing row makes the CAS lose.
type casHookWriter struct {
	beads.Store
	before func(id string)
	calls  int
}

func (w *casHookWriter) ConditionalWritesModeSource() beads.Store { return w.Store }

func (w *casHookWriter) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	w.calls++
	w.before(id)
	return w.cw().UpdateIfMatch(id, rev, opts)
}

func (w *casHookWriter) cw() beads.ConditionalWriter {
	cw, _ := beads.ConditionalWriterFor(w.Store)
	return cw
}

func (w *casHookWriter) CloseIfMatch(id string, rev int64) error { return w.cw().CloseIfMatch(id, rev) }

func (w *casHookWriter) DeleteIfMatch(id string, rev int64) error {
	return w.cw().DeleteIfMatch(id, rev)
}

func (w *casHookWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return w.cw().CompareAndSetMetadataKey(id, key, expected, next)
}

// TestRequestFreshRestartFencedLosesToAMint proves a start that mints a new
// incarnation between the deciding read and the CAS refuses the request:
// the re-decide sees the successor's token.
func TestRequestFreshRestartFencedLosesToAMint(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedRestartFenceSession(t, store, nil)
	w := &casHookWriter{Store: store}
	w.before = func(id string) {
		if w.calls == 1 {
			if err := store.SetMetadataBatch(id, map[string]string{"generation": "4", "instance_token": restartFenceOtherToken}); err != nil {
				t.Fatalf("mint: %v", err)
			}
		}
	}
	m := newTestManager(t, w, runtime.NewFake())

	err := m.RequestFreshRestartFenced(created.ID, restartFenceToken)
	if !errors.Is(err, ErrStaleRestartRequest) {
		t.Fatalf("err = %v, want ErrStaleRestartRequest", err)
	}
	assertRestartRequestKeys(t, store, created.ID, "")
}

// TestRequestFreshRestartFencedContended proves a row that moves under every
// attempt is not written and reports ErrRestartRequestContended.
func TestRequestFreshRestartFencedContended(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedRestartFenceSession(t, store, nil)
	w := &casHookWriter{Store: store}
	w.before = func(id string) {
		if err := store.SetMetadata(id, "last_woke_at", "moved"); err != nil {
			t.Fatalf("move: %v", err)
		}
	}
	m := newTestManager(t, w, runtime.NewFake())

	err := m.RequestFreshRestartFenced(created.ID, restartFenceToken)
	if !errors.Is(err, ErrRestartRequestContended) {
		t.Fatalf("err = %v, want ErrRestartRequestContended", err)
	}
	if w.calls != restartRequestAttempts {
		t.Fatalf("CAS attempts = %d, want %d", w.calls, restartRequestAttempts)
	}
	assertRestartRequestKeys(t, store, created.ID, "")
}

// TestRequestFreshRestartFencedWithoutAConditionalWriter proves a legacy
// store with no conditional writer still decides on the token: a stale
// caller is refused and the current one is recorded.
func TestRequestFreshRestartFencedWithoutAConditionalWriter(t *testing.T) {
	store := beads.NewMemStore()
	created := seedRestartFenceSession(t, store, nil)
	m := newTestManager(t, store, runtime.NewFake())

	if err := m.RequestFreshRestartFenced(created.ID, "tok-dead"); !errors.Is(err, ErrStaleRestartRequest) {
		t.Fatalf("stale caller: err = %v, want ErrStaleRestartRequest", err)
	}
	assertRestartRequestKeys(t, store, created.ID, "")
	if err := m.RequestFreshRestartFenced(created.ID, restartFenceToken); err != nil {
		t.Fatalf("current caller: %v", err)
	}
	assertRestartRequestKeys(t, store, created.ID, "true")
}

// TestRequestFreshRestartFencedRequireRefusesAnIncapableStore proves a store
// that must fence and cannot refuses the write.
func TestRequestFreshRestartFencedRequireRefusesAnIncapableStore(t *testing.T) {
	mem := beads.NewMemStore()
	store := stampedMemStore(t, gate.Require, mem)
	created := seedRestartFenceSession(t, store, nil)
	mem.DisableConditionalWrites = true
	m := newTestManager(t, store, runtime.NewFake())

	err := m.RequestFreshRestartFenced(created.ID, restartFenceToken)
	if !beads.IsConditionalWritesRequired(err) {
		t.Fatalf("err = %v, want the require refusal", err)
	}
	assertRestartRequestKeys(t, mem, created.ID, "")
}

// countingGetStore counts reads.
type countingGetStore struct {
	beads.Store
	gets int
}

func (s *countingGetStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

// TestRequestFreshRestartFencedRefusesNoToken proves a request naming no
// incarnation is refused before any read.
func TestRequestFreshRestartFencedRefusesNoToken(t *testing.T) {
	mem := beads.NewMemStore()
	created := seedRestartFenceSession(t, mem, nil)
	store := &countingGetStore{Store: mem}
	m := newTestManager(t, store, runtime.NewFake())

	for _, token := range []string{"", "  \t"} {
		if err := m.RequestFreshRestartFenced(created.ID, token); !errors.Is(err, ErrRestartRequestUnfenced) {
			t.Fatalf("token %q: err = %v, want ErrRestartRequestUnfenced", token, err)
		}
	}
	if store.gets != 0 {
		t.Fatalf("reads = %d, want none", store.gets)
	}
	assertRestartRequestKeys(t, mem, created.ID, "")
}

// TestRequestFreshRestartFencedCoalesces proves a pending request is not
// written again.
func TestRequestFreshRestartFencedCoalesces(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedRestartFenceSession(t, store, map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	})
	m := newTestManager(t, store, runtime.NewFake())

	if err := m.RequestFreshRestartFenced(created.ID, restartFenceToken); err != nil {
		t.Fatalf("RequestFreshRestartFenced: %v", err)
	}
	got := assertRestartRequestKeys(t, store, created.ID, "true")
	if got.Revision != created.Revision {
		t.Fatalf("revision = %d, want %d (no write)", got.Revision, created.Revision)
	}
}

// TestRequestFreshRestartFencedRefusesAClosedRow proves a closed row is not
// written.
func TestRequestFreshRestartFencedRefusesAClosedRow(t *testing.T) {
	store := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	created := seedRestartFenceSession(t, store, nil)
	if err := store.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m := newTestManager(t, store, runtime.NewFake())

	if err := m.RequestFreshRestartFenced(created.ID, restartFenceToken); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("err = %v, want ErrSessionClosed", err)
	}
	assertRestartRequestKeys(t, store, created.ID, "")
}
