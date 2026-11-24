package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionSyncTestReply decodes the controller's session-sync reply without
// naming the production wire type, so this file compiles against a tree whose
// controller predates the verb and fails at runtime instead: that controller
// closes the connection without replying.
type sessionSyncTestReply struct {
	Outcome    string `json:"outcome"`
	Generation string `json:"generation"`
	Error      string `json:"error"`
}

// askSessionSync drives one session-sync exchange through the controller's
// own connection handler, as the controller socket serves it, and returns the
// decoded reply. The zero reply means the connection closed unanswered.
func askSessionSync(t *testing.T, cityPath string, wake *controllerWake, sessionID string) sessionSyncTestReply {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleControllerConn(server, cityPath, controllerHostingStandalone, func() {}, nil, nil, nil, nil, wake)
	}()
	defer func() { <-done }()
	defer client.Close() //nolint:errcheck // best-effort cleanup
	if _, err := client.Write([]byte("session-sync:{\"session_id\":" + strconv.Quote(sessionID) + "}\n")); err != nil {
		t.Fatalf("writing the verb: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck // a hung handler fails the test on read
	reply, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		return sessionSyncTestReply{}
	}
	var decoded sessionSyncTestReply
	if err := json.Unmarshal([]byte(strings.TrimSpace(reply)), &decoded); err != nil {
		t.Fatalf("decoding session-sync reply %q: %v", reply, err)
	}
	return decoded
}

// TestSessionSyncSocketRefreshesStaleCachedRow (ga-45119r AC-1, #5843): the
// controller's session census reads through its primed CachingStore, so the
// reset markers a CLI writes straight to the backing stay invisible to the
// pass a keyed poke wakes — until the socket's session-sync verb installs the
// backing row into that cache. The handler must reply ok with the installed
// row's generation, leave both markers readable through the cache, and enqueue
// the session's wake after the refresh, before replying.
func TestSessionSyncSocketRefreshesStaleCachedRow(t *testing.T) {
	backing := censusStore(censusSession("gc-1", map[string]string{
		"state": "active", "session_name": "s1", "generation": "1", "instance_token": "tok",
	}))
	pokeCh := make(chan struct{}, 1)
	// The refresh announces the row it installs synchronously; the session's
	// wake must not have been enqueued yet when it does.
	var updates, wokenBeforeRefresh int
	controllerCache := beads.NewCachingStoreForTest(backing, func(eventType, _ string, _ json.RawMessage) {
		if eventType == "bead.updated" {
			updates++
			wokenBeforeRefresh += len(pokeCh)
		}
	})
	if err := controllerCache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	cityPath := t.TempDir()
	registerResidencyRoutes(cityPath, nil, func() beads.Store { return controllerCache })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, nil) })

	// The CLI's one-shot handle writes the reset markers straight to the
	// backing, around the controller's cache.
	cliHandle := beads.Store(backing)
	if err := cliHandle.SetMetadataBatch("gc-1", map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}); err != nil {
		t.Fatalf("cli reset write: %v", err)
	}
	// Stale before: the cache still serves the row the prime read, without
	// the markers.
	cached, err := controllerCache.Get("gc-1")
	if err != nil {
		t.Fatalf("cache get before sync: %v", err)
	}
	if cached.Metadata["restart_requested"] == "true" {
		t.Fatalf("the cache already holds the reset marker; the fixture does not reproduce the stale row")
	}

	wake := newLegacyWake(pokeCh, nil)
	reply := askSessionSync(t, cityPath, wake, "gc-1")
	if reply.Outcome != "ok" {
		t.Fatalf("session-sync reply = %+v, want outcome ok", reply)
	}
	if reply.Generation != "1" {
		t.Fatalf("session-sync reply generation = %q, want \"1\" (the installed row's)", reply.Generation)
	}

	// The refresh installed the backing row: both markers now read through
	// the cache the census serves from.
	refreshed, err := controllerCache.Get("gc-1")
	if err != nil {
		t.Fatalf("cache get after sync: %v", err)
	}
	if got := refreshed.Metadata["restart_requested"]; got != "true" {
		t.Fatalf("cache row after sync: restart_requested=%q, want \"true\"", got)
	}
	if got := refreshed.Metadata["continuation_reset_pending"]; got != "true" {
		t.Fatalf("cache row after sync: continuation_reset_pending=%q, want \"true\"", got)
	}

	// The session's wake landed before the reply was written: the handler
	// enqueues after the refresh and before replying.
	select {
	case <-pokeCh:
	default:
		t.Fatalf("no wake landed for gc-1")
	}
	if updates != 1 || wokenBeforeRefresh != 0 {
		t.Fatalf("refresh announced %d updates with %d wakes already queued, want 1 update before the wake", updates, wokenBeforeRefresh)
	}
}

// syncReadCountStore counts the reads of its backing, so a test can prove the
// uncached barrier never read the row at all.
type syncReadCountStore struct {
	beads.Store
	reads int
}

func (s *syncReadCountStore) Get(id string) (beads.Bead, error) {
	s.reads++
	return s.Store.Get(id)
}

// registerSyncWorkCache registers a controller residency whose session class
// resolves to store over nil routes — the single-store shape the AC-1 proof
// used — and returns the city path. The caller unregisters with the same nil
// routes in its cleanup.
func registerSyncWorkCache(t *testing.T, store beads.Store) string {
	t.Helper()
	cityPath := t.TempDir()
	registerResidencyRoutes(cityPath, nil, func() beads.Store { return store })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, nil) })
	return cityPath
}

// syncPokeWake returns a legacy wake whose poke channel the test can read.
func syncPokeWake() (chan struct{}, *controllerWake) {
	pokeCh := make(chan struct{}, 1)
	return pokeCh, newLegacyWake(pokeCh, nil)
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

// TestSessionSyncRefreshesRoutedSessionClassCache (ga-45119r AC-7, #5843):
// on a city whose routes relocate sessions to a class binding, session-sync
// refreshes the binding's cache and leaves the city work cache alone, even
// where the work backing holds a row under the same id that changed too.
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

	pokeCh, wake := syncPokeWake()
	reply := askSessionSync(t, cityPath, wake, id)
	if reply.Outcome != "ok" || reply.Generation != "2" {
		t.Fatalf("reply = %+v, want ok generation 2", reply)
	}
	if got, err := binding.Get(id); err != nil || got.Metadata["restart_requested"] != "true" || got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("binding cache row = %v, %v; want the reset installed", got.Metadata, err)
	}
	if got, err := work.Get(id); err != nil || got.Metadata["restart_requested"] != "" {
		t.Fatalf("work cache row = %v, %v; want it untouched", got.Metadata, err)
	}
	if workChanges != 0 {
		t.Fatalf("work cache announced %d changes, want none", workChanges)
	}
	select {
	case <-pokeCh:
	default:
		t.Fatalf("no wake landed for %s", id)
	}
}

// blockingMetadataWriteStore delays the backing call of one metadata write
// until release is closed, signaling started once the write is in flight —
// after the cache has registered it as a local write, so a yielding refresh
// in between deterministically sees the write.
type blockingMetadataWriteStore struct {
	beads.Store
	started chan struct{}
	release chan struct{}
}

func (s *blockingMetadataWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	select {
	case <-s.started:
	default:
		close(s.started)
	}
	<-s.release
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestSessionSyncRetriesYieldedRefresh (ga-45119r AC-7, #5843): the refresh
// asks to yield to the controller's own in-flight local writes, so a sync
// that arrives while one is in flight must not answer busy — the handler
// retries within its budget, and the write's own absorb is what installs
// its row. The reply is ok, with the write's row visible through the cache,
// never a busy the caller would have to interpret.
func TestSessionSyncRetriesYieldedRefresh(t *testing.T) {
	backing := censusStore(censusSession("gc-1", map[string]string{
		"state": "active", "session_name": "s1", "generation": "1", "instance_token": "tok",
	}))
	block := &blockingMetadataWriteStore{Store: backing, started: make(chan struct{}), release: make(chan struct{})}
	controllerCache := beads.NewCachingStoreForTest(block, nil)
	if err := controllerCache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	cityPath := registerSyncWorkCache(t, controllerCache)

	// The controller's own write of the row, in flight: the refresh must
	// yield to it until its backing call returns.
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- controllerCache.SetMetadataBatch("gc-1", map[string]string{"session_name": "s1-renamed"})
	}()
	<-block.started
	time.AfterFunc(150*time.Millisecond, func() { close(block.release) })

	pokeCh, wake := syncPokeWake()
	reply := askSessionSync(t, cityPath, wake, "gc-1")
	if reply.Outcome != "ok" {
		t.Fatalf("session-sync reply = %+v, want outcome ok once the local write returned", reply)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("the blocked local write: %v", err)
	}
	refreshed, err := controllerCache.Get("gc-1")
	if err != nil {
		t.Fatalf("cache get after sync: %v", err)
	}
	if got := refreshed.Metadata["session_name"]; got != "s1-renamed" {
		t.Fatalf("cache row after sync: session_name=%q, want the local write's \"s1-renamed\"", got)
	}
	select {
	case <-pokeCh:
	default:
		t.Fatalf("no wake landed for gc-1")
	}
}

// TestSessionSyncUncachedStoreAcks (ga-45119r AC-7, #5843): a session store
// the census serves uncached reads live already — every read is a backing
// read — so the barrier is the wake alone: the verb answers ok without
// reading the row, and the enqueued wake is what makes the next pass see
// the caller's write.
func TestSessionSyncUncachedStoreAcks(t *testing.T) {
	work := &syncReadCountStore{Store: censusStore(censusSession("gc-1", map[string]string{
		"state": "active", "session_name": "s1", "generation": "1", "instance_token": "tok",
	}))}
	cityPath := registerSyncWorkCache(t, work)

	pokeCh, wake := syncPokeWake()
	reply := askSessionSync(t, cityPath, wake, "gc-1")
	if reply.Outcome != "ok" {
		t.Fatalf("session-sync reply = %+v, want outcome ok on an uncached store", reply)
	}
	if reply.Generation != "" {
		t.Fatalf("session-sync reply generation = %q, want the empty generation: an uncached store installed nothing", reply.Generation)
	}
	if work.reads != 0 {
		t.Fatalf("the uncached barrier read the row %d times, want 0: an uncached store's reads are already live", work.reads)
	}
	select {
	case <-pokeCh:
	default:
		t.Fatalf("no wake landed for gc-1: the wake is the whole barrier on an uncached store")
	}
}

// TestSessionSyncMissingRow (ga-45119r AC-7, #5843): a sync naming a row the
// backing does not hold answers missing — the caller learns the row it
// wrote is gone, never an ok that would claim a refresh — and the session's
// wake still lands, so a row that reappears is picked up by the next pass.
func TestSessionSyncMissingRow(t *testing.T) {
	backing := censusStore()
	controllerCache := beads.NewCachingStoreForTest(backing, nil)
	if err := controllerCache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	cityPath := registerSyncWorkCache(t, controllerCache)

	pokeCh, wake := syncPokeWake()
	reply := askSessionSync(t, cityPath, wake, "gc-gone")
	if reply.Outcome != "missing" {
		t.Fatalf("session-sync reply = %+v, want outcome missing", reply)
	}
	select {
	case <-pokeCh:
	default:
		t.Fatalf("no wake landed for gc-gone")
	}
}

// TestSessionSyncAlwaysEnqueuesWake (ga-45119r AC-7, #5843): whatever the
// refresh answers — ok, missing, busy, or failed — the session's wake lands
// before the reply: the enqueued pass is the fallback that converges every
// outcome, so no reply can leave the caller's write unobserved.
func TestSessionSyncAlwaysEnqueuesWake(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		controllerCache := beads.NewCachingStoreForTest(censusStore(censusSession("gc-1", map[string]string{
			"state": "active", "session_name": "s1", "generation": "1", "instance_token": "tok",
		})), nil)
		if err := controllerCache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		cityPath := registerSyncWorkCache(t, controllerCache)
		pokeCh, wake := syncPokeWake()
		if reply := askSessionSync(t, cityPath, wake, "gc-1"); reply.Outcome != "ok" {
			t.Fatalf("session-sync reply = %+v, want outcome ok", reply)
		}
		select {
		case <-pokeCh:
		default:
			t.Fatalf("no wake landed with the ok reply")
		}
	})
	t.Run("missing", func(t *testing.T) {
		controllerCache := beads.NewCachingStoreForTest(censusStore(), nil)
		if err := controllerCache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		cityPath := registerSyncWorkCache(t, controllerCache)
		pokeCh, wake := syncPokeWake()
		if reply := askSessionSync(t, cityPath, wake, "gc-gone"); reply.Outcome != "missing" {
			t.Fatalf("session-sync reply = %+v, want outcome missing", reply)
		}
		select {
		case <-pokeCh:
		default:
			t.Fatalf("no wake landed with the missing reply")
		}
	})
	t.Run("busy", func(t *testing.T) {
		// An unprimed cache is not live: the refresh cannot install the row,
		// so the verb answers busy — with the wake landed all the same.
		controllerCache := beads.NewCachingStoreForTest(censusStore(), nil)
		cityPath := registerSyncWorkCache(t, controllerCache)
		pokeCh, wake := syncPokeWake()
		if reply := askSessionSync(t, cityPath, wake, "gc-1"); reply.Outcome != "busy" {
			t.Fatalf("session-sync reply = %+v, want outcome busy", reply)
		}
		select {
		case <-pokeCh:
		default:
			t.Fatalf("no wake landed with the busy reply")
		}
	})
	t.Run("failed", func(t *testing.T) {
		// The backing's Get fails (Prime lists, so the cache is live): the
		// refresh reads the error and the verb answers failed.
		backing := &getErrStore{
			Store: censusStore(censusSession("gc-1", map[string]string{
				"state": "active", "session_name": "s1", "generation": "1", "instance_token": "tok",
			})),
			err: errors.New("backing read blew up"),
		}
		controllerCache := beads.NewCachingStoreForTest(backing, nil)
		if err := controllerCache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		cityPath := registerSyncWorkCache(t, controllerCache)
		pokeCh, wake := syncPokeWake()
		reply := askSessionSync(t, cityPath, wake, "gc-1")
		if reply.Outcome != "failed" {
			t.Fatalf("session-sync reply = %+v, want outcome failed", reply)
		}
		if !strings.Contains(reply.Error, "backing read blew up") {
			t.Fatalf("session-sync reply error = %q, want the refresh's own reason", reply.Error)
		}
		select {
		case <-pokeCh:
		default:
			t.Fatalf("no wake landed with the failed reply")
		}
	})
}
