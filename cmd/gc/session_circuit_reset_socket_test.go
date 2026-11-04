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
