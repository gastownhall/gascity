package orders

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// The T7b.2 order reads over legs against the frozen reads of main
// (store_legs_reference_test.go): every op runs once frozen and once live on
// fresh copies of one seeded fixture, and the results, the logged failures,
// and the ordered reads that reached each backing store (op, query, and the
// leg role on the reading stack, as the split-strict gate reads it) must
// match. The live side is the leg form over a strict, a federated and a relic
// leg (whose reads must carry that role and nothing else differs), and
// today's refactored methods (which must match exactly).

// legsLog is one fixture's shared record of backing reads and logged lines.
type legsLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *legsLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *legsLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.lines
	l.lines = nil
	return out
}

// legsRecordingStore records each backing read with the leg role on its
// stack; listErr fails a List outright, partialErr returns the rows with it.
type legsRecordingStore struct {
	beads.Store
	name       string
	log        *legsLog
	listErr    error
	partialErr error
}

func (s *legsRecordingStore) note(op string, arg any) {
	pcs := make([]uintptr, 64)
	role := beads.LegRoleOnStack(pcs[:runtime.Callers(1, pcs)])
	s.log.add(fmt.Sprintf("%s %s %+v role=%v", s.name, op, arg, role))
}

func (s *legsRecordingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.note("List", q)
	if s.listErr != nil {
		return nil, s.listErr
	}
	rows, err := s.Store.List(q)
	if err == nil && s.partialErr != nil {
		err = s.partialErr
	}
	return rows, err
}

func (s *legsRecordingStore) Get(id string) (beads.Bead, error) {
	s.note("Get", id)
	return s.Store.Get(id)
}

var (
	errLegsDown    = errors.New("store down")
	errLegsPartial = errors.New("one tier unreadable")
)

// legsStore is one store's order-run evidence, ids and timestamps fixed:
// tracking beads open and closed with event seqs, a created-at tie (the open
// o1 run and the closed o2 run), a graph root, a newest plain bead carrying
// the run label (no run record: it forces NewestRunRecords' second read), and
// a tracking bead with no run label.
func legsStore(seqBase int) *beads.MemStore {
	base := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	id := func(i int) string { return fmt.Sprintf("gc-%d%02d", seqBase, i) }
	seed := []beads.Bead{
		{ID: id(0), Title: "order:o1", Status: "closed", Labels: []string{"order-run:o1", "order-tracking", fmt.Sprintf("seq:%d", seqBase+5)}, CreatedAt: base},
		{ID: id(1), Title: "order:o1", Status: "open", Labels: []string{"order-run:o1", "order-tracking", fmt.Sprintf("seq:%d", seqBase+7)}, CreatedAt: base.Add(time.Minute)},
		{ID: id(2), Title: "order:o2", Status: "closed", Labels: []string{"order-run:o2", "order-tracking"}, CreatedAt: base.Add(time.Minute)},
		{ID: id(3), Title: "wisp: o1", Status: "open", Type: "molecule", Labels: []string{"order-run:o1", fmt.Sprintf("seq:%d", seqBase+9)}, CreatedAt: base.Add(2 * time.Minute)},
		{ID: id(4), Title: "found by o1", Status: "open", Type: "task", Labels: []string{"order-run:o1", "seq:999"}, CreatedAt: base.Add(3 * time.Minute)},
		{ID: id(5), Title: "order:legacy", Status: "open", Labels: []string{"order-tracking"}, CreatedAt: base.Add(4 * time.Minute)},
	}
	for i := range seed {
		seed[i].UpdatedAt = seed[i].CreatedAt
	}
	return beads.NewMemStoreFrom(len(seed), seed, nil)
}

// legsShape builds one store of a shape over a fresh seeded fixture.
type legsShape func(t *testing.T, log *legsLog, name string, seqBase int) beads.Store

var legsShapes = map[string]legsShape{
	"mem": func(_ *testing.T, log *legsLog, name string, seqBase int) beads.Store {
		m := legsStore(seqBase)
		return &legsRecordingStore{Store: m, name: name, log: log}
	},
	"cache warm": func(t *testing.T, log *legsLog, name string, seqBase int) beads.Store {
		m := legsStore(seqBase)
		c := beads.NewCachingStoreForTest(&legsRecordingStore{Store: m, name: name, log: log}, nil)
		if err := c.Prime(t.Context()); err != nil {
			t.Fatal(err)
		}
		log.take()
		return c
	},
	"cache cold": func(_ *testing.T, log *legsLog, name string, seqBase int) beads.Store {
		m := legsStore(seqBase)
		return beads.NewCachingStoreForTest(&legsRecordingStore{Store: m, name: name, log: log}, nil)
	},
	"failing": func(_ *testing.T, log *legsLog, name string, seqBase int) beads.Store {
		m := legsStore(seqBase)
		return &legsRecordingStore{Store: m, name: name, log: log, listErr: errLegsDown}
	},
	"partial": func(_ *testing.T, log *legsLog, name string, seqBase int) beads.Store {
		m := legsStore(seqBase)
		return &legsRecordingStore{Store: m, name: name, log: log, partialErr: errLegsPartial}
	},
}

// legsMints are the leg forms the live side reads through, with the role its
// reads must carry.
var legsMints = map[string]struct {
	mint func(beads.Store) beads.FederatedReader
	role beads.LegRole
}{
	"strict":    {beadstest.StrictLeg, beads.NoLegRole},
	"federated": {beadstest.FederatedLeg, beads.FederatedLegRole},
	"relic":     {beadstest.RelicLeg, beads.RelicLegRole},
}

// legsOp is one read, frozen and live, over the stores a fixture built.
type legsOp struct {
	name   string
	frozen func(stores []beads.Store) string
	method func(stores []beads.Store) string
	legs   func(legs []beads.FederatedReader) string
}

func legsRender(v any, err error) string {
	if err != nil {
		return fmt.Sprintf("%+v err=%v", v, err)
	}
	return fmt.Sprintf("%+v", v)
}

func legsOps() []legsOp {
	var ops []legsOp
	for _, limit := range []int{0, 1, 3, 50} {
		ops = append(ops, legsOp{
			name: fmt.Sprintf("ListTracking(%d)", limit),
			frozen: func(s []beads.Store) string {
				return legsRender(frozenListTracking(NewStore(beads.OrdersStore{Store: s[0]}), limit))
			},
			method: func(s []beads.Store) string {
				return legsRender(NewStore(beads.OrdersStore{Store: s[0]}).ListTracking(limit))
			},
			legs: func(l []beads.FederatedReader) string { return legsRender(ListTrackingOn(l[0], limit)) },
		})
	}
	for _, scoped := range []string{"o1", "o2", "none"} {
		ops = append(ops, legsOp{
			name: "LatestOpenRun(" + scoped + ")",
			frozen: func(s []beads.Store) string {
				run, found, err := frozenLatestOpenRun(NewStore(beads.OrdersStore{Store: s[0]}), scoped)
				return legsRender(fmt.Sprint(run, found), err)
			},
			method: func(s []beads.Store) string {
				run, found, err := NewStore(beads.OrdersStore{Store: s[0]}).LatestOpenRun(scoped)
				return legsRender(fmt.Sprint(run, found), err)
			},
			legs: func(l []beads.FederatedReader) string {
				run, found, err := LatestOpenRunOn(l[0], scoped)
				return legsRender(fmt.Sprint(run, found), err)
			},
		})
		// The API's fresh /orders/check cursor: one front door per store,
		// each store the door's orders and graph leg alike.
		ops = append(ops, legsOp{
			name: "CursorAcross(" + scoped + ")",
			frozen: func(s []beads.Store) string {
				doors := make([]*Store, 0, len(s))
				for _, store := range s {
					doors = append(doors, NewStoreWithGraph(beads.OrdersStore{Store: store}, beads.GraphStore{Store: store}))
				}
				return fmt.Sprint(frozenCursorAcross(doors)(scoped))
			},
			method: func(s []beads.Store) string {
				doors := make([]*Store, 0, len(s))
				for _, store := range s {
					doors = append(doors, NewStoreWithGraph(beads.OrdersStore{Store: store}, beads.GraphStore{Store: store}))
				}
				return fmt.Sprint(CursorAcross(doors)(scoped))
			},
			legs: func(l []beads.FederatedReader) string { return fmt.Sprint(CursorAcrossLegs(l)(scoped)) },
		})
	}
	return ops
}

// legsFixtures are the store lists an op runs over: one store of a shape,
// and for the cursor a federation of a shape beside a higher-seq mem store
// and a failing one.
func legsFixtures() map[string]func(t *testing.T, log *legsLog) []beads.Store {
	out := map[string]func(t *testing.T, log *legsLog) []beads.Store{}
	for name, shape := range legsShapes {
		out[name] = func(t *testing.T, log *legsLog) []beads.Store { return []beads.Store{shape(t, log, "a", 0)} }
		out[name+" + mem + failing"] = func(t *testing.T, log *legsLog) []beads.Store {
			return []beads.Store{shape(t, log, "a", 0), legsShapes["mem"](t, log, "b", 100), legsShapes["failing"](t, log, "c", 200)}
		}
	}
	return out
}

// legsRun runs fn over a fresh fixture and returns its result and its log,
// the logged lines included.
func legsRun(t *testing.T, build func(*testing.T, *legsLog) []beads.Store, fn func([]beads.Store) string) (string, []string) {
	t.Helper()
	log := &legsLog{}
	stores := build(t, log)
	log.take()
	prev := runtimeHelpersLogf
	runtimeHelpersLogf = func(format string, args ...any) {
		log.add("LOG " + fmt.Sprintf(format, args...))
	}
	defer func() { runtimeHelpersLogf = prev }()
	got := fn(stores)
	return got, log.take()
}

// legsStripRole drops each line's role, for comparing a leg read with the
// strict frozen read it replaces; legsRoles is every read's role.
func legsStripRole(lines []string) (stripped, roles []string) {
	for _, l := range lines {
		if i := strings.LastIndex(l, " role="); i >= 0 {
			stripped = append(stripped, l[:i])
			roles = append(roles, l[i+len(" role="):])
			continue
		}
		stripped = append(stripped, l)
	}
	return stripped, roles
}

func TestOrderReadsOverLegsMatchTheFrozenReads(t *testing.T) {
	for fixture, build := range legsFixtures() {
		for _, op := range legsOps() {
			if !strings.HasPrefix(op.name, "Cursor") && strings.Contains(fixture, "+") {
				continue // one-store reads run over one store
			}
			frozen, frozenLog := legsRun(t, build, op.frozen)
			// A warm cache serves an open-only read itself: no backing read,
			// on either side, which the log comparison below holds.
			if len(frozenLog) == 0 && !strings.HasPrefix(fixture, "cache warm") {
				t.Fatalf("%s %s: the frozen read reached no backing store", fixture, op.name)
			}

			method, methodLog := legsRun(t, build, op.method)
			if method != frozen || strings.Join(methodLog, "\n") != strings.Join(frozenLog, "\n") {
				t.Errorf("%s %s: the method differs from main\nfrozen %s\n%s\nmethod %s\n%s", fixture, op.name, frozen, strings.Join(frozenLog, "\n"), method, strings.Join(methodLog, "\n"))
			}

			wantReads, frozenRoles := legsStripRole(frozenLog)
			for _, r := range frozenRoles {
				if r != beads.NoLegRole.String() {
					t.Fatalf("%s %s: a frozen read ran in role %s", fixture, op.name, r)
				}
			}
			for mintName, m := range legsMints {
				live, liveLog := legsRun(t, build, func(stores []beads.Store) string {
					legs := make([]beads.FederatedReader, 0, len(stores))
					for _, s := range stores {
						legs = append(legs, m.mint(s))
					}
					return op.legs(legs)
				})
				gotReads, roles := legsStripRole(liveLog)
				if live != frozen || strings.Join(gotReads, "\n") != strings.Join(wantReads, "\n") {
					t.Errorf("%s %s over %s legs differs from main\nfrozen %s\n%s\nlegs   %s\n%s", fixture, op.name, mintName, frozen, strings.Join(wantReads, "\n"), live, strings.Join(gotReads, "\n"))
				}
				for _, r := range roles {
					if r != m.role.String() {
						t.Errorf("%s %s over %s legs: a read ran in role %s, want %s", fixture, op.name, mintName, r, m.role)
					}
				}
			}
		}
	}
}

// TestOrderReadsOverAZeroLegReadNothing: a zero leg is no read, as a zero
// OrdersStore is.
func TestOrderReadsOverAZeroLegReadNothing(t *testing.T) {
	if runs, err := ListTrackingOn(beads.FederatedReader{}, 5); runs != nil || err != nil {
		t.Errorf("ListTrackingOn(zero) = %v, %v", runs, err)
	}
	if _, found, err := LatestOpenRunOn(beads.FederatedReader{}, "o1"); found || err != nil {
		t.Errorf("LatestOpenRunOn(zero) = %v, %v", found, err)
	}
	if got := CursorAcrossLegs([]beads.FederatedReader{{}})("o1"); got != 0 {
		t.Errorf("CursorAcrossLegs(zero) = %d", got)
	}
}

// TestCursorAcrossLegsKeepsTheBdTraceScope: bdtrace attributes a bd read to
// the order-dispatch scope by a frame naming "orders.CursorAcross"; the leg
// form's reads run under such a frame.
func TestCursorAcrossLegsKeepsTheBdTraceScope(t *testing.T) {
	var frames []string
	spy := &legsFrameStore{Store: beads.NewMemStore(), frames: &frames}
	CursorAcrossLegs([]beads.FederatedReader{beadstest.StrictLeg(spy)})("o1")
	for _, f := range frames {
		if strings.Contains(f, "orders.CursorAcross") {
			return
		}
	}
	t.Errorf("no frame names orders.CursorAcross:\n%s", strings.Join(frames, "\n"))
}

type legsFrameStore struct {
	beads.Store
	frames *[]string
}

func (s *legsFrameStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for {
		f, more := frames.Next()
		*s.frames = append(*s.frames, f.Function)
		if !more {
			break
		}
	}
	return s.Store.List(q)
}
