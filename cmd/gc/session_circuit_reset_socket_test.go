package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// circuitResetSocketCity is a file-store city directory whose city.toml
// relocates nothing. That is the config a long-lived process saw before the
// city was migrated onto a class binding.
func circuitResetSocketCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeCLIStorageRoutes() })
	return cityDir
}

// seedOpenCircuitSessionBead plants a session bead whose persisted circuit
// breaker is OPEN, under a pinned id.
func seedOpenCircuitSessionBead(t *testing.T, store beads.Store, id string) {
	t.Helper()
	bead := beads.Bead{
		ID:     id,
		Title:  "riley",
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			sessionCircuitStateMetadata: "open",
		},
	}
	if _, err := store.Create(bead); err != nil {
		t.Fatalf("seeding session bead %s: %v", id, err)
	}
}

// runCircuitResetSocketCmd drives handleSessionCircuitResetSocketCmd over a pipe
// and returns its reply.
func runCircuitResetSocketCmd(t *testing.T, cityDir, sessionID, identity string) sessionCircuitResetReply {
	t.Helper()
	payload, err := json.Marshal(sessionCircuitResetRequest{Identity: identity, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close() //nolint:errcheck
		handleSessionCircuitResetSocketCmd(server, cityDir, string(payload))
	}()
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	<-done
	var reply sessionCircuitResetReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("decoding reply %q: %v", line, err)
	}
	return reply
}

// pinnedIDMemStore is a MemStore that keeps the ids its rows are seeded with,
// as a migrated binding keeps a relic's id.
func pinnedIDMemStore() *beads.MemStore {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	return store
}

func registerCircuitResetRuntime(t *testing.T, cityDir string, routes *storageRoutes, work beads.Store) {
	t.Helper()
	registerResidencyRoutes(cityDir, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityDir, routes) })
}

// TestSessionCircuitResetSocketReadsTheRegisteredSessionBinding is the
// production `gc session reset` failure on a split city: the session bead lives
// in the infra class binding under the relic id the migration preserved, and
// the controller answered "bead not found" because it read the work store.
//
// The socket command runs inside the controller (or supervisor) process. The
// one-shot routing funnel memoizes its answer for the life of the process, so
// once that process has resolved the city as single-store (before a
// migration), the funnel keeps answering "work" after the runtime has
// re-booted onto the binding. The runtime's registered routes are the only
// answer that tracks what this process serves now.
func TestSessionCircuitResetSocketReadsTheRegisteredSessionBinding(t *testing.T) {
	cityDir := circuitResetSocketCity(t)
	if routes := cliStorageRoutes(cityDir); routes != nil {
		t.Fatalf("precondition: a city with no [storage] resolves nil one-shot routes, got %+v", routes)
	}

	binding := pinnedIDMemStore()
	work := pinnedIDMemStore()
	routes := &storageRoutes{binding: "infra", stores: map[coordclass.Class]beads.Store{}}
	for _, class := range coordclass.Classes() {
		if class != coordclass.ClassWork {
			routes.stores[class] = binding
		}
	}
	registerCircuitResetRuntime(t, cityDir, routes, work)

	const relicID = "pl-wisp-35xhai"
	seedOpenCircuitSessionBead(t, binding, relicID)

	reply := runCircuitResetSocketCmd(t, cityDir, relicID, "riley")
	if reply.Outcome != "ok" {
		t.Fatalf("reset reply = %+v, want ok: the session bead is in the registered binding", reply)
	}
	got, err := binding.Get(relicID)
	if err != nil {
		t.Fatal(err)
	}
	if state := got.Metadata[sessionCircuitStateMetadata]; state != "" {
		t.Errorf("binding row still records circuit state %q after the reset, want it cleared", state)
	}
	if _, err := work.Get(relicID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("work store Get(%s) = %v, want ErrNotFound: the reset must not write a work-store copy", relicID, err)
	}
}

// TestSessionCircuitResetSocketReadsTheWorkStoreOnASingleStoreCity is the
// non-split case: a runtime that relocates nothing registers nil routes, and its
// session beads live in its city work store.
func TestSessionCircuitResetSocketReadsTheWorkStoreOnASingleStoreCity(t *testing.T) {
	cityDir := circuitResetSocketCity(t)
	work := pinnedIDMemStore()
	registerCircuitResetRuntime(t, cityDir, nil, work)

	const id = "demo-42"
	seedOpenCircuitSessionBead(t, work, id)

	reply := runCircuitResetSocketCmd(t, cityDir, id, "riley")
	if reply.Outcome != "ok" {
		t.Fatalf("reset reply = %+v, want ok", reply)
	}
	got, err := work.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if state := got.Metadata[sessionCircuitStateMetadata]; state != "" {
		t.Errorf("work row still records circuit state %q after the reset, want it cleared", state)
	}
}

// TestSessionCircuitResetSocketRefusesWithoutARegisteredRuntime pins the
// fail-closed answer for a socket command that arrives before the runtime has
// registered (the standalone controller starts its socket first). Falling back
// to the one-shot funnel there is the bug: it can answer from a stale memo.
func TestSessionCircuitResetSocketRefusesWithoutARegisteredRuntime(t *testing.T) {
	cityDir := circuitResetSocketCity(t)
	reply := runCircuitResetSocketCmd(t, cityDir, "demo-42", "riley")
	if reply.Outcome != "failed" {
		t.Fatalf("reset reply = %+v, want failed", reply)
	}
	if !strings.Contains(reply.Error, "not serving") {
		t.Errorf("reset error = %q, want it to say the controller is not serving the city yet", reply.Error)
	}
}

// splitRoutesOver returns routes that relocate every infrastructure class to
// binding, as a split city's runtime registers them.
func splitRoutesOver(binding beads.Store) *storageRoutes {
	routes := &storageRoutes{binding: "infra", stores: map[coordclass.Class]beads.Store{}}
	for _, class := range coordclass.Classes() {
		if class != coordclass.ClassWork {
			routes.stores[class] = binding
		}
	}
	return routes
}

// TestOneShotRoutesAnswerFromTheRegisteredRuntime pins the backstop that covers
// every in-process caller of a class resolver, not only the reset socket: the
// nudge poller, the nudge fence census and sling's named-session materialize
// all reach cliStorageRoutes from inside the controller. Once a runtime
// registers, the funnel answers with its routes, and the memo taken before the
// boot is retired so it cannot come back after the runtime stops.
func TestOneShotRoutesAnswerFromTheRegisteredRuntime(t *testing.T) {
	cityDir := circuitResetSocketCity(t)
	if routes := cliStorageRoutes(cityDir); routes != nil {
		t.Fatalf("precondition: a city with no [storage] resolves nil one-shot routes, got %+v", routes)
	}
	if !cliStorageRoutesResolved(cityDir) {
		t.Fatal("precondition: the funnel memoized its answer")
	}

	binding := pinnedIDMemStore()
	const relicID = "pl-wisp-35xhai"
	seedOpenCircuitSessionBead(t, binding, relicID)
	routes := splitRoutesOver(binding)
	registerResidencyRoutes(cityDir, routes, func() beads.Store { return pinnedIDMemStore() })
	registered := true
	t.Cleanup(func() {
		if registered {
			unregisterResidencyRoutes(cityDir, routes)
		}
	})

	if cliStorageRoutesResolved(cityDir) {
		t.Error("the pre-boot funnel memo survived the registration")
	}
	if got := cliStorageRoutes(cityDir); got != routes {
		t.Fatalf("cliStorageRoutes = %p, want the registered routes %p", got, routes)
	}
	if _, err := cliSessionStore(pinnedIDMemStore(), nil, cityDir).Get(relicID); err != nil {
		t.Fatalf("cliSessionStore Get(%s) inside a registered process: %v, want the binding row", relicID, err)
	}

	unregisterResidencyRoutes(cityDir, routes)
	registered = false
	if got := cliStorageRoutes(cityDir); got != nil {
		t.Errorf("after the runtime unregistered, cliStorageRoutes = %+v, want the city's own (nil) answer", got)
	}
}

// TestRegisteringTheFunnelsOwnRoutesKeepsThem is the one-shot `gc start` pass,
// which registers the routes the funnel itself resolved. Retiring the memo
// there would close the routes the pass is about to use.
func TestRegisteringTheFunnelsOwnRoutesKeepsThem(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	t.Cleanup(func() { _ = closeCLIStorageRoutes() })
	routes := cliStorageRoutes(cityDir)
	if routes == nil {
		t.Fatal("precondition: the split city resolved no one-shot routes")
	}
	registerResidencyRoutes(cityDir, routes, func() beads.Store { return beads.NewMemStore() })
	t.Cleanup(func() { unregisterResidencyRoutes(cityDir, routes) })

	if !cliStorageRoutesResolved(cityDir) {
		t.Fatal("registering the funnel's own routes retired them")
	}
	sessions, relocated := routes.storeFor(coordclass.ClassSessions)
	if !relocated {
		t.Fatal("precondition: the split routes relocate sessions")
	}
	if _, err := sessions.Create(beads.Bead{Title: "riley", Type: "session"}); err != nil {
		t.Fatalf("the registered funnel routes no longer serve a write: %v", err)
	}
}

// TestRegisteringARuntimeRetiresAStaleFunnelMemo checks that the retired memo's
// routes are closed rather than leaked: a supervisor re-boots a city many times
// over its life, and each stale handle would otherwise stay open until exit.
func TestRegisteringARuntimeRetiresAStaleFunnelMemo(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	t.Cleanup(func() { _ = closeCLIStorageRoutes() })
	stale := cliStorageRoutes(cityDir)
	staleSessions, relocated := stale.storeFor(coordclass.ClassSessions)
	if !relocated {
		t.Fatal("precondition: the split routes relocate sessions")
	}

	routes := splitRoutesOver(beads.NewMemStore())
	registerResidencyRoutes(cityDir, routes, func() beads.Store { return beads.NewMemStore() })
	t.Cleanup(func() { unregisterResidencyRoutes(cityDir, routes) })

	if cliStorageRoutesResolved(cityDir) {
		t.Fatal("the stale funnel memo survived the registration")
	}
	if _, err := staleSessions.Create(beads.Bead{Title: "riley", Type: "session"}); err == nil {
		t.Error("the retired funnel routes still accept a write: they were detached but never closed")
	}
}
