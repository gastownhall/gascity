package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// frontDoorStoreFreeFiles are the cmd/gc source files whose every function was
// converted to take a dependency-injected typed front door
// (*session.InfoStore / *orders.Store / *nudgequeue.Store) in place of a raw
// bead store. They must never regress to holding a raw store: with no
// beads.Store in scope, a raw bead op on a non-work object (a session
// state-heal, a circuit-breaker metadata write, …) is *untypeable* rather than
// merely absent — the compile-time half of the object-model front-door boundary
// (engdocs/plans/infra-store-decouple/OBJECT-MODEL-FRONT-DOOR-DESIGN.md).
//
// Only files that are ENTIRELY store-free belong here. Mixed/root files
// (session_reconciler.go, cmd_nudge.go, order_dispatch.go, …) legitimately keep
// a raw store for their work/by-id/federation/graph residual and construct the
// front door inline from it — that is the front door being used, not a leak —
// so they are intentionally not listed. Add a file here once all of its
// functions take the injected front door.
var frontDoorStoreFreeFiles = []string{
	"session_circuit_breaker.go",
	"soft_reload.go",
}

// frontDoorForbiddenInStoreFreeFiles are the raw-store parameter types and the
// inline front-door constructors that must not reappear in a store-free file. A
// store-free file receives its front door already constructed at a composition
// root and threaded in.
var frontDoorForbiddenInStoreFreeFiles = []string{
	"beads.Store",
	"beads.SessionStore",
	"beads.OrdersStore",
	"beads.NudgesStore",
	"sessionFrontDoor(",
	"orders.NewStore(",
	"nudgeFrontDoor(",
	"workAssignment{",
}

// TestFrontDoorStoreFreeFilesStayStoreFree pins the front-door dependency-injection
// boundary: the fully-converted files must never reintroduce a raw store —
// neither as a parameter type nor by constructing a front door inline. Mirrors
// TestGCNonTestFilesStayOnWorkerBoundary.
func TestFrontDoorStoreFreeFilesStayStoreFree(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range frontDoorStoreFreeFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range frontDoorForbiddenInStoreFreeFiles {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw-store/front-door-construction pattern %q — this file is dependency-injection store-free; receive the typed front door (*session.InfoStore / *orders.Store / *nudgequeue.Store) as a parameter instead of holding a raw store", name, needle)
			}
		}
	}
}
