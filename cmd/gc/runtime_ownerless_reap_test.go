package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ownerlessFake is the dead-runtime fake with an agent's process liveness
// and a client's attachment, each settable per name.
type ownerlessFake struct {
	*deadRuntimeArtifactProvider
	alive     map[string]bool
	attached  map[string]bool
	attachErr map[string]error
	// onAttachRead, when set, runs at each attachment read with its count,
	// the last read the classification makes before the reap takes the lease.
	onAttachRead func(n int)
	attachReads  int
	// created is each runtime's creation time (none: unknown), freshErrs
	// each fresh liveness read's error.
	created   map[string]time.Time
	freshErrs map[string]error
}

func (f *ownerlessFake) ProcessAlive(name string, _ []string) bool { return f.alive[name] }

// ObserveLivenessSince is the fresh read: a corpse, a live pane with the
// agent alive by its names (none reads alive), or nothing, with its
// creation time.
func (f *ownerlessFake) ObserveLivenessSince(name string, names []string, _ time.Time) (runtime.Liveness, error) {
	if err := f.freshErrs[name]; err != nil {
		return runtime.Liveness{}, err
	}
	var l runtime.Liveness
	if !f.visible[name] {
		return l, nil
	}
	if c, ok := f.created[name]; ok {
		l.ObjectCreated = strconv.FormatInt(c.Unix(), 10)
	}
	switch {
	case f.dead[name]:
		l.Corpse = true
	case f.live[name]:
		l.Running, l.Alive = true, len(names) == 0 || f.alive[name]
	}
	return l, nil
}

func (f *ownerlessFake) IsAttachedWithError(name string) (bool, error) {
	f.attachReads++
	if f.onAttachRead != nil {
		f.onAttachRead(f.attachReads)
	}
	if err := f.attachErr[name]; err != nil {
		return false, err
	}
	return f.attached[name], nil
}

// ownerGetErrStore answers every Get with err.
type ownerGetErrStore struct {
	*beads.MemStore
	err error
}

func (s *ownerGetErrStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// ownerAppearsStore holds session bead gc-gone once written is set.
type ownerAppearsStore struct {
	*beads.MemStore
	written bool
}

func (s *ownerAppearsStore) Get(id string) (beads.Bead, error) {
	if id == "gc-gone" && s.written {
		return beads.Bead{ID: id, Status: "open", Type: "session", Labels: []string{"gc:session"}}, nil
	}
	return s.MemStore.Get(id)
}

// ownerlessCase is a city with one runtime, "named-1", bound to session bead
// gc-gone that the store does not hold; the open named row gc-named owns the
// name now. The runtime is this city's and a corpse unless a case says else.
type ownerlessCase struct {
	city      string
	sp        *ownerlessFake
	store     beads.Store
	others    []beads.Store // the other stores a session row can live in
	snapshot  *sessionBeadSnapshot
	sightings *ownerlessSightings
	gap       time.Duration
	rec       *events.Fake
	stderr    bytes.Buffer
}

func newOwnerlessCase(t *testing.T) *ownerlessCase {
	t.Helper()
	c := &ownerlessCase{city: t.TempDir(), sightings: &ownerlessSightings{}, gap: ownerlessReapConfirmAfter, rec: events.NewFake()}
	c.sp = &ownerlessFake{
		deadRuntimeArtifactProvider: newDeadRuntimeArtifactProvider(), alive: map[string]bool{}, attached: map[string]bool{}, attachErr: map[string]error{},
		created: map[string]time.Time{"named-1": ownerlessT0.Add(-time.Hour)}, freshErrs: map[string]error{},
	}
	c.sp.visible["named-1"] = true
	c.sp.dead["named-1"] = true
	for k, v := range map[string]string{"GC_SESSION_ID": "gc-gone", "GC_CITY_PATH": c.city} {
		if err := c.sp.SetMeta("named-1", k, v); err != nil {
			t.Fatal(err)
		}
	}
	named := beads.Bead{ID: "gc-named", Status: "open", Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"session_name": "named-1"}}
	c.store = beads.NewMemStoreFrom(1, []beads.Bead{named}, nil)
	c.snapshot = newSessionBeadSnapshot([]beads.Bead{named})
	return c
}

// ownerlessT0 is the cases' first tick.
var ownerlessT0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// owners is the case's owner lookup: its sessions store, then the others.
func (c *ownerlessCase) owners() ownerLookup {
	return ownerLookup{stores: append([]beads.Store{c.store}, c.others...)}
}

// tick runs the closed-bead reap with the ownerless memory at now, a
// minute's age grace.
func (c *ownerlessCase) tick(now time.Time) int {
	o := &ownerlessReap{sightings: c.sightings, owners: c.owners(), rec: c.rec, gap: c.gap, grace: time.Minute, now: now, clock: func() time.Time { return now }}
	return reapRuntimesBoundToClosedBeadsWith(c.store, c.snapshot, nil, c.sp, nil, c.city, o, &c.stderr)
}

// TestOwnerlessDeadRuntimeIsReapedOnTheSecondSighting (CONTRACT v5.10j,
// mc-gjc95): a dead runtime of this city whose owner row the store does not
// hold is not reaped on its first sighting, nor on a second one too soon
// after, and is on one ownerlessReapConfirmAfter later; a zombie (a pane up,
// none of its published processes alive) is too.
func TestOwnerlessDeadRuntimeIsReapedOnTheSecondSighting(t *testing.T) {
	for _, shape := range []string{"corpse", "zombie"} {
		t.Run(shape, func(t *testing.T) {
			c := newOwnerlessCase(t)
			if shape == "zombie" {
				delete(c.sp.dead, "named-1")
				c.sp.live["named-1"] = true
				if err := c.sp.SetMeta("named-1", "GT_PROCESS_NAMES", "claude"); err != nil {
					t.Fatal(err)
				}
			}
			t0 := ownerlessT0
			if n := c.tick(t0); n != 0 || c.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("first sighting: reaped %d, stops %d; want none", n, c.sp.stopCalls["named-1"])
			}
			if n := c.tick(t0.Add(10 * time.Second)); n != 0 || c.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("a sighting too soon: reaped %d; want none", n)
			}
			if n := c.tick(t0.Add(ownerlessReapConfirmAfter)); n != 1 || c.sp.stopCalls["named-1"] != 1 {
				t.Fatalf("confirmed sighting: reaped %d, stops %d; want it reaped; stderr=%q", n, c.sp.stopCalls["named-1"], c.stderr.String())
			}
		})
	}
}

// TestOwnerlessRuntimeLeftRunning (CONTRACT v5.10j): a runtime whose owner
// the store does not hold is never stopped when it may be alive (reported
// once), when a client is attached or attachment cannot be read, when it is
// another city's or its city cannot be read (doctor finding only), when the
// store's Get fails with anything but not-found, or while another holder has
// the name's runtime lease.
func TestOwnerlessRuntimeLeftRunning(t *testing.T) {
	for _, c := range []struct {
		name   string
		set    func(t *testing.T, c *ownerlessCase) func()
		report string
	}{
		{"alive", func(t *testing.T, c *ownerlessCase) func() {
			delete(c.sp.dead, "named-1")
			c.sp.live["named-1"] = true
			c.sp.alive["named-1"] = true
			if err := c.sp.SetMeta("named-1", "GT_PROCESS_NAMES", "claude"); err != nil {
				t.Fatal(err)
			}
			return nil
		}, "left running (alive)"},
		{"running with no published process names", func(_ *testing.T, c *ownerlessCase) func() {
			delete(c.sp.dead, "named-1")
			c.sp.live["named-1"] = true
			return nil
		}, "left running (alive)"},
		{"attached", func(_ *testing.T, c *ownerlessCase) func() { c.sp.attached["named-1"] = true; return nil }, "left running (attached)"},
		{"attachment unreadable", func(_ *testing.T, c *ownerlessCase) func() {
			c.sp.attachErr["named-1"] = runtime.ErrRuntimeUnavailable
			return nil
		}, "left running (attached)"},
		{"another city", func(t *testing.T, c *ownerlessCase) func() {
			if err := c.sp.SetMeta("named-1", "GC_CITY_PATH", "/elsewhere/city"); err != nil {
				t.Fatal(err)
			}
			return nil
		}, ""},
		{"city unreadable", func(t *testing.T, c *ownerlessCase) func() {
			if err := c.sp.SetMeta("named-1", "GC_CITY_PATH", ""); err != nil {
				t.Fatal(err)
			}
			return nil
		}, ""},
		{"a store error other than not found", func(_ *testing.T, c *ownerlessCase) func() {
			c.store = &ownerGetErrStore{MemStore: beads.NewMemStore(), err: errors.New("store unreachable")}
			return nil
		}, ""},
		{"the name's lease held", func(t *testing.T, c *ownerlessCase) func() {
			_, release, err := tryRuntimeLease(nil, c.city, "named-1", "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			return release
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			oc := newOwnerlessCase(t)
			if release := c.set(t, oc); release != nil {
				defer release()
			}
			t0 := ownerlessT0
			for i := range 3 {
				if n := oc.tick(t0.Add(time.Duration(i) * ownerlessReapConfirmAfter)); n != 0 {
					t.Fatalf("tick %d reaped %d", i, n)
				}
			}
			if oc.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("stopped %d time(s); want it left running; stderr=%q", oc.sp.stopCalls["named-1"], oc.stderr.String())
			}
			if c.report != "" && strings.Count(oc.stderr.String(), c.report) != 1 {
				t.Fatalf("stderr %q, want %q reported once", oc.stderr.String(), c.report)
			}
		})
	}
}

// TestOwnerlessReapRereadsUnderTheLease (CONTRACT v5.10j): what the reap
// read before it took the name's lease it reads again under it, so a runtime
// that a start rebound to a new owner, or whose owner the store now holds,
// in between is left running.
func TestOwnerlessReapRereadsUnderTheLease(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, c *ownerlessCase){
		"rebound to a new owner": func(t *testing.T, c *ownerlessCase) {
			if err := c.sp.SetMeta("named-1", "GC_SESSION_ID", "gc-new"); err != nil {
				t.Fatal(err)
			}
		},
		"its owner written": func(_ *testing.T, c *ownerlessCase) { c.store.(*ownerAppearsStore).written = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := newOwnerlessCase(t)
			c.store = &ownerAppearsStore{MemStore: c.store.(*beads.MemStore)}
			c.sp.onAttachRead = func(n int) {
				if n == 2 { // the confirming sighting's classification
					change(t, c)
				}
			}
			t0 := ownerlessT0
			c.tick(t0)
			if n := c.tick(t0.Add(ownerlessReapConfirmAfter)); n != 0 || c.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("reaped %d, stops %d; want it left running; stderr=%q", n, c.sp.stopCalls["named-1"], c.stderr.String())
			}
		})
	}
}

// TestOwnerlessSightingForgottenWhenNotSeen (CONTRACT v5.10j): a pass that
// does not sight the pair forgets it, so the next sighting is a first one
// again; a restart (fresh memory) does the same.
func TestOwnerlessSightingForgottenWhenNotSeen(t *testing.T) {
	c := newOwnerlessCase(t)
	t0 := ownerlessT0
	c.tick(t0)
	delete(c.sp.visible, "named-1")
	c.tick(t0.Add(ownerlessReapConfirmAfter))
	c.sp.visible["named-1"] = true
	if n := c.tick(t0.Add(2 * ownerlessReapConfirmAfter)); n != 0 {
		t.Fatalf("reaped %d on the first sighting after the gap; want none", n)
	}
	c.sightings = &ownerlessSightings{} // a controller restart
	if n := c.tick(t0.Add(3 * ownerlessReapConfirmAfter)); n != 0 {
		t.Fatalf("reaped %d on the first sighting after a restart; want none", n)
	}
}

// TestOwnerlessRuntimeFindings (CONTRACT v5.10j): the doctor finding names
// each runtime no row owns, with why: no GC_SESSION_ID, an owner the store
// does not hold (and whether the tick reaps it, or why not: another city's,
// its city unreadable); a runtime whose row the store holds is not named,
// nor one whose owner a failing store read cannot rule out.
func TestOwnerlessRuntimeFindings(t *testing.T) {
	c := newOwnerlessCase(t)
	c.sp.visible["anon"] = true
	c.sp.visible["owned"] = true
	if err := c.sp.SetMeta("owned", "GC_SESSION_ID", "gc-named"); err != nil {
		t.Fatal(err)
	}
	c.sp.visible["foreign"] = true
	c.sp.dead["foreign"] = true
	for k, v := range map[string]string{"GC_SESSION_ID": "gc-other", "GC_CITY_PATH": "/elsewhere"} {
		if err := c.sp.SetMeta("foreign", k, v); err != nil {
			t.Fatal(err)
		}
	}
	c.sp.visible["nocity"] = true
	c.sp.dead["nocity"] = true
	if err := c.sp.SetMeta("nocity", "GC_SESSION_ID", "gc-third"); err != nil {
		t.Fatal(err)
	}
	findings, err := ownerlessRuntimeFindings(c.owners(), c.sp, c.city, ownerlessT0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(findings, "\n")
	for _, want := range []string{
		"anon: no GC_SESSION_ID",
		"foreign: session bead gc-other not found (city-foreign)",
		"named-1: session bead gc-gone not found (dead: the controller reaps it)",
		"nocity: session bead gc-third not found (city-unreadable)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "owned") || len(findings) != 4 {
		t.Errorf("findings %q, want the four ownerless runtimes only", got)
	}
	// A store that cannot answer proves no owner gone: only the runtime
	// without GC_SESSION_ID is named.
	findings, err = ownerlessRuntimeFindings(ownerLookup{stores: []beads.Store{&ownerGetErrStore{MemStore: beads.NewMemStore(), err: errors.New("store unreachable")}}}, c.sp, c.city, ownerlessT0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(findings, "\n"); len(findings) != 1 || !strings.Contains(got, "anon: no GC_SESSION_ID") {
		t.Errorf("findings over an unreadable store %q, want the anon runtime only", got)
	}
}

// TestOwnerlessRuntimesDoctorCheck (CONTRACT v5.10j): gc doctor's
// ownerless-runtimes check warns with the findings, and is OK when every
// runtime has an owning row.
func TestOwnerlessRuntimesDoctorCheck(t *testing.T) {
	c := newOwnerlessCase(t)
	check := newOwnerlessRuntimesCheck(nil, c.city, func(string) (beads.Store, error) { return c.store, nil }, c.sp, nil)
	res := check.Run(nil)
	if res.Status != doctor.StatusWarning || len(res.Details) != 1 || !strings.Contains(res.Details[0], "named-1: session bead gc-gone not found") {
		t.Fatalf("result %+v, want a warning naming named-1", res)
	}
	delete(c.sp.visible, "named-1")
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("result %+v, want OK with no ownerless runtime", res)
	}
}

// TestOwnerlessReapReadsEveryOwnerStore (the #7811 review, CONTRACT
// v5.10j item 2): the owner is missing only when every store a session row
// can live in answers not found. A row in another store (a rig's, a moved
// sessions binding's) keeps its runtime, and so does a store that errors.
func TestOwnerlessReapReadsEveryOwnerStore(t *testing.T) {
	for name, other := range map[string]beads.Store{
		"the owner in another store": beads.NewMemStoreFrom(1, []beads.Bead{{ID: "gc-gone", Status: "open", Type: "session", Labels: []string{"gc:session"}}}, nil),
		"another store errors":       &ownerGetErrStore{MemStore: beads.NewMemStore(), err: errors.New("rig store unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			c := newOwnerlessCase(t)
			c.others = []beads.Store{beads.NewMemStore(), other}
			for i := range 3 {
				c.tick(ownerlessT0.Add(time.Duration(i) * ownerlessReapConfirmAfter))
			}
			if c.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("stopped %d time(s); want it left running", c.sp.stopCalls["named-1"])
			}
			if findings, _ := ownerlessRuntimeFindings(c.owners(), c.sp, c.city, ownerlessT0, time.Minute); strings.Contains(strings.Join(findings, "\n"), "named-1") {
				t.Fatalf("findings %q name a runtime whose owner is not proven missing", findings)
			}
		})
	}
	t.Run("an alive runtime whose owner another store holds is not reported", func(t *testing.T) {
		c := newOwnerlessCase(t)
		delete(c.sp.dead, "named-1")
		c.sp.live["named-1"] = true
		c.others = []beads.Store{beads.NewMemStoreFrom(1, []beads.Bead{{ID: "gc-gone", Status: "open", Type: "session", Labels: []string{"gc:session"}}}, nil)}
		c.tick(ownerlessT0)
		if strings.Contains(c.stderr.String(), "left running") {
			t.Fatalf("stderr %q reports a runtime whose owner is held", c.stderr.String())
		}
	})
	t.Run("every store answers not found", func(t *testing.T) {
		c := newOwnerlessCase(t)
		c.others = []beads.Store{beads.NewMemStore(), beads.NewMemStore()}
		c.tick(ownerlessT0)
		if n := c.tick(ownerlessT0.Add(ownerlessReapConfirmAfter)); n != 1 {
			t.Fatalf("reaped %d, want the runtime reaped", n)
		}
	})
	if (ownerLookup{err: errors.New("rig store unopenable"), stores: []beads.Store{beads.NewMemStore()}}).notFound("gc-gone") || (ownerLookup{}).notFound("gc-gone") {
		t.Fatal("an incomplete or empty lookup proved an owner missing")
	}
}

// TestOwnerlessDoctorCheckNeedsEveryStore (the #7811 review): a rig store
// the doctor cannot open leaves it unable to prove any owner missing, and
// the check says so instead of naming a runtime.
func TestOwnerlessDoctorCheckNeedsEveryStore(t *testing.T) {
	c := newOwnerlessCase(t)
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}, Rigs: []config.Rig{{Name: "r", Path: "/rigs/r"}}}
	open := func(path string) (beads.Store, error) {
		if path == "/rigs/r" {
			return nil, errors.New("rig store locked")
		}
		return c.store, nil
	}
	res := newOwnerlessRuntimesCheck(cfg, c.city, open, c.sp, nil).Run(nil)
	if res.Status != doctor.StatusWarning || !strings.Contains(res.Message, "rig store locked") || len(res.Details) != 0 {
		t.Fatalf("result %+v, want a warning that the rig store would not open, naming no runtime", res)
	}
}

// TestOwnerlessReapWaitsAPatrolAndTheAgeGrace (CONTRACT v5.10j items 2):
// the two sightings are at least one patrol apart (the gap), and a runtime
// younger than the age grace, or whose age cannot be read, is not reaped.
func TestOwnerlessReapWaitsAPatrolAndTheAgeGrace(t *testing.T) {
	t.Run("a patrol longer than the confirm window", func(t *testing.T) {
		c := newOwnerlessCase(t)
		c.gap = 2 * time.Minute
		c.tick(ownerlessT0)
		if n := c.tick(ownerlessT0.Add(ownerlessReapConfirmAfter)); n != 0 {
			t.Fatalf("reaped %d inside the patrol gap", n)
		}
		if n := c.tick(ownerlessT0.Add(c.gap)); n != 1 {
			t.Fatalf("reaped %d a patrol later, want it reaped", n)
		}
	})
	for name, created := range map[string]time.Time{"young": ownerlessT0.Add(ownerlessReapConfirmAfter), "age unknown": {}} {
		t.Run(name, func(t *testing.T) {
			c := newOwnerlessCase(t)
			if created.IsZero() {
				delete(c.sp.created, "named-1")
			} else {
				c.sp.created["named-1"] = created
			}
			for i := range 3 {
				c.tick(ownerlessT0.Add(time.Duration(i) * ownerlessReapConfirmAfter))
			}
			if c.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("stopped %d time(s); want a %s runtime left running", c.sp.stopCalls["named-1"], name)
			}
		})
	}
}

// TestOwnerlessReapRecordsAnEvent (CONTRACT v5.10j item 5): each reap
// records a session.stopped event naming the runtime and its missing owner.
func TestOwnerlessReapRecordsAnEvent(t *testing.T) {
	c := newOwnerlessCase(t)
	c.tick(ownerlessT0)
	c.tick(ownerlessT0.Add(ownerlessReapConfirmAfter))
	if len(c.rec.Events) != 1 || c.rec.Events[0].Type != events.SessionStopped || c.rec.Events[0].Subject != "named-1" || c.rec.Events[0].SessionID != "gc-gone" {
		t.Fatalf("events %+v, want one session.stopped for named-1 (gc-gone)", c.rec.Events)
	}
}

// TestOwnerlessLivenessErrorLeavesTheRuntime (the #7811 review): a fresh
// liveness read, or the process-names read, that errors on an alive
// ownerless runtime never reads it dead: it is left running, and the
// doctor names the read's failure.
func TestOwnerlessLivenessErrorLeavesTheRuntime(t *testing.T) {
	for _, which := range []string{"an alive runtime", "a process-names read"} {
		t.Run(which, func(t *testing.T) {
			c := newOwnerlessCase(t)
			delete(c.sp.dead, "named-1")
			c.sp.live["named-1"], c.sp.alive["named-1"] = true, true
			if err := c.sp.SetMeta("named-1", "GT_PROCESS_NAMES", "claude"); err != nil {
				t.Fatal(err)
			}
			if which == "an alive runtime" {
				c.sp.freshErrs["named-1"] = errors.New("tmux: state refresh failed")
			} else {
				c.sp.GetMetaErrors = map[string]map[string]error{"named-1": {"GT_PROCESS_NAMES": errors.New("tmux: server busy")}}
			}
			for i := range 3 {
				c.tick(ownerlessT0.Add(time.Duration(i) * ownerlessReapConfirmAfter))
			}
			if c.sp.stopCalls["named-1"] != 0 {
				t.Fatalf("stopped %d time(s); want the alive runtime left running", c.sp.stopCalls["named-1"])
			}
			if findings, _ := ownerlessRuntimeFindings(c.owners(), c.sp, c.city, ownerlessT0, time.Minute); !strings.Contains(strings.Join(findings, "\n"), "(liveness-error)") {
				t.Fatalf("findings %q, want the liveness error named", findings)
			}
		})
	}
}

// TestOwnerlessLookupPlannedOnDemand: the tick plans its owner lookup on
// the first ownerless runtime a pass meets, once, and a pass with none
// plans nothing.
func TestOwnerlessLookupPlannedOnDemand(t *testing.T) {
	c := newOwnerlessCase(t)
	plans := 0
	tick := func(now time.Time) int {
		o := &ownerlessReap{
			sightings: c.sightings, rec: c.rec, gap: c.gap, grace: time.Minute, now: now,
			lookup: func() ownerLookup { plans++; return c.owners() },
		}
		return reapRuntimesBoundToClosedBeadsWith(c.store, c.snapshot, nil, c.sp, nil, c.city, o, &c.stderr)
	}
	tick(ownerlessT0)
	if n := tick(ownerlessT0.Add(ownerlessReapConfirmAfter)); n != 1 || plans != 2 {
		t.Fatalf("reaped %d with %d plans; want it reaped, one plan a pass", n, plans)
	}
	tick(ownerlessT0.Add(2 * ownerlessReapConfirmAfter))
	if plans != 2 {
		t.Fatalf("%d plans after a pass with no ownerless runtime, want none added", plans)
	}
}

// ownerlessRigCity is a city with one rig, "r", its patrol and startup
// timeout set, whose rig store holds session row gc-rig.
func ownerlessRigCity(t *testing.T) (city string, cfg *config.City, sessions beads.Store, rigs map[string]beads.Store) {
	t.Helper()
	city = t.TempDir()
	cfg = &config.City{Workspace: config.Workspace{Name: "c"}, Rigs: []config.Rig{{Name: "r", Path: t.TempDir()}}}
	cfg.Daemon.PatrolInterval = "2m"
	cfg.Session.StartupTimeout = "3m"
	sessions = beads.NewMemStore()
	rigs = map[string]beads.Store{"r": beads.NewMemStoreFrom(1, []beads.Bead{{ID: "gc-rig", Status: "open", Type: "session", Labels: []string{"gc:session"}}}, nil)}
	return city, cfg, sessions, rigs
}

// TestNewOwnerlessReapIsTheTicks (the #7811 review r2): the tick's reap
// looks the owner up in the rig stores too (a row in a rig is never
// missing), waits a patrol between sightings, keeps the city's
// startup_timeout as the age grace, and records its reaps.
func TestNewOwnerlessReapIsTheTicks(t *testing.T) {
	city, cfg, sessions, rigs := ownerlessRigCity(t)
	rec := events.NewFake()
	o := newOwnerlessReap(city, cfg, sessions, func() map[string]beads.Store { return rigs }, &ownerlessSightings{}, rec, ownerlessT0)
	if o.gap != 2*time.Minute || o.grace != 3*time.Minute || o.rec != events.Recorder(rec) || o.clock == nil {
		t.Fatalf("reap gap %v grace %v rec %v clock set %t; want the patrol, the startup timeout, the recorder and a clock", o.gap, o.grace, o.rec, o.clock != nil)
	}
	owners := o.lookup()
	if owners.err != nil || owners.notFound("gc-rig") || !owners.notFound("gc-nowhere") {
		t.Fatalf("lookup %+v: gc-rig missing %t, gc-nowhere missing %t; want the rig's row found and an absent one missing", owners, owners.notFound("gc-rig"), owners.notFound("gc-nowhere"))
	}
}

// TestOwnerlessLookupNeedsEveryRigStore (the #7811 review r2): a configured
// rig whose store is not open fails the lookup, so no owner is proven
// missing without it.
func TestOwnerlessLookupNeedsEveryRigStore(t *testing.T) {
	city, cfg, sessions, _ := ownerlessRigCity(t)
	owners := sessionOwnerLookup(city, cfg, sessions, map[string]beads.Store{})
	if owners.err == nil || owners.notFound("gc-nowhere") {
		t.Fatalf("lookup %+v, want it failed and proving nothing", owners)
	}
}

// TestOwnerlessFinalCheckReadsNow (the #7811 review r2): the check under
// the lease reads as of its own time, not the tick's: a runtime the tick
// saw past the age grace that the check's clock reads young is left.
func TestOwnerlessFinalCheckReadsNow(t *testing.T) {
	c := newOwnerlessCase(t)
	c.sp.created["named-1"] = ownerlessT0.Add(-2 * time.Minute)
	c.tick(ownerlessT0)
	at := ownerlessT0.Add(ownerlessReapConfirmAfter)
	o := &ownerlessReap{
		sightings: c.sightings, owners: c.owners(), rec: c.rec, gap: c.gap, grace: 2 * time.Minute, now: at,
		clock: func() time.Time { return ownerlessT0.Add(-time.Minute) },
	} // the check's clock reads the runtime a minute old
	if n := reapRuntimesBoundToClosedBeadsWith(c.store, c.snapshot, nil, c.sp, nil, c.city, o, &c.stderr); n != 0 {
		t.Fatalf("reaped %d, want the final check's own clock to hold it", n)
	}
}
