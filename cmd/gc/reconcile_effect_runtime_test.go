package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
	"github.com/gastownhall/gascity/internal/session"
)

// The effects' fresh runtime read (v5 O1, O2; PLUMBING): its class on a
// cache-aware leaf, through an auto-like composite, and on leaves that
// cannot read fresh.

// recordingLeaf is the simulator's cache-aware provider recording each fresh
// read's process names and since, and running during (when set) inside each.
type recordingLeaf struct {
	*simProvider
	names  [][]string
	sinces []time.Time
	during func()
}

func (l *recordingLeaf) ObserveLivenessSince(name string, pn []string, since time.Time) (runtime.Liveness, error) {
	l.names, l.sinces = append(l.names, pn), append(l.sinces, since)
	if l.during != nil {
		l.during()
	}
	return l.simProvider.ObserveLivenessSince(name, pn, since)
}

// fallThrough is an auto-like composite over leaf: routed there, and its
// fresh read falls through to other when leaf reads nothing running.
type fallThrough struct {
	*recordingLeaf
	other runtime.Provider
}

func (r fallThrough) Backends() []runtime.Backend {
	return []runtime.Backend{{Label: "leaf", Provider: r.recordingLeaf}, {Label: "other", Provider: r.other}}
}

func (r fallThrough) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "leaf", Provider: r.recordingLeaf}, Known: true}
}

func (r fallThrough) ObserveLivenessSince(name string, pn []string, since time.Time) (runtime.Liveness, error) {
	if l, err := r.recordingLeaf.ObserveLivenessSince(name, pn, since); err != nil || l.Running {
		return l, err
	}
	return runtime.ObserveLivenessSince(r.other, name, pn, since)
}

// staleRead is tmux's backend answering its error-bearing read from a cache
// taken before anything started, as tmux's may.
type staleRead struct{ runtime.FullFake }

func (staleRead) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

// staleHybrid is production's hybrid, tmux over k8s, its tmux leg f read
// through a cache: it reports errors but neither reads fresh nor is fresh by
// construction, so only its attested listing proves a name absent.
func staleHybrid(f *runtime.Fake) runtime.Provider {
	k8s, _ := fakeProfile(runtime.ProfileK8s)
	return sessionhybrid.New(runtime.NewFakeProfile(runtime.ProfileTmux, staleRead{runtime.FullFake{Fake: f}}), k8s, func(string) bool { return false })
}

// downLeaf is a fresh leaf whose reads fail and report nothing.
type downLeaf struct{ *simProvider }

func (downLeaf) ObserveLivenessSince(string, []string, time.Time) (runtime.Liveness, error) {
	return runtime.Liveness{}, runtime.ErrRuntimeUnavailable
}

func newSimProvider() *simProvider {
	return &simProvider{Fake: runtime.NewFake(), rts: make(map[string]*simRuntime), changed: make(map[string]uint64), now: func() time.Time { return gatherNow }}
}

// Kills a read through the provider's cache, absence or a corpse concluded
// on the leaf alone, on a hop's failed read (review RT5), or on a backend
// that cannot read fresh without its attested listing (the ruling), or from
// a leaf that cannot read fresh, a failed read taken as
// absence, a bracket that misses a replaced object, and the template's
// process names dropped: the leaf answers its plain read from its last
// listing, and the class is the fresh read's, absent only when every
// fall-through hop agrees.
func TestReadRuntimeClassifiesTheFreshRead(t *testing.T) {
	sp := newSimProvider()
	leaf := &recordingLeaf{simProvider: sp}
	read := func(p runtime.Provider) *txRuntime {
		t.Helper()
		rt, cause := readRuntime(context.Background(), p, []string{"agent"}, "s-1", gatherNow, func() time.Time { return gatherNow })
		if cause != "" {
			t.Fatalf("refused %q", cause)
		}
		return rt
	}
	sp.put("s-1", "gc-1", "1", "tok-1")
	sp.listed()
	sp.drop("s-1")
	if rt := read(leaf); rt.Class != rtAbsent || !slices.Equal(leaf.names[0], []string{"agent"}) {
		t.Fatalf("vanished: %+v, process names %v; want absent, the template's names", rt, leaf.names)
	}
	other := newSimProvider()
	other.put("s-1", "gc-1", "1", "tok-1")
	if rt := read(fallThrough{recordingLeaf: leaf, other: other}); rt.Class != rtUnknown {
		t.Fatalf("alive on the other backend: class %d, want unknown", rt.Class)
	}
	if rt := read(fallThrough{recordingLeaf: leaf, other: newSimProvider()}); rt.Class != rtAbsent {
		t.Fatalf("absent on both backends: class %d, want absent", rt.Class)
	}
	errs := newSimProvider()
	errs.put("s-1", "gc-1", "1", "tok-1")
	errs.rts["s-1"].probeErr = true
	if rt := read(fallThrough{recordingLeaf: leaf, other: errs}); rt.Class != rtUnknown {
		t.Fatalf("the hop's read of the other backend failed: class %d, want unknown", rt.Class)
	}
	if rt := read(fallThrough{recordingLeaf: leaf, other: downLeaf{newSimProvider()}}); rt.Class != rtUnknown {
		t.Fatalf("the hop's read failed, reporting nothing: class %d, want unknown", rt.Class)
	}
	if rt := read(fallThrough{recordingLeaf: leaf, other: staleHybrid(runtime.NewFake())}); rt.Class != rtAbsent {
		t.Fatalf("the other backend cannot read fresh, its attested listing is empty: class %d, want absent", rt.Class)
	}
	unattested := runtime.NewFake()
	unattested.ListingUnattested = true
	if rt := read(fallThrough{recordingLeaf: leaf, other: staleHybrid(unattested)}); rt.Class != rtUnknown {
		t.Fatalf("the other backend cannot read fresh nor attest its listing: class %d, want unknown", rt.Class)
	}
	listed := runtime.NewFake()
	_ = listed.Start(context.Background(), "s-1", runtime.Config{})
	if rt := read(fallThrough{recordingLeaf: leaf, other: staleHybrid(listed)}); rt.Class != rtUnknown {
		t.Fatalf("listed on a backend that cannot read fresh: class %d, want unknown", rt.Class)
	}
	sp.put("s-1", "gc-1", "1", "tok-1")
	sp.rts["s-1"].corpse = true
	if rt := read(fallThrough{recordingLeaf: leaf, other: other}); rt.Class != rtUnknown {
		t.Fatalf("a corpse on the leaf, alive on the other backend: class %d, want unknown", rt.Class)
	}
	if rt := read(fallThrough{recordingLeaf: leaf, other: newSimProvider()}); rt.Class != rtCorpse {
		t.Fatalf("a corpse on the leaf, nothing elsewhere: class %d, want corpse", rt.Class)
	}
	if rt := read(fallThrough{recordingLeaf: leaf, other: staleHybrid(listed)}); rt.Class != rtUnknown {
		t.Fatalf("a corpse on the leaf, listed on a backend that cannot read fresh: class %d, want unknown", rt.Class)
	}
	sp.drop("s-1")
	sp.put("s-1", "gc-1", "2", "tok-2")
	if rt := read(leaf); rt.Class != rtAlive || !rt.Alive() || rt.Identity.Token != "tok-2" || rt.Again.ObjectID != rt.Live.ObjectID {
		t.Fatalf("up: %+v, want alive with its identity, one object", rt)
	}
	leaf.sinces = nil
	leaf.during = func() {
		if len(leaf.sinces) == 2 { // between the brackets: another object under the name
			sp.put("s-1", "gc-1", "2", "tok-2")
		}
	}
	if rt := read(leaf); rt.Same || rt.Alive() {
		t.Fatalf("replaced mid-read: %+v, want not one object", rt)
	}
	leaf.during = nil
	sp.rts["s-1"].zombie = true
	if rt := read(leaf); rt.Class != rtZombie {
		t.Fatalf("agent dead: class %d, want zombie", rt.Class)
	}
	sp.rts["s-1"].corpse = true
	if rt := read(leaf); rt.Class != rtCorpse {
		t.Fatalf("pane dead: class %d, want corpse", rt.Class)
	}
	sp.rts["s-1"].corpse, sp.rts["s-1"].zombie, sp.rts["s-1"].probeErr = false, false, true
	if rt := read(leaf); rt.Class != rtUnknown {
		t.Fatalf("probe errs: class %d, want unknown", rt.Class)
	}
	for _, p := range []runtime.Profile{runtime.ProfileHerdr, runtime.ProfileK8s, runtime.ProfileExec, runtime.ProfileSSH, runtime.ProfileT3Bridge} {
		if l, _ := fakeProfile(p); read(l).Class != rtUnsupported {
			t.Fatalf("profile %d, a leaf without a fresh read: class %d, want unsupported", p, read(l).Class)
		}
	}
	if _, cause := readRuntime(context.Background(), nil, nil, "s-1", gatherNow, time.Now); cause != causeRouteUnknown {
		t.Fatalf("no provider: cause %q, want %q", cause, causeRouteUnknown)
	}
}

// routedComposite is an auto-like composite routing every name to leaf,
// whose own fresh read answers from other: the split-backend shape.
type routedComposite struct {
	*simProvider // other
	leaf         *simProvider
}

func (r routedComposite) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "leaf", Provider: r.leaf}, Known: true}
}

// Kills presence or identity read on the composite (EFFECT-STRUCTURE §4
// must-fix 1; review mutants T11, T12): with the backends disagreeing, a
// runtime on the routed leaf reads with the leaf's object and token, and one
// only on the other backend is neither present nor absent, but unknown.
func TestReadRuntimeReadsPresenceAndIdentityOnTheRoutedLeaf(t *testing.T) {
	for _, leafHas := range []bool{true, false} {
		leaf, other := newSimProvider(), newSimProvider()
		other.put("offset", "x", "1", "x") // object ids differ between backends
		other.put("s-1", "gc-1", "1", "tok-other")
		if leafHas {
			leaf.put("s-1", "gc-1", "1", "tok-leaf")
		}
		rt, _ := readRuntime(context.Background(), routedComposite{simProvider: other, leaf: leaf}, nil, "s-1", gatherNow, time.Now)
		switch {
		case leafHas && (rt.Class != rtAlive || rt.Live.ObjectID != "$1" || rt.Identity.Token != "tok-leaf"):
			t.Fatalf("runtime %+v, want alive with the routed leaf's object ($1) and token", rt)
		case !leafHas && (rt.Class != rtUnknown || rt.Live.Present() || rt.Identity.Token != ""):
			t.Fatalf("runtime %+v, want unknown: absent on the leaf, present through the composite", rt)
		}
	}
}

// flakyProbe is a bool-only backend's (herdr, k8s, exec) whose probe
// failed: IsRunning answers false, as those providers do on an error.
type flakyProbe struct{ runtime.FullFake }

func (flakyProbe) IsRunning(string) bool { return false }

// Kills absence proven by a bool-only backend's failed probe (review pin
// N1, adapted to the ruling): through real auto, a name routed to a fresh
// leaf and gone there reads absent only when the bool-only default's listing
// is attested, error-free and without it; listed there, or with a failing
// listing, it reads unknown.
func TestReadRuntimeAbsenceThroughAutoOverABoolOnlyDefault(t *testing.T) {
	for _, c := range []struct {
		name  string
		base  func() *runtime.Fake
		class runtimeClass
	}{
		{"an attested empty listing", runtime.NewFake, rtAbsent},
		{"alive on the default", func() *runtime.Fake {
			f := runtime.NewFake()
			_ = f.Start(context.Background(), "s-1", runtime.Config{})
			return f
		}, rtUnknown},
		{"a failing listing", runtime.NewFailFake, rtUnknown},
	} {
		a := sessionauto.New(runtime.NewFakeProfile(runtime.ProfileK8s, flakyProbe{runtime.FullFake{Fake: c.base()}}), newSimProvider())
		a.RouteACP("s-1")
		if rt, cause := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); cause != "" || rt.Class != c.class {
			t.Fatalf("%s: class %d (cause %q), want %d", c.name, rt.Class, cause, c.class)
		}
	}
}

// Kills a rekey that proceeds on a read that proves nothing (review K2).
func TestRekeyRefusesAnUnsupportedRead(t *testing.T) {
	if got := rekeyRefusal(&txRuntime{Class: rtUnsupported}, session.Info{}, "tok"); got != causeLivenessUnsupported {
		t.Fatalf("refusal %q, want %q", got, causeLivenessUnsupported)
	}
}

// hangingListing is a bool-only backend's (k8s) whose listing blocks until
// release.
type hangingListing struct {
	runtime.FullFake
	release chan struct{}
}

func (hangingListing) IsRunning(string) bool { return false }

func (h hangingListing) ListRunning(prefix string) ([]string, error) {
	<-h.release
	return h.Fake.ListRunning(prefix)
}

// Kills an unbounded hop listing (review pin N10): a backend whose
// ListRunning hangs ends the read at the effect's context, Unknown, and
// never holds the locks past it.
func TestReadRuntimeBoundsTheHopListing(t *testing.T) {
	h := hangingListing{FullFake: runtime.FullFake{Fake: runtime.NewFake()}, release: make(chan struct{})}
	defer close(h.release)
	a := sessionauto.New(runtime.NewFakeProfile(runtime.ProfileK8s, h), newSimProvider())
	a.RouteACP("s-1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan runtimeClass, 1)
	go func() {
		rt, _ := readRuntime(ctx, a, nil, "s-1", gatherNow, time.Now)
		done <- rt.Class
	}()
	select {
	case c := <-done:
		if c != rtUnknown {
			t.Fatalf("class %d from a listing that never answered, want unknown", c)
		}
	case <-time.After(fenceProbeTimeout):
		t.Fatal("readRuntime still blocked in a hop backend's listing after its context ended")
	}
}

// hybridShaped is a composite that routes name to leaf but reads nothing
// fresh itself: a hop reading it gets its leaf's cached answer.
type hybridShaped struct {
	runtime.Provider
	leaf runtime.Provider
}

func (h hybridShaped) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "local", Provider: h.leaf}, Known: true}
}

// Kills freshness judged on the resolved leaf rather than on the backend the
// hop reads (the conformance re-check): auto over a hybrid default reads
// the hybrid, which answers from its leaf's cached listing, so a runtime
// started since that listing is not proven absent by it. The backend routed
// to is the leaf's, already read fresh.
func TestReadRuntimeJudgesTheBackendTheHopReads(t *testing.T) {
	cached := newSimProvider()
	cached.listed()
	cached.put("s-1", "gc-1", "1", "tok") // up since the last listing: the plain read misses it
	a := sessionauto.New(hybridShaped{Provider: cached, leaf: cached}, newSimProvider())
	a.RouteACP("s-1")
	if rt, cause := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); cause != "" || rt.Class == rtAbsent {
		t.Fatalf("class %d (cause %q) through a hybrid's cached read, want not absent", rt.Class, cause)
	}
	routed := newSimProvider() // the hybrid routed to: its leaf was read fresh, so its listing is not asked
	routed.put("s-1", "gc-1", "1", "tok")
	routed.rts["s-1"].corpse = true
	a = sessionauto.New(hybridShaped{Provider: routed, leaf: routed}, newSimProvider())
	a.SeedRoutes(nil)
	if rt, _ := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); rt.Class != rtCorpse {
		t.Fatalf("a corpse on the routed hybrid's leaf: class %d, want corpse", rt.Class)
	}
}

// Pins the cost of the listing ruling (review pin, inverted): herdr's listing
// skips a bound session whose lookup errors, so it is not provably complete
// and does not attest; an ACP-routed name absent everywhere in a
// herdr-default city reads Unknown, and C5a1's launch refuses
// liveness-unknown until herdr attests.
func TestReadRuntimeAcpAbsenceInAHerdrDefaultCityIsUnknown(t *testing.T) {
	if _, ok := any((*herdr.Provider)(nil)).(runtime.ListingAttestation); ok {
		t.Fatal("herdr attests its listing now: confirm it is complete and flip this pin to absent")
	}
	herdrLeaf, _ := fakeProfile(runtime.ProfileHerdr) // bool-only, and no listing attestation
	a := sessionauto.New(herdrLeaf, newSimProvider())
	a.RouteACP("s-1")
	if rt, cause := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); cause != "" || rt.Class != rtUnknown {
		t.Fatalf("class %d (cause %q), want unknown", rt.Class, cause)
	}
}

// Kills a corpse verdict through real auto that ignores the other backend
// (review pin): a corpse on the routed leaf stands only while the other
// backend reads nothing running without error.
func TestReadRuntimeCorpseThroughAuto(t *testing.T) {
	leaf, other := newSimProvider(), newSimProvider()
	leaf.put("s-1", "gc-1", "1", "tok")
	leaf.rts["s-1"].corpse = true
	a := sessionauto.New(leaf, other)
	a.SeedRoutes(nil) // s-1 routes to the default
	if rt, cause := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); cause != "" || rt.Class != rtCorpse {
		t.Fatalf("corpse on the leaf, nothing elsewhere: class %d, want corpse", rt.Class)
	}
	other.put("s-1", "gc-1", "1", "tok")
	if rt, _ := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); rt.Class != rtUnknown {
		t.Fatalf("corpse on the leaf, alive on the other backend: class %d, want unknown", rt.Class)
	}
	other.rts["s-1"].probeErr = true
	if rt, _ := readRuntime(context.Background(), a, nil, "s-1", gatherNow, time.Now); rt.Class != rtUnknown {
		t.Fatalf("corpse on the leaf, the other backend's read failing: class %d, want unknown", rt.Class)
	}
}

// Kills a blank name read (review item 5), and a corpse on a second read
// after a running first one standing without the hop check (review item
// 6): a blank name refuses route-unknown; a runtime running at the first
// read and a corpse at the second reads a corpse only while the other
// backend runs nothing.
func TestReadRuntimeBlankNameAndALateCorpse(t *testing.T) {
	if _, cause := readRuntime(context.Background(), newSimProvider(), nil, "  ", gatherNow, time.Now); cause != causeRouteUnknown {
		t.Fatalf("blank name: cause %q, want %q", cause, causeRouteUnknown)
	}
	for _, elsewhere := range []bool{false, true} {
		sp := newSimProvider()
		sp.put("s-1", "gc-1", "1", "tok")
		leaf := &recordingLeaf{simProvider: sp}
		leaf.during = func() {
			if len(leaf.sinces) == 2 { // dies between the brackets
				sp.rts["s-1"].corpse = true
			}
		}
		other := newSimProvider()
		if elsewhere {
			other.put("s-1", "gc-1", "1", "tok")
		}
		rt, _ := readRuntime(context.Background(), fallThrough{recordingLeaf: leaf, other: other}, nil, "s-1", gatherNow, time.Now)
		if want := map[bool]runtimeClass{false: rtCorpse, true: rtUnknown}[elsewhere]; rt.Class != want {
			t.Errorf("running elsewhere %t: class %d, want %d", elsewhere, rt.Class, want)
		}
	}
}
