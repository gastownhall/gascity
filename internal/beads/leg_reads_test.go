package beads

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// capProbe is a roleProbe that also answers the exact batch read, the
// cache-first list and the count, each recording the role on its stack.
type capProbe struct {
	*roleProbe
	mu sync.Mutex
}

func newCapProbe() *capProbe { return &capProbe{roleProbe: &roleProbe{MemStore: NewMemStore()}} }

// record is roleProbe.record made safe for GetMany's goroutines.
func (p *capProbe) record() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roleProbe.record()
}

func (p *capProbe) Get(id string) (Bead, error) { p.record(); return p.MemStore.Get(id) }

func (p *capProbe) GetExactBatch(ids []string) (map[string]Bead, []string, error) {
	p.record()
	return map[string]Bead{}, ids, nil
}

func (p *capProbe) CachedList(ListQuery) ([]Bead, bool) { p.record(); return nil, true }

func (p *capProbe) Count(context.Context, ListQuery, ...string) (int, error) {
	p.record()
	return 0, nil
}

// roles returns every role the probe saw.
func (p *capProbe) roles() []LegRole {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]LegRole(nil), p.seen...)
}

// noBatchStore is a capProbe seen only through the Store interface, so it
// has no exact batch read and GetMany fans out one Get per id.
type noBatchStore struct {
	Store
	p *capProbe
}

func (s noBatchStore) Get(id string) (Bead, error) { return s.p.Get(id) }

// legShapes are the ways a caller holds a leg over a store: the reader, and
// a resident handle over the leg.
func legShapes(mint func(Store, string) FederatedReader) map[string]func(Store) StoreReader {
	return map[string]func(Store) StoreReader{
		"reader":   func(s Store) StoreReader { return mint(s, "rig:r") },
		"resident": func(s Store) StoreReader { return MintResidentLeg(mint(s, "rig:r")) },
	}
}

// belowLegReads are the reads a caller makes below a leg, by name: through
// LiveFor, through HandlesFor's two readers, GetMany both ways, and the two
// capability adapters As and ReaderAs hand out.
func belowLegReads(t *testing.T) map[string]func(StoreReader) {
	read := func(r legSideReader) {
		_, _ = r.Get("x")
		_, _ = r.List(ListQuery{AllowScan: true})
		_, _ = r.Ready()
		_, _ = r.DepList("x", "down")
	}
	handlesOf := func(s StoreReader) StoreHandles {
		switch h := s.(type) {
		case FederatedReader:
			return HandlesFor(h.leg)
		case Store:
			return HandlesFor(h)
		}
		t.Fatalf("no handles for %T", s)
		return StoreHandles{}
	}
	return map[string]func(StoreReader){
		"LiveFor":           func(s StoreReader) { read(LiveFor(s)) },
		"LiveFor batch":     func(s StoreReader) { _, _, _ = LiveFor(s).(ExactBatchGetter).GetExactBatch([]string{"x"}) },
		"HandlesFor.Live":   func(s StoreReader) { read(handlesOf(s).Live) },
		"HandlesFor.Cached": func(s StoreReader) { read(handlesOf(s).Cached) },
		"GetMany":           func(s StoreReader) { _, _ = GetMany(s, []string{"a", "b", "c"}) },
		"CachedLister": func(s StoreReader) {
			c, ok := asFromReader[CachedLister](s)
			if !ok {
				t.Fatalf("%T: no CachedLister", s)
			}
			_, _ = c.CachedList(ListQuery{AllowScan: true})
		},
		"Counter": func(s StoreReader) {
			c, ok := asFromReader[Counter](s)
			if !ok {
				t.Fatalf("%T: no Counter", s)
			}
			_, _ = c.Count(context.Background(), ListQuery{AllowScan: true})
		},
	}
}

// asFromReader is ReaderAs for a reader and As for a store.
func asFromReader[T any](s StoreReader) (T, bool) {
	if r, ok := s.(FederatedReader); ok {
		return ReaderAs[T](r)
	}
	return As[T](s.(Store))
}

// TestReadsBelowALegCarryItsRole: every read a caller makes below a leg
// (LiveFor, HandlesFor's readers, GetMany's exact batch and its per-id
// fan-out goroutines, a cache-first list and a count) carries the leg's role,
// through the reader and through a resident over the leg.
func TestReadsBelowALegCarryItsRole(t *testing.T) {
	for want, mint := range map[LegRole]func(Store, string) FederatedReader{
		FederatedLegRole: MintFederatedLeg,
		RelicLegRole:     MintRelicLeg,
	} {
		for shape, over := range legShapes(mint) {
			for name, read := range belowLegReads(t) {
				p := newCapProbe()
				read(over(p))
				seen := p.roles()
				if len(seen) == 0 {
					t.Fatalf("%v %s %s: the probe saw no read", want, shape, name)
				}
				for _, got := range seen {
					if got != want {
						t.Errorf("%v %s %s: a read carried %v, want %v (all: %v)", want, shape, name, got, want, seen)
						break
					}
				}
			}
			p := newCapProbe()
			_, _ = GetMany(over(noBatchStore{Store: p.MemStore, p: p}), []string{"a", "b", "c", "d"})
			seen := p.roles()
			if len(seen) != 4 {
				t.Fatalf("%v %s GetMany fan-out: %d reads, want one per id", want, shape, len(seen))
			}
			for _, got := range seen {
				if got != want {
					t.Errorf("%v %s GetMany fan-out goroutine: role %v, want %v", want, shape, got, want)
				}
			}
		}
	}
}

// TestReadsBelowNoLegCarryNone: the same reads through the store, a class
// handle, a resident over the store and a strict leg carry no role.
func TestReadsBelowNoLegCarryNone(t *testing.T) {
	shapes := map[string]func(Store) StoreReader{
		"the store":       func(s Store) StoreReader { return s },
		"a class handle":  func(s Store) StoreReader { return WorkStore{s} },
		"a resident":      func(s Store) StoreReader { return MintResident(s, "") },
		"a strict leg":    func(s Store) StoreReader { return MintStrictLeg(s, "") },
		"a strict leg rs": func(s Store) StoreReader { return MintResidentLeg(MintStrictLeg(s, "")) },
	}
	for shape, over := range shapes {
		for name, read := range belowLegReads(t) {
			p := newCapProbe()
			read(over(p))
			seen := p.roles()
			for _, got := range seen {
				if got != NoLegRole {
					t.Errorf("%s %s: role %v, want none", shape, name, got)
					break
				}
			}
		}
	}
}

// TestReadsBelowNestedLegsCarryTheInnermost: below nested legs the innermost
// leg's role wins, as it does for the leg layers' own frames.
func TestReadsBelowNestedLegsCarryTheInnermost(t *testing.T) {
	for want, s := range map[LegRole]func(Store) StoreReader{
		FederatedLegRole: func(s Store) StoreReader { return MintRelicLeg(MintResidentLeg(MintFederatedLeg(s, "")), "") },
		RelicLegRole:     func(s Store) StoreReader { return MintFederatedLeg(MintResidentLeg(MintRelicLeg(s, "")), "") },
	} {
		for name, read := range belowLegReads(t) {
			p := newCapProbe()
			read(s(p))
			seen := p.roles()
			for _, got := range seen {
				if got != want {
					t.Errorf("nested, innermost %v, %s: role %v", want, name, got)
					break
				}
			}
		}
	}
}

// TestLegAdaptersAnswerAsTheStore: an adapter reads exactly what the store
// does, and a store without an exact batch read answers GetExactBatch with
// ErrExactBatchGetUnsupported, so GetMany falls back to one Get per id.
func TestLegAdaptersAnswerAsTheStore(t *testing.T) {
	mem := NewMemStore()
	b, err := mem.Create(Bead{Title: "row"})
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]FederatedReader{"federated": MintFederatedLeg(mem, ""), "relic": MintRelicLeg(mem, "")} {
		live := LiveFor(r)
		if got, err := live.Get(b.ID); err != nil || got.ID != b.ID {
			t.Errorf("%s LiveFor.Get = %v, %v", name, got.ID, err)
		}
		if _, _, err := live.(ExactBatchGetter).GetExactBatch([]string{b.ID}); !errors.Is(err, ErrExactBatchGetUnsupported) {
			t.Errorf("%s GetExactBatch over a MemStore: %v, want ErrExactBatchGetUnsupported", name, err)
		}
		got, err := GetMany(r, []string{b.ID, "missing"})
		if err != nil || len(got) != 1 || got[b.ID].ID != b.ID {
			t.Errorf("%s GetMany = %v, %v", name, got, err)
		}
	}
	// A cached reader has no exact batch read of its own: below a leg it
	// answers ErrExactBatchGetUnsupported, so a caller falls back to Gets.
	cached := HandlesFor(MintRelicLeg(NewCachingStoreForTest(NewMemStore(), nil), "").leg).Cached
	if getter, ok := cached.(ExactBatchGetter); !ok {
		t.Errorf("the cached reader below a leg is %T, not a role adapter", cached)
	} else if _, _, err := getter.GetExactBatch([]string{"x"}); !errors.Is(err, ErrExactBatchGetUnsupported) {
		t.Errorf("GetExactBatch on the cached reader below a leg: %v, want ErrExactBatchGetUnsupported", err)
	}
	if LegAdapterBase(mem) != Store(mem) {
		t.Error("LegAdapterBase of a non-adapter is not itself")
	}
}
