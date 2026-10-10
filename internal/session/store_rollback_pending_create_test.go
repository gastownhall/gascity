package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// closedReadHookStore runs hook once, right after the first read that returns
// the row closed. For RollbackPendingCreateAtomically that read is the
// post-close step's re-read, so hook lands between that re-read and its fenced
// write. It points conditional-write resolution at its inner store, as the
// production wrappers do, so the post-close write is fenced.
type closedReadHookStore struct {
	beads.Store
	hook func(id string)
}

func (s *closedReadHookStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func (s *closedReadHookStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil && b.Status == "closed" && s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook(id)
	}
	return b, err
}

func seedRollbackPendingSession(t *testing.T, store beads.Store) (beads.Bead, Info) {
	t.Helper()
	created, err := store.Create(sessionBeadFixture("rollback", "open", map[string]string{
		"state":                string(StateCreating),
		"session_name":         "worker-1",
		"generation":           "1",
		"instance_token":       "tok-1",
		"pending_create_claim": "true",
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	info, _, err := NewStore(beads.SessionStore{Store: store}).GetPersistedResponse(created.ID)
	if err != nil {
		t.Fatalf("GetPersistedResponse: %v", err)
	}
	return created, info
}

func rollbackClosePatch() MetadataPatch {
	patch := ClosePatch(closeTestNow, string(StateFailedCreate))
	patch["pending_create_claim"] = ""
	return patch
}

// fencedAtomicCloseBackends are the patch-fence backends whose post-close
// write is revision-fenced and which provide the atomic close.
func fencedAtomicCloseBackends(t *testing.T) []patchFenceBackend {
	t.Helper()
	var out []patchFenceBackend
	for _, backend := range patchFenceBackends() {
		store, _ := backend.open(t)
		if _, ok := beads.AtomicConditionalCloserFor(store); backend.fenced && ok {
			out = append(out, backend)
		}
	}
	if len(out) == 0 {
		t.Fatal("no fenced backend provides the atomic close")
	}
	return out
}

// TestRollbackPendingCreateAtomicallyRetriesALostPostCloseFence covers the
// fenced post-close write. A writer that touches the closed row between the
// re-read and the session_name clear wins the fence. The clear re-reads, finds
// the row still closed, and lands on the retry without undoing that write.
func TestRollbackPendingCreateAtomicallyRetriesALostPostCloseFence(t *testing.T) {
	for _, backend := range fencedAtomicCloseBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created, info := seedRollbackPendingSession(t, store)
			hooked := &closedReadHookStore{Store: store, hook: func(id string) {
				if err := backing.SetMetadata(id, "test_archived_note", "kept"); err != nil {
					t.Errorf("SetMetadata: %v", err)
				}
			}}
			front := NewStore(beads.SessionStore{Store: hooked})

			closed, postClosed, err := front.RollbackPendingCreateAtomically(info, rollbackClosePatch(), MetadataPatch{"session_name": ""})
			if err != nil || !closed || !postClosed {
				t.Fatalf("RollbackPendingCreateAtomically = (%v, %v, %v), want (true, true, nil)", closed, postClosed, err)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" || got.Metadata["state"] != string(StateFailedCreate) ||
				got.Metadata["session_name"] != "" || got.Metadata["test_archived_note"] != "kept" {
				t.Fatalf("row = status %q state %q session_name %q archived_note %q, want closed failed-create, name released, the other write kept",
					got.Status, got.Metadata["state"], got.Metadata["session_name"], got.Metadata["test_archived_note"])
			}
		})
	}
}

// TestRollbackPendingCreateAtomicallyLeavesAReopenedRowItsName covers the
// fenced post-close write against a reopen that lands between the re-read and
// the clear: the fence refuses the write, the re-read finds the row open, and
// its name stays.
func TestRollbackPendingCreateAtomicallyLeavesAReopenedRowItsName(t *testing.T) {
	for _, backend := range fencedAtomicCloseBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			store, backing := backend.open(t)
			created, info := seedRollbackPendingSession(t, store)
			hooked := &closedReadHookStore{Store: store, hook: func(id string) {
				open := "open"
				if err := backing.Update(id, beads.UpdateOpts{Status: &open}); err != nil {
					t.Errorf("reopen: %v", err)
				}
			}}
			front := NewStore(beads.SessionStore{Store: hooked})

			closed, postClosed, err := front.RollbackPendingCreateAtomically(info, rollbackClosePatch(), MetadataPatch{"session_name": ""})
			if err != nil || !closed || postClosed {
				t.Fatalf("RollbackPendingCreateAtomically = (%v, %v, %v), want (true, false, nil)", closed, postClosed, err)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || got.Metadata["session_name"] != "worker-1" {
				t.Fatalf("row = status %q session_name %q, want the reopened row left named", got.Status, got.Metadata["session_name"])
			}
		})
	}
}

// TestRollbackPendingCreateAtomicallyReportsUnsupportedWithoutWriting pins the
// fallback signal: a store without the atomic close gets an error the caller
// recognizes as unsupported, and nothing is written.
func TestRollbackPendingCreateAtomicallyReportsUnsupportedWithoutWriting(t *testing.T) {
	store := beads.NewMemStore()
	created, info := seedRollbackPendingSession(t, store)
	front := NewStore(beads.SessionStore{Store: store})

	closed, postClosed, err := front.RollbackPendingCreateAtomically(info, rollbackClosePatch(), MetadataPatch{"session_name": ""})
	if closed || postClosed || !beads.IsConditionalWriteUnsupported(err) {
		t.Fatalf("RollbackPendingCreateAtomically = (%v, %v, %v), want (false, false, unsupported)", closed, postClosed, err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Metadata["session_name"] != "worker-1" || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("row = status %q session_name %q claim %q, want it untouched", got.Status, got.Metadata["session_name"], got.Metadata["pending_create_claim"])
	}
}
