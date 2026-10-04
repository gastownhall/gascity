package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The decide's purity guard (P3 spec §4.4). The decide may not read a
// store, a provider, the filesystem, the environment or the clock, and
// takes no locks: every fact comes in through allocInputs.
//
// Two tests pin it. TestDecideIsPure (fast suite, here) runs the decide over
// a census read from stores that panic once the read is done, checks that
// identical inputs give identical outputs, and that the decide's files
// import no lock or I/O package. TestDecideReachesNoIO (integration tier,
// allocator_decide_reach_integration_test.go) type-checks the module with
// go/packages, builds SSA for it and every dependency, and walks every
// function decideAllocation can reach, flagging I/O, the clock, the
// environment, logging and locks.

// decideIOPackages are packages any function of which is I/O, the
// environment or logging.
var decideIOPackages = map[string]bool{
	"os": true, "os/exec": true, "os/signal": true, "os/user": true, "log": true, "log/slog": true,
	"syscall": true, "net": true, "net/http": true, "io/ioutil": true,
}

// purityStore is a census leg's store that panics on every call once
// armed: the decide must never reach back into a store it was given rows
// from.
type purityStore struct {
	beads.Store
	armed *atomic.Bool
}

func (s purityStore) guard(op string) {
	if s.armed.Load() {
		panic("allocator decide called the store: " + op)
	}
}

func (s purityStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.guard("List")
	return s.Store.List(q)
}

func (s purityStore) Get(id string) (beads.Bead, error) {
	s.guard("Get")
	return s.Store.Get(id)
}

func (s purityStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.guard("Ready")
	return s.Store.Ready(q...)
}

// TestDecideIsPure pins the decide's purity in the fast suite: it runs over
// a census whose stores panic once read, identical inputs give identical
// outputs (map iteration included), and the P3-5a files import no lock or
// I/O package. TestDecideReachesNoIO walks the call graph.
//
// Kills: the decide reading a census store, nondeterministic output, a lock
// or os import in the decide's files.
func TestDecideIsPure(t *testing.T) {
	armed := &atomic.Bool{}
	in := purityInputs(t, armed)
	armed.Store(true)
	first := mustDecide(t, in)
	for i := 0; i < 20; i++ {
		if got := mustDecide(t, in); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs from the first: the decide is nondeterministic\n got  %s\n want %s", i, describeDecision(got), describeDecision(first))
		}
	}
	if len(first.Snapshot.Entries) < 5 {
		t.Fatalf("the fixture must exercise plans and entries: %s", describeDecision(first))
	}
	if _, err := decideAllocation(allocInputs{}); err == nil {
		t.Fatal("a pass with a zero Now must be refused")
	}
	fset := token.NewFileSet()
	for _, name := range decideFiles {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); p == "sync" || p == "sync/atomic" || decideIOPackages[p] {
				t.Errorf("%s imports %s: the decide takes no locks and reads nothing", name, p)
			}
		}
	}
}

// decideFiles are the P3-5a files.
var decideFiles = []string{"allocator_decide.go", "allocator_snapshot.go"}

// purityInputs is a city that exercises every step: pool rows, named
// sessions, a manual
// row, a dependency, identity verdicts, a rollback candidate and an
// unknown-state row. With armed set, its census stores panic once armed.
func purityInputs(t *testing.T, armed *atomic.Bool) allocInputs {
	t.Helper()
	cfg := &config.City{
		Agents: []config.Agent{
			allocPoolAgent("worker", 4),
			{Name: "db", MaxActiveSessions: intPtr(2)},
			{Name: "app", MaxActiveSessions: intPtr(3), DependsOn: []string{"db"}},
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
	).alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
	if armed != nil {
		for i := range f.legs {
			f.legs[i].store = purityStore{Store: f.legs[i].store, armed: armed}
		}
	}
	in := f.in
	in.Census = f.census()
	in.Obs = f.observation()
	return in
}

// describeDecision renders a decision deterministically for failure output.
func describeDecision(d allocDecision) string {
	var b strings.Builder
	ids := entryIDs(d)
	sort.Strings(ids)
	fmt.Fprintf(&b, "mode=%s entries=%v", d.Snapshot.Mode, ids)
	return b.String()
}
