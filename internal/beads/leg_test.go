package beads

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
)

// roleProbe is a store whose reads record the leg role on their own stack,
// as the split-strict gate's views and strict leaves do (LegRoleOnStack).
type roleProbe struct {
	*MemStore
	seen []LegRole
}

func (p *roleProbe) record() {
	pcs := make([]uintptr, 64)
	p.seen = append(p.seen, LegRoleOnStack(pcs[:runtime.Callers(1, pcs)]))
}

func (p *roleProbe) Get(id string) (Bead, error) { p.record(); return p.MemStore.Get(id) }
func (p *roleProbe) List(q ListQuery) ([]Bead, error) {
	p.record()
	return p.MemStore.List(q)
}

func (p *roleProbe) ListOpen(status ...string) ([]Bead, error) {
	p.record()
	return p.MemStore.ListOpen(status...)
}

func (p *roleProbe) Ready(q ...ReadyQuery) ([]Bead, error) {
	p.record()
	return p.MemStore.Ready(q...)
}

func (p *roleProbe) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	p.record()
	return p.MemStore.Children(parentID, opts...)
}

func (p *roleProbe) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	p.record()
	return p.MemStore.ListByLabel(label, limit, opts...)
}

func (p *roleProbe) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	p.record()
	return p.MemStore.ListByAssignee(assignee, status, limit)
}

func (p *roleProbe) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	p.record()
	return p.MemStore.ListByMetadata(filters, limit, opts...)
}

func (p *roleProbe) DepList(id, direction string) ([]Dep, error) {
	p.record()
	return p.MemStore.DepList(id, direction)
}

func (p *roleProbe) Update(id string, opts UpdateOpts) error {
	p.record()
	return p.MemStore.Update(id, opts)
}

// last is the role the probe's latest read saw.
func (p *roleProbe) last(t *testing.T) LegRole {
	t.Helper()
	if len(p.seen) == 0 {
		t.Fatal("the probe saw no read")
	}
	return p.seen[len(p.seen)-1]
}

// legReads calls every bead read of a Store-shaped leg, by name.
var legReads = map[string]func(Store){
	"Get":            func(s Store) { _, _ = s.Get("x") },
	"List":           func(s Store) { _, _ = s.List(ListQuery{AllowScan: true}) },
	"ListOpen":       func(s Store) { _, _ = s.ListOpen() },
	"Ready":          func(s Store) { _, _ = s.Ready() },
	"Children":       func(s Store) { _, _ = s.Children("x") },
	"ListByLabel":    func(s Store) { _, _ = s.ListByLabel("l", 0) },
	"ListByAssignee": func(s Store) { _, _ = s.ListByAssignee("a", "open", 0) },
	"ListByMetadata": func(s Store) { _, _ = s.ListByMetadata(map[string]string{"k": "v"}, 0) },
	"DepList":        func(s Store) { _, _ = s.DepList("x", "down") },
}

// TestLegRoleOnStackSeesEveryLegRead: every bead read of each leg layer
// carries its role, through the reader and through a resident handle over
// the leg; a write through the leg carries none, and neither does a read
// through no leg.
func TestLegRoleOnStackSeesEveryLegRead(t *testing.T) {
	for want, mint := range map[LegRole]func(Store, string) FederatedReader{
		FederatedLegRole: MintFederatedLeg,
		RelicLegRole:     MintRelicLeg,
	} {
		p := &roleProbe{MemStore: NewMemStore()}
		leg := MintResidentLeg(mint(p, "rig:r"))
		for name, read := range legReads {
			read(leg)
			if got := p.last(t); got != want {
				t.Errorf("%s leg %s: role %v, want %v", want, name, got, want)
			}
		}
		_ = leg.Update("x", UpdateOpts{})
		if got := p.last(t); got != NoLegRole {
			t.Errorf("%s leg Update: role %v, want none: a write carries no role", want, got)
		}
	}
	p := &roleProbe{MemStore: NewMemStore()}
	for name, s := range map[string]Store{"the store": p, "a class handle": WorkStore{p}, "a resident": MintResident(p, "")} {
		for read, call := range legReads {
			call(s)
			if got := p.last(t); got != NoLegRole {
				t.Errorf("%s %s: role %v, want none", name, read, got)
			}
		}
	}
}

// TestLegRoleOnStackShapes: a reader's read called directly, as a method
// value, from a closure, and from a goroutine the caller starts all carry the
// role; nested legs answer the innermost; a read the store hands to another
// goroutine carries none (the doc's calling-goroutine rule).
func TestLegRoleOnStackShapes(t *testing.T) {
	p := &roleProbe{MemStore: NewMemStore()}
	fed, relic := MintFederatedLeg(p, ""), MintRelicLeg(p, "")

	_, _ = fed.Get("x")
	if got := p.last(t); got != FederatedLegRole {
		t.Errorf("direct: %v", got)
	}
	get := relic.Get
	_, _ = get("x")
	if got := p.last(t); got != RelicLegRole {
		t.Errorf("method value of the reader: %v", got)
	}
	legGet := relic.leg.Get
	_, _ = legGet("x")
	if got := p.last(t); got != RelicLegRole {
		t.Errorf("method value of the leg layer: %v", got)
	}
	func() { _, _ = fed.List(ListQuery{AllowScan: true}) }()
	if got := p.last(t); got != FederatedLegRole {
		t.Errorf("closure: %v", got)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = relic.DepList("x", "down") }()
	<-done
	if got := p.last(t); got != RelicLegRole {
		t.Errorf("a goroutine making the read: %v", got)
	}

	_, _ = MintRelicLeg(MintResidentLeg(MintFederatedLeg(p, "")), "").Get("x")
	if got := p.last(t); got != FederatedLegRole {
		t.Errorf("a relic leg over a federated leg: %v, want the innermost (federated)", got)
	}
	_, _ = MintFederatedLeg(MintResidentLeg(MintRelicLeg(p, "")), "").Get("x")
	if got := p.last(t); got != RelicLegRole {
		t.Errorf("a federated leg over a relic leg: %v, want the innermost (relic)", got)
	}

	handOff := &handOffStore{roleProbe: p}
	_, _ = MintFederatedLeg(handOff, "").Get("x")
	if got := p.last(t); got != NoLegRole {
		t.Errorf("a read handed to another goroutine: %v, want none", got)
	}
	if got := LegRoleOnStack(nil); got != NoLegRole {
		t.Errorf("no stack: %v", got)
	}
}

// handOffStore makes its Get on a goroutine of its own.
type handOffStore struct{ *roleProbe }

func (s *handOffStore) Get(id string) (Bead, error) {
	var b Bead
	var err error
	done := make(chan struct{})
	go func() { defer close(done); b, err = s.roleProbe.Get(id) }()
	<-done
	return b, err
}

// TestZeroLegHandles: a zero ResidentStore or FederatedReader is no store,
// answers no capability, and names no ref or role.
func TestZeroLegHandles(t *testing.T) {
	var resident ResidentStore
	if !IsNilStore(resident) {
		t.Error("IsNilStore(zero ResidentStore) = false")
	}
	if _, ok := As[Store](resident); ok {
		t.Error("As[Store](zero ResidentStore) = true")
	}
	if _, ok := As[Counter](resident); ok {
		t.Error("As[Counter](zero ResidentStore) = true")
	}
	var reader FederatedReader
	if !IsNilStore(reader) {
		t.Error("IsNilStore(zero FederatedReader) = false")
	}
	if _, ok := ReaderAs[Counter](reader); ok {
		t.Error("ReaderAs[Counter](zero FederatedReader) = true")
	}
	if reader.Ref() != "" || reader.Role() != NoLegRole {
		t.Errorf("zero reader: ref %q, role %v", reader.Ref(), reader.Role())
	}
	if got := MintResidentLeg(reader); !IsNilStore(got) || got.Ref() != "" {
		t.Errorf("MintResidentLeg(zero reader) = %#v, want a zero handle", got)
	}
	if !IsNilStore(MintFederatedLeg(nil, "")) || !IsNilStore(MintResident(nil, "")) {
		t.Error("a leg or resident over no store is a store")
	}
	if SameStore(reader, resident) {
		t.Error("two zero handles are one store")
	}
}

// TestLegHandlesNameTheirRefAndRole: the mints keep the ref and role they
// were given, and a resident over a leg keeps the leg's ref.
func TestLegHandlesNameTheirRefAndRole(t *testing.T) {
	mem := NewMemStore()
	fed, relic := MintFederatedLeg(mem, "rig:a"), MintRelicLeg(mem, "")
	if fed.Ref() != "rig:a" || fed.Role() != FederatedLegRole || relic.Role() != RelicLegRole {
		t.Fatalf("fed %q %v, relic %v", fed.Ref(), fed.Role(), relic.Role())
	}
	if got := MintResidentLeg(fed).Ref(); got != "rig:a" {
		t.Errorf("resident over a leg: ref %q", got)
	}
	if got := MintResident(mem, "class:gmnos"); got.Ref() != "class:gmnos" || got.Raw() != Store(mem) {
		t.Errorf("resident: ref %q, raw %T", got.Ref(), got.Raw())
	}
	for name, r := range map[string]FederatedReader{"federated": fed, "relic": relic} {
		if !SameStore(r, mem) || !SameStore(r, WorkStore{mem}) || PhysicalStoreKey(r) != PhysicalStoreKey(mem) {
			t.Errorf("%s reader: not its store's identity", name)
		}
		// The reader's live reader is the store's, in an adapter carrying the
		// leg's role (leg_reads.go), so it is not the store's own value.
		if live := LiveFor(r); live == LiveFor(mem) || LegAdapterBase(live) != LiveFor(mem) {
			t.Errorf("%s reader: LiveFor is %#v, want the store's in a role adapter", name, live)
		}
	}
}

// TestReaderAsAnswersOnlyReadCapabilities: a leg reader hands out its
// store's readers, never a writer (the assignee-only writers the class gate
// passes included) or the store.
func TestReaderAsAnswersOnlyReadCapabilities(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	r := MintRelicLeg(WorkStore{cache}, "")
	if c, ok := ReaderAs[Counter](r); !ok || LegAdapterBase(c) != Counter(cache) {
		t.Errorf("ReaderAs[Counter] = %v, %v; want the cache, in a role adapter", c, ok)
	}
	if _, ok := ReaderAs[CachedLister](r); !ok {
		t.Error("ReaderAs[CachedLister] = false")
	}
	if _, ok := ReaderAs[ConditionalWriter](r); ok {
		t.Error("ReaderAs[ConditionalWriter]: a read-only leg handed out a writer")
	}
	if _, ok := As[ConditionalAssignmentReleaser](WorkStore{cache}); !ok {
		t.Fatal("fixture: the cache is no ConditionalAssignmentReleaser")
	}
	if _, ok := ReaderAs[ConditionalAssignmentReleaser](r); ok {
		t.Error("ReaderAs[ConditionalAssignmentReleaser]: a read-only leg handed out an assignee writer")
	}
	if _, ok := ReaderAs[ConditionalAssigneeTransferer](r); ok {
		t.Error("ReaderAs[ConditionalAssigneeTransferer]: a read-only leg handed out an assignee writer")
	}
	if _, ok := ReaderAs[*CachingStore](r); ok {
		t.Error("ReaderAs[*CachingStore]: a read-only leg handed out its store")
	}
	if _, ok := ReaderAs[Store](r); ok {
		t.Error("ReaderAs[Store]: a read-only leg handed out a store")
	}
}

// TestHandlesAndLiveForThroughAResidentReadPastTheCache: HandlesFor and
// LiveFor through a ResidentStore over a CachingStore (bare, or over a leg)
// read the backing store: a row written past the cache is found, a row
// changed past the cache reads fresh, and neither read installs the row or
// announces it. Before Handles() forwarding, HandlesFor's logical live
// reader over the handle served Get from the cache.
func TestHandlesAndLiveForThroughAResidentReadPastTheCache(t *testing.T) {
	for name, mint := range map[string]func(Store) ResidentStore{
		"a resident":                func(s Store) ResidentStore { return MintResident(s, "") },
		"a resident over a relic":   func(s Store) ResidentStore { return MintResidentLeg(MintRelicLeg(s, "")) },
		"a resident over a handle":  func(s Store) ResidentStore { return MintResident(SessionStore{s}, "") },
		"a resident over a fed leg": func(s Store) ResidentStore { return MintResidentLeg(MintFederatedLeg(s, "")) },
	} {
		backing := NewMemStore()
		primed, err := backing.Create(Bead{Title: "primed"})
		if err != nil {
			t.Fatal(err)
		}
		var announced int
		cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) { announced++ })
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
		late, err := backing.Create(Bead{Title: "written past the cache"})
		if err != nil {
			t.Fatal(err)
		}
		retitled := "changed past the cache"
		if err := backing.Update(primed.ID, UpdateOpts{Title: &retitled}); err != nil {
			t.Fatal(err)
		}
		h := mint(cache)
		for reader, live := range map[string]LiveReader{"HandlesFor": HandlesFor(h).Live, "LiveFor": LiveFor(h)} {
			if got, err := live.Get(primed.ID); err != nil || got.Title != retitled {
				t.Errorf("%s, %s Live.Get = %q, %v; want the backing's %q", name, reader, got.Title, err, retitled)
			}
			if _, err := live.Get(late.ID); err != nil {
				t.Errorf("%s, %s Live.Get missed %s, written past the cache: %v", name, reader, late.ID, err)
			}
			rows, err := live.List(ListQuery{AllowScan: true})
			if err != nil || !containsBead(rows, late.ID) {
				t.Errorf("%s, %s Live.List = %v, %v; want %s", name, reader, capBeadIDs(rows), err, late.ID)
			}
		}
		if got := LegAdapterBase(HandlesFor(h).Cached); got != (cachedStoreReader{store: cache}) {
			t.Errorf("%s: HandlesFor.Cached = %#v, want the cache's own reader", name, got)
		}
		if w := HandlesFor(h).Writer; w != Writer(h) {
			t.Errorf("%s: HandlesFor.Writer = %T, want the handle", name, w)
		}
		if _, err := cache.cachedGetOnly(late.ID); err == nil {
			t.Errorf("%s: a live read installed %s into the cache", name, late.ID)
		}
		if cached, err := cache.cachedGetOnly(primed.ID); err != nil || cached.Title != "primed" {
			t.Errorf("%s: the cache's %s = %q, %v; a live read refreshed it", name, primed.ID, cached.Title, err)
		}
		if announced != 0 {
			t.Errorf("%s: live reads announced %d changes, want none", name, announced)
		}
	}
}

// constStore is a store whose Get costs nothing, so a benchmark over it
// measures the layers above.
type constStore struct{ *MemStore }

func (constStore) Get(id string) (Bead, error) { return Bead{ID: id}, nil }

// legGetShapes are the ways a read reaches constStore: the store itself
// through the Store interface (the baseline), a class handle, a leg reader
// and a resident handle over a leg.
func legGetShapes() map[string]func(string) (Bead, error) {
	var raw Store = constStore{}
	var handle Store = WorkStore{raw}
	reader := MintFederatedLeg(raw, "")
	var resident Store = MintResidentLeg(MintRelicLeg(raw, ""))
	return map[string]func(string) (Bead, error){
		"store":    raw.Get,
		"handle":   handle.Get,
		"reader":   reader.Get,
		"resident": resident.Get,
	}
}

// TestLegGetAllocatesNothing: a read through a leg allocates nothing (R11):
// legs are minted once, and a read makes no closure.
func TestLegGetAllocatesNothing(t *testing.T) {
	for name, get := range legGetShapes() {
		if allocs := testing.AllocsPerRun(100, func() { _, _ = get("x") }); allocs != 0 {
			t.Errorf("%s Get: %v allocations per read, want 0", name, allocs)
		}
	}
}

// BenchmarkLegGet compares a Get through a leg reader (one extra indirect
// call: the reader's inlined Get calls the leg layer's, which calls the
// store's) with one through the store itself and through a class handle.
func BenchmarkLegGet(b *testing.B) {
	shapes := legGetShapes()
	for _, name := range []string{"store", "handle", "reader", "resident"} {
		get := shapes[name]
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = get("x")
			}
		})
	}
}

// TestHasExplicitHandlesAsksThePhysicalStore: a class layer forwards
// Handles whatever it wraps, so the question is the store's below the
// layers: a cache or a policy store has explicit handles, a plain store has
// none, through any handle, leg or resident.
func TestHasExplicitHandlesAsksThePhysicalStore(t *testing.T) {
	mem := NewMemStore()
	cache := NewCachingStoreForTest(mem, nil)
	for name, wrap := range handleWraps {
		if HasExplicitHandles(wrap(mem)) {
			t.Errorf("%s over a plain store: explicit handles", name)
		}
		if !HasExplicitHandles(wrap(cache)) {
			t.Errorf("%s over a cache: no explicit handles", name)
		}
	}
	if HasExplicitHandles(MintFederatedLeg(mem, "")) || !HasExplicitHandles(MintRelicLeg(cache, "")) {
		t.Error("a leg reader does not answer its store's")
	}
	if HasExplicitHandles(nil) || HasExplicitHandles(WorkStore{}) {
		t.Error("no store has explicit handles")
	}
}
