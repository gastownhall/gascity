package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
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
}

func (f *ownerlessFake) ProcessAlive(name string, _ []string) bool { return f.alive[name] }

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
	snapshot  *sessionBeadSnapshot
	sightings *ownerlessSightings
	stderr    bytes.Buffer
}

func newOwnerlessCase(t *testing.T) *ownerlessCase {
	t.Helper()
	c := &ownerlessCase{city: t.TempDir(), sightings: &ownerlessSightings{}}
	c.sp = &ownerlessFake{deadRuntimeArtifactProvider: newDeadRuntimeArtifactProvider(), alive: map[string]bool{}, attached: map[string]bool{}, attachErr: map[string]error{}}
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

// tick runs the closed-bead reap with the ownerless memory at now.
func (c *ownerlessCase) tick(now time.Time) int {
	return reapRuntimesBoundToClosedBeadsWith(c.store, c.snapshot, nil, c.sp, nil, c.city, c.sightings, now, &c.stderr)
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
			t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
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
			t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
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
			t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
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
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
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
	findings, err := ownerlessRuntimeFindings(c.store, c.sp, c.city)
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
	findings, err = ownerlessRuntimeFindings(&ownerGetErrStore{MemStore: beads.NewMemStore(), err: errors.New("store unreachable")}, c.sp, c.city)
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
