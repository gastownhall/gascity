package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// The decide's purity guard (P3 spec §4.4). The decide may not read a
// store, a provider, the filesystem, the environment or the clock, and
// takes no locks: every fact comes in through allocInputs, which carries
// data only (no store, provider or clock handle).
//
// TestDecideIsPure pins it: identical inputs give identical outputs, and the
// decide's files neither import an I/O, environment, logging or lock package
// nor call the clock, the filesystem helpers or fsys.

// decideBannedPackages are packages the decide's files may not import.
var decideBannedPackages = map[string]bool{
	"os": true, "os/exec": true, "os/signal": true, "os/user": true, "log": true, "log/slog": true,
	"syscall": true, "net": true, "net/http": true, "io/ioutil": true, "sync": true, "sync/atomic": true,
	"github.com/gastownhall/gascity/internal/fsys": true,
}

// decideBannedCalls are package functions the decide's files may not
// reference: the clock and the filesystem path resolvers.
var decideBannedCalls = map[string]bool{
	"time.Now": true, "time.Since": true, "time.Until": true, "time.Sleep": true, "time.After": true,
	"time.Tick": true, "time.NewTimer": true, "time.NewTicker": true, "time.AfterFunc": true,
	"path/filepath.Abs": true, "path/filepath.EvalSymlinks": true, "path/filepath.Glob": true, "path/filepath.Walk": true,
	"path/filepath.WalkDir": true,
}

// TestDecideIsPure pins the decide's purity in the fast suite: identical
// inputs give identical outputs (map iteration included), and the P3-5a
// files import no I/O or lock package and reference no clock or filesystem
// function.
//
// Kills: a time.Now (or any banned call) in the decide's files, an os, sync
// or fsys import, nondeterministic output.
func TestDecideIsPure(t *testing.T) {
	in := purityInputs(t)
	first := mustDecide(t, in)
	for i := 0; i < 20; i++ {
		if got := mustDecide(t, in); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs from the first: the decide is nondeterministic\n got  %s\n want %s", i, describeDecision(got), describeDecision(first))
		}
	}
	if len(first.Plans) == 0 || len(first.Snapshot.Entries) < 5 {
		t.Fatalf("the fixture must exercise plans and entries: %s", describeDecision(first))
	}
	if e := entryOf(t, first, "gc-5"); e.Desired != desireNone || e.Reason != reasonIdentityDuplicate {
		t.Fatalf("named duplicate gc-5 = %s/%s, want None(identity-duplicate)", e.Desired, e.Reason)
	}
	if _, err := decideAllocation(allocInputs{}); err == nil {
		t.Fatal("a pass with a zero Now must be refused")
	}
	checkPureFiles(t, decideFiles)
}

// checkPureFiles fails t if a file imports a banned package or references a
// banned call: a decide's facts all come in through its inputs.
func checkPureFiles(t *testing.T, files []string) {
	t.Helper()
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		imports := make(map[string]string) // local name -> path
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if decideBannedPackages[p] {
				t.Errorf("%s imports %s: the decide takes no locks and reads nothing", name, p)
			}
			local := path.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok {
				if call := imports[x.Name] + "." + sel.Sel.Name; decideBannedCalls[call] {
					t.Errorf("%s: %s references %s: the clock and the filesystem come in through the decide's inputs", fset.Position(sel.Pos()), name, call)
				}
			}
			return true
		})
	}
}

// decideFiles are the decide's files, A2's realization index, admission,
// decideRow's arm table and arms, the dirty filter and the identity
// comparator.
var decideFiles = []string{
	"allocator_decide.go", "allocator_plan.go", "allocator_snapshot.go", "allocator_index.go", "reconcile_admit.go",
	"reconcile_session_decide.go", "reconcile_arms_identity.go", "reconcile_dirty.go", "reconcile_identity.go",
	"reconcile_stop_request.go", "reconcile_arms_drain.go",
}

// purityInputs is a city that exercises every step: pool reuse and plans,
// named sessions (several planned at once, so plan order is tested), an
// overlay row, a named duplicate, a rollback candidate, an unknown-state
// row, two start-lease holders of one trigger and a live fence backoff.
func purityInputs(t *testing.T) allocInputs {
	t.Helper()
	cfg := &config.City{
		Agents: []config.Agent{
			allocPoolAgent("worker", 7),
			{Name: "db", MaxActiveSessions: intPtr(2)},
			{Name: "app", MaxActiveSessions: intPtr(3)},
			{Name: "chat"},
			{Name: "alpha"},
			{Name: "beta"},
			{Name: "gamma"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "chat", Mode: "always"},
			{Template: "alpha", Mode: "always"},
			{Template: "beta", Mode: "always"},
			{Template: "gamma", Mode: "always"},
		},
	}
	f := newAllocFixture(t, cfg)
	f.sessions(
		poolRow("gc-1", "worker", 1, "active"),
		poolRow("gc-2", "worker", 2, "asleep"),
		poolRow("gc-3", "worker", 3, "creating", "pending_create_claim", "true",
			"pending_create_started_at", allocNow.Add(-30*time.Minute).Format(time.RFC3339)),
		sessionRow("gc-4", "template", "chat", "state", "asleep", "session_name", "s-gc-4",
			"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always", "generation", "2"),
		sessionRow("gc-5", "template", "chat", "state", "asleep", "session_name", "s-gc-5",
			"configured_named_session", "true", "configured_named_identity", "chat", "configured_named_mode", "always", "generation", "1"),
		sessionRow("gc-6", "template", "worker", "state", "active", "session_name", "manual-1", "manual_session", "true"),
		sessionRow("gc-7", "template", "worker", "state", "draining-enterprise", "session_name", "s-gc-7"),
		poolRow("gc-8", "worker", 4, "creating", "gc.trigger_bead_id", "w-9", "pending_create_claim", "true",
			"pending_create_started_at", allocNow.Add(-10*time.Second).Format(time.RFC3339)),
		poolRow("gc-9", "worker", 5, "creating", "gc.trigger_bead_id", "w-9", "pending_create_claim", "true",
			"pending_create_started_at", allocNow.Add(-10*time.Second).Format(time.RFC3339)),
	).alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).demand("worker", "w-1", "w-2", "w-3", "w-4", "w-9").demand("app", "w-5")
	f.in.Backoff = createRefusal("worker/worker-6", createStageFence, allocNow.Add(time.Minute))
	return f.inputs()
}

// describeDecision renders a decision deterministically for failure output.
func describeDecision(d allocDecision) string {
	var b strings.Builder
	ids := entryIDs(d)
	sort.Strings(ids)
	fmt.Fprintf(&b, "mode=%s entries=%v plans=", d.Snapshot.Mode, ids)
	for _, p := range d.Plans {
		fmt.Fprintf(&b, "%s/%s ", p.Kind, p.identity())
	}
	return b.String()
}
