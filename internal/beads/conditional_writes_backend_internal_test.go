package beads

import "testing"

// resolveTargetStore declares inner as its conditional-writes resolve target,
// as the cmd/gc policy store does.
type resolveTargetStore struct {
	Store
	inner Store
}

func (s resolveTargetStore) ConditionalWritesResolveTarget() Store { return s.inner }

// modeSourcedStore declares source as its conditional-writes mode source, as
// the one-shot CLI's emitting class store does.
type modeSourcedStore struct {
	Store
	source Store
}

func (s modeSourcedStore) ConditionalWritesModeSource() Store { return s.source }

// selfTargetStore declares a fresh copy of itself as its resolve target, so
// the walk never reaches a leaf.
type selfTargetStore struct{ Store }

func (s *selfTargetStore) ConditionalWritesResolveTarget() Store {
	return &selfTargetStore{Store: s.Store}
}

func TestResolveConditionalWritesBackendUnwrapsToTheWritingStore(t *testing.T) {
	sqlite, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.(interface{ CloseStore() error }).CloseStore() })
	native := &NativeDoltStore{}
	bd := &BdStore{}

	for name, tc := range map[string]struct {
		store      Store
		kind       string
		sqlite     bool
		doltServed bool
	}{
		"bare SQLite":   {store: sqlite, kind: "SQLiteStore", sqlite: true},
		"cached SQLite": {store: NewCachingStoreForTest(sqlite, nil), kind: "SQLiteStore", sqlite: true},
		"class wrapped, policy wrapped, cached SQLite (a split city's sessions store)": {
			store:  SessionStore{Store: resolveTargetStore{Store: NewMemStore(), inner: NewCachingStoreForTest(sqlite, nil)}},
			kind:   "SQLiteStore",
			sqlite: true,
		},
		"mode-sourced SQLite": {
			store:  modeSourcedStore{Store: NewMemStore(), source: NewCachingStoreForTest(sqlite, nil)},
			kind:   "SQLiteStore",
			sqlite: true,
		},
		"native Dolt":            {store: native, kind: storeNameNativeDoltStore, doltServed: true},
		"cached native Dolt":     {store: SessionStore{Store: NewCachingStoreForTest(native, nil)}, kind: storeNameNativeDoltStore, doltServed: true},
		"bd":                     {store: NewCachingStoreForTest(bd, nil), kind: storeNameBdStore, doltServed: true},
		"proxied (bd writes)":    {store: &ProxiedStore{native: native, bd: bd}, kind: storeNameBdStore, doltServed: true},
		"beads lib (bd)":         {store: &BeadsLibStore{BdStore: bd}, kind: "*beads.BeadsLibStore", doltServed: true},
		"memory":                 {store: NewCachingStoreForTest(NewMemStore(), nil), kind: "MemStore"},
		"a cyclic resolve chain": {store: &selfTargetStore{Store: sqlite}, kind: "*beads.selfTargetStore"},
		"nil":                    {store: nil, kind: "<nil>"},
	} {
		t.Run(name, func(t *testing.T) {
			got := ResolveConditionalWritesBackend(tc.store)
			if got.Kind != tc.kind || got.SQLite != tc.sqlite || got.DoltServed != tc.doltServed {
				t.Fatalf("ResolveConditionalWritesBackend = %+v, want {Kind:%s SQLite:%v DoltServed:%v}",
					got, tc.kind, tc.sqlite, tc.doltServed)
			}
		})
	}
}
