package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// readGate holds every store read back until need reads run at once, or
// fails them after wait, and records the most it saw at once.
type readGate struct {
	need int
	wait time.Duration
	open chan struct{}

	mu       sync.Mutex
	inFlight int
	maxSeen  int
}

func newReadGate(need int, wait time.Duration) *readGate {
	return &readGate{need: need, wait: wait, open: make(chan struct{})}
}

func (g *readGate) enter() error {
	g.mu.Lock()
	g.inFlight++
	g.maxSeen = max(g.maxSeen, g.inFlight)
	if g.inFlight >= g.need {
		select {
		case <-g.open:
		default:
			close(g.open)
		}
	}
	g.mu.Unlock()
	select {
	case <-g.open:
		return nil
	case <-time.After(g.wait):
		return fmt.Errorf("%d reads never ran at once", g.need)
	}
}

func (g *readGate) leave() {
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
}

// gatedStore is a store whose List runs only once its gate opens.
type gatedStore struct {
	beads.Store
	gate *readGate
}

func (s *gatedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	defer s.gate.leave()
	if err := s.gate.enter(); err != nil {
		return nil, err
	}
	return s.Store.List(q)
}

// hookedStore is a store whose List runs before first, then returns err if
// it is set; after (if set) is closed once List has returned.
type hookedStore struct {
	beads.Store
	before func()
	err    error
	after  chan struct{}
}

func (s *hookedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.after != nil {
		defer close(s.after)
	}
	if s.before != nil {
		s.before()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.List(q)
}

func cooldownOrders(n int) []orders.Order {
	aa := make([]orders.Order, n)
	for i := range aa {
		aa[i] = orders.Order{Name: fmt.Sprintf("o%02d", i), Trigger: "cooldown", Interval: "1h", Formula: "mol-x"}
	}
	return aa
}

func orderCheckOutput(t *testing.T, aa []orders.Order, stores map[string]beads.Store, jsonOutput bool) (int, string, string) {
	t.Helper()
	resolve := func(a orders.Order) ([]beads.OrdersStore, error) {
		store, ok := stores[a.Name]
		if !ok {
			return nil, fmt.Errorf("no store for %s", a.Name)
		}
		return []beads.OrdersStore{{Store: store}}, nil
	}
	var stdout, stderr bytes.Buffer
	code := doOrderCheckWithStoresResolverScopedJSON("", nil, aa, time.Now(), nil, resolve, jsonOutput, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// tableNames is the NAME column of gc order check's table, in its order.
func tableNames(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("no table header in:\n%s", out)
	}
	var names []string
	for _, l := range lines[1:] {
		names = append(names, strings.Fields(l)[0])
	}
	return names
}

func jsonNames(t *testing.T, out string) []string {
	t.Helper()
	var got orderCheckJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	var names []string
	for _, row := range got.Orders {
		names = append(names, row.Name)
	}
	return names
}

func orderNames(aa []orders.Order) []string {
	var names []string
	for _, a := range aa {
		names = append(names, a.Name)
	}
	return names
}

// TestOrderCheckReadsOrdersAtOnce: gc order check reads its orders' last runs
// orderCheckReadConcurrency at a time, never more. Read one order after
// another, each read waited for others that never came: a city of 58 orders
// took 10 s and more a check on a busy host, one bd read after another.
func TestOrderCheckReadsOrdersAtOnce(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", jsonOutput), func(t *testing.T) {
			aa := cooldownOrders(3 * orderCheckReadConcurrency)
			gate := newReadGate(orderCheckReadConcurrency, 10*time.Second)
			stores := map[string]beads.Store{}
			for _, a := range aa {
				stores[a.Name] = &gatedStore{Store: beads.NewMemStore(), gate: gate}
			}
			code, out, errOut := orderCheckOutput(t, aa, stores, jsonOutput)
			if code != 0 || errOut != "" {
				t.Fatalf("code %d (want 0, every order never run), stderr: %s", code, errOut)
			}
			if gate.maxSeen != orderCheckReadConcurrency {
				t.Fatalf("at most %d reads ran at once, want %d", gate.maxSeen, orderCheckReadConcurrency)
			}
			names := tableNames
			if jsonOutput {
				names = jsonNames
			}
			if got, want := names(t, out), orderNames(aa); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("orders %v, want %v", got, want)
			}
		})
	}
}

// TestOrderCheckPrintsInOrderAndStopsAtTheFirstError: the check prints its
// orders in their own order whichever read ends first, and the error it
// reports is the first order's in that order, as when it read them one after
// another: here o1's read fails after o3's, and o5 has no store.
func TestOrderCheckPrintsInOrderAndStopsAtTheFirstError(t *testing.T) {
	waitFor := func(t *testing.T, ch chan struct{}) func() {
		return func() {
			select {
			case <-ch:
			case <-time.After(10 * time.Second):
				t.Error("a read waited for another that never ran")
			}
		}
	}
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("in order, json=%v", jsonOutput), func(t *testing.T) {
			aa := cooldownOrders(4)
			o3done := make(chan struct{})
			stores := map[string]beads.Store{
				"o00": &hookedStore{Store: beads.NewMemStore(), before: waitFor(t, o3done)},
				"o01": beads.NewMemStore(),
				"o02": beads.NewMemStore(),
				"o03": &hookedStore{Store: beads.NewMemStore(), after: o3done},
			}
			code, out, errOut := orderCheckOutput(t, aa, stores, jsonOutput)
			if code != 0 || errOut != "" {
				t.Fatalf("code %d (want 0), stderr: %s", code, errOut)
			}
			names := tableNames
			if jsonOutput {
				names = jsonNames
			}
			if got, want := names(t, out), orderNames(aa); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("orders %v, want %v", got, want)
			}
		})
		t.Run(fmt.Sprintf("first error, json=%v", jsonOutput), func(t *testing.T) {
			aa := cooldownOrders(6)
			o3done := make(chan struct{})
			stores := map[string]beads.Store{
				"o00": beads.NewMemStore(),
				"o01": &hookedStore{Store: beads.NewMemStore(), before: waitFor(t, o3done), err: errors.New("o01 down")},
				"o02": beads.NewMemStore(),
				"o03": &hookedStore{Store: beads.NewMemStore(), err: errors.New("o03 down"), after: o3done},
				"o04": beads.NewMemStore(),
				// o05 has none: its setup fails, after o01's and o03's reads.
			}
			code, out, errOut := orderCheckOutput(t, aa, stores, jsonOutput)
			if code != 1 {
				t.Fatalf("code %d, want 1", code)
			}
			if want := "gc order check: reading last run for o01: o01 down\n"; errOut != want {
				t.Fatalf("stderr %q, want %q", errOut, want)
			}
			if jsonOutput {
				if out != "" {
					t.Fatalf("stdout %q, want none", out)
				}
				return
			}
			if got := tableNames(t, out); strings.Join(got, " ") != "o00" {
				t.Fatalf("rows %v, want o00 alone before the error", got)
			}
		})
	}
}
