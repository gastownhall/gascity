package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
	controllerCache := beads.NewCachingStoreForTest(backing, nil)
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

	pokeCh := make(chan struct{}, 1)
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
}
