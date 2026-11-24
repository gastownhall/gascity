package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionSyncHarness is a controller's view of one session row, split from
// the CLI's: a primed CachingStore registered as the controller runtime's
// session store, and the shared backing a one-shot CLI handle writes to
// directly, so no event, feed or re-scan reaches the cache.
type sessionSyncHarness struct {
	cityPath string
	backing  *beads.MemStore
	cache    *beads.CachingStore
	id       string
}

func newSessionSyncHarness(t *testing.T) *sessionSyncHarness {
	t.Helper()
	return newSessionSyncHarnessNotifying(t, nil)
}

// newSessionSyncHarnessNotifying is newSessionSyncHarness with onChange
// hearing the cache's change notifications as they are made.
func newSessionSyncHarnessNotifying(t *testing.T, onChange func(eventType, beadID string, payload json.RawMessage)) *sessionSyncHarness {
	t.Helper()
	backing := beads.NewMemStore()
	created, err := backing.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":          "active",
			"session_name":   "s-sync",
			"generation":     "4",
			"instance_token": "tok-4",
		},
	})
	if err != nil {
		t.Fatalf("seeding session row: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, onChange)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	cityPath := t.TempDir()
	registerResidencyRoutes(cityPath, nil, func() beads.Store { return cache })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, nil) })
	return &sessionSyncHarness{cityPath: cityPath, backing: backing, cache: cache, id: created.ID}
}

// cliReset writes the reset markers the way `gc session reset` does, through
// a store handle of its own on the shared backing.
func (h *sessionSyncHarness) cliReset(t *testing.T) {
	t.Helper()
	cli := beads.Store(h.backing)
	if err := cli.SetMetadataBatch(h.id, map[string]string{"restart_requested": "true", "continuation_reset_pending": "true"}); err != nil {
		t.Fatalf("cli reset write: %v", err)
	}
}

// runSessionSyncCommand drives handleControllerConn with one session-sync
// line under wake and decodes the reply.
func runSessionSyncCommand(t *testing.T, cityPath, sessionID string, wake *controllerWake) sessionSyncReply {
	t.Helper()
	payload, err := json.Marshal(sessionSyncRequest{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityPath, controllerHostingStandalone, func() {}, nil, nil, nil, nil, wake)
		close(done)
	}()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := client.Write([]byte(sessionSyncCommandPrefix + string(payload) + "\n")); err != nil {
		t.Fatalf("write command: %v", err)
	}
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read session-sync reply: %v", err)
	}
	client.Close() //nolint:errcheck
	awaitClose(t, done, "handleControllerConn to exit")
	var reply sessionSyncReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("decoding reply %q: %v", line, err)
	}
	return reply
}

// TestSessionSyncSocketRefreshesStaleCachedRow is #5843's visibility gap:
// the CLI's reset lands on the backing past the controller's primed cache,
// and session-sync makes the cache hold it before the reply, with the
// session's wake enqueued after the refresh.
func TestSessionSyncSocketRefreshesStaleCachedRow(t *testing.T) {
	pokeCh := make(chan struct{}, 1)
	// The refresh announces the row it installs synchronously; the wake must
	// not have been enqueued yet when it does.
	var updates, wokenBeforeRefresh int
	h := newSessionSyncHarnessNotifying(t, func(eventType, _ string, _ json.RawMessage) {
		if eventType == "bead.updated" {
			updates++
			wokenBeforeRefresh += len(pokeCh)
		}
	})
	h.cliReset(t)
	if got, err := h.cache.Get(h.id); err != nil || got.Metadata["restart_requested"] != "" {
		t.Fatalf("precondition: cache Get = %v, %v; want the stale clean row", got.Metadata, err)
	}

	reply := runSessionSyncCommand(t, h.cityPath, h.id, newLegacyWake(pokeCh, make(chan struct{}, 1)))
	if reply.Outcome != sessionSyncOK || reply.Generation != "4" {
		t.Fatalf("reply = %+v, want ok with generation 4", reply)
	}
	got, err := h.cache.Get(h.id)
	if err != nil {
		t.Fatalf("cache Get: %v", err)
	}
	if got.Metadata["restart_requested"] != "true" || got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("cache row after session-sync = %v, want both reset markers", got.Metadata)
	}
	if len(pokeCh) != 1 {
		t.Fatal("session-sync enqueued no wake")
	}
	if updates != 1 || wokenBeforeRefresh != 0 {
		t.Fatalf("refresh announced %d updates with %d wakes already queued, want 1 update before the wake", updates, wokenBeforeRefresh)
	}
}

// fakeRowRefresher answers RefreshRowReport from a script of errors, then ok.
type fakeRowRefresher struct {
	errs       []error
	calls      int
	reconciles int
}

func (f *fakeRowRefresher) RefreshRowReport(string, beads.RowRefreshOptions) (beads.RowRefresh, error) {
	f.calls++
	if f.calls <= len(f.errs) {
		return beads.RowRefresh{}, f.errs[f.calls-1]
	}
	return beads.RowRefresh{Found: true, Bead: beads.Bead{Metadata: map[string]string{"generation": "9"}}}, nil
}

func (f *fakeRowRefresher) RequestReconcile() { f.reconciles++ }

// TestSessionSyncRetriesYieldedRefresh: a refresh that yields to a local
// write is retried with backoff; one that stays contended past the budget
// replies busy and requests a re-scan.
func TestSessionSyncRetriesYieldedRefresh(t *testing.T) {
	yielded := fmt.Errorf("refresh x: %w", beads.ErrRowRefreshYielded)
	t.Run("yield then ok", func(t *testing.T) {
		f := &fakeRowRefresher{errs: []error{yielded, yielded}}
		var slept []time.Duration
		reply := syncSessionRow(f, "x", func(d time.Duration) { slept = append(slept, d) })
		if reply.Outcome != sessionSyncOK || reply.Generation != "9" {
			t.Fatalf("reply = %+v, want ok generation 9", reply)
		}
		if f.calls != 3 || len(slept) != 2 || slept[1] <= slept[0] {
			t.Fatalf("calls=%d slept=%v, want 3 calls with doubling backoff", f.calls, slept)
		}
		if f.reconciles != 0 {
			t.Fatalf("reconciles = %d, want none on ok", f.reconciles)
		}
	})
	t.Run("outlasted", func(t *testing.T) {
		old := sessionSyncRefreshBudget
		sessionSyncRefreshBudget = 30 * time.Millisecond
		t.Cleanup(func() { sessionSyncRefreshBudget = old })
		f := &fakeRowRefresher{errs: make([]error, 1000)}
		for i := range f.errs {
			f.errs[i] = fmt.Errorf("refresh x: %w", beads.ErrRowRefreshFenced)
		}
		reply := syncSessionRow(f, "x", time.Sleep)
		if reply.Outcome != sessionSyncBusy || reply.Error == "" {
			t.Fatalf("reply = %+v, want busy with a reason", reply)
		}
		if f.reconciles != 1 {
			t.Fatalf("reconciles = %d, want 1", f.reconciles)
		}
	})
	t.Run("cache unavailable", func(t *testing.T) {
		f := &fakeRowRefresher{errs: []error{beads.ErrCacheUnavailable}}
		if reply := syncSessionRow(f, "x", time.Sleep); reply.Outcome != sessionSyncBusy || f.reconciles != 1 {
			t.Fatalf("reply = %+v reconciles=%d, want busy and one re-scan", reply, f.reconciles)
		}
	})
	t.Run("other error", func(t *testing.T) {
		f := &fakeRowRefresher{errs: []error{errors.New("backing down")}}
		if reply := syncSessionRow(f, "x", time.Sleep); reply.Outcome != sessionSyncFailed || f.reconciles != 1 {
			t.Fatalf("reply = %+v reconciles=%d, want failed and one re-scan", reply, f.reconciles)
		}
	})
}

// TestSessionSyncUncachedStoreAcks: a runtime whose session store is not
// cached reads the store directly, so session-sync acknowledges at once.
func TestSessionSyncUncachedStoreAcks(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	registerResidencyRoutes(cityPath, nil, func() beads.Store { return store })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, nil) })
	pokeCh := make(chan struct{}, 1)
	reply := runSessionSyncCommand(t, cityPath, "gc-1", newLegacyWake(pokeCh, make(chan struct{}, 1)))
	if reply.Outcome != sessionSyncOK {
		t.Fatalf("reply = %+v, want ok", reply)
	}
	if len(pokeCh) != 1 {
		t.Fatal("session-sync enqueued no wake")
	}
}

// TestSessionSyncMissingRow: a row the backing does not have replies
// missing, and the wake still goes out.
func TestSessionSyncMissingRow(t *testing.T) {
	h := newSessionSyncHarness(t)
	pokeCh := make(chan struct{}, 1)
	reply := runSessionSyncCommand(t, h.cityPath, "gc-nope", newLegacyWake(pokeCh, make(chan struct{}, 1)))
	if reply.Outcome != sessionSyncMissing {
		t.Fatalf("reply = %+v, want missing", reply)
	}
	if len(pokeCh) != 1 {
		t.Fatal("session-sync enqueued no wake")
	}
}

// TestSessionSyncAlwaysEnqueuesWake: even a controller with no registered
// runtime (starting or stopping) enqueues the session's wake, and replies
// busy rather than ok.
func TestSessionSyncAlwaysEnqueuesWake(t *testing.T) {
	pokeCh := make(chan struct{}, 1)
	reply := runSessionSyncCommand(t, t.TempDir(), "gc-1", newLegacyWake(pokeCh, make(chan struct{}, 1)))
	if reply.Outcome != sessionSyncBusy || reply.Error == "" {
		t.Fatalf("reply = %+v, want busy with a reason", reply)
	}
	if len(pokeCh) != 1 {
		t.Fatal("session-sync enqueued no wake")
	}
}

// primedPinnedCache returns a primed cache over a backing that keeps seeded
// ids, holding a clean copy of session row id, and the backing.
func primedPinnedCache(t *testing.T, id string, onChange func(eventType, beadID string, payload json.RawMessage)) (*beads.CachingStore, *beads.MemStore) {
	t.Helper()
	backing := pinnedIDMemStore()
	if _, err := backing.Create(beads.Bead{
		ID: id, Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"state": "active", "session_name": "s-routed", "generation": "2"},
	}); err != nil {
		t.Fatalf("seeding %s: %v", id, err)
	}
	cache := beads.NewCachingStoreForTest(backing, onChange)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	return cache, backing
}

// TestSessionSyncRefreshesRoutedSessionClassCache: on a city whose routes
// relocate sessions to a class binding, session-sync refreshes the binding's
// cache and leaves the city work cache alone, even where the work backing
// holds a row under the same id that changed too.
func TestSessionSyncRefreshesRoutedSessionClassCache(t *testing.T) {
	const id = "pl-wisp-routed"
	var workChanges int
	work, workBacking := primedPinnedCache(t, id, func(string, string, json.RawMessage) { workChanges++ })
	binding, bindingBacking := primedPinnedCache(t, id, nil)
	cityPath := t.TempDir()
	routes := splitRoutesOver(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	reset := map[string]string{"restart_requested": "true", "continuation_reset_pending": "true"}
	for _, backing := range []*beads.MemStore{bindingBacking, workBacking} {
		if err := backing.SetMetadataBatch(id, reset); err != nil {
			t.Fatalf("cli reset write: %v", err)
		}
	}

	reply := runSessionSyncCommand(t, cityPath, id, newLegacyWake(make(chan struct{}, 1), make(chan struct{}, 1)))
	if reply.Outcome != sessionSyncOK || reply.Generation != "2" {
		t.Fatalf("reply = %+v, want ok generation 2", reply)
	}
	if got, err := binding.Get(id); err != nil || got.Metadata["restart_requested"] != "true" {
		t.Fatalf("binding cache row = %v, %v; want the reset installed", got.Metadata, err)
	}
	if got, err := work.Get(id); err != nil || got.Metadata["restart_requested"] != "" {
		t.Fatalf("work cache row = %v, %v; want it untouched", got.Metadata, err)
	}
	if workChanges != 0 {
		t.Fatalf("work cache announced %d changes, want none", workChanges)
	}
}
