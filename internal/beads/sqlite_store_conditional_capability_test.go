package beads

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// openSQLiteLayoutForTest opens an SQLite store over a schema fixture with or
// without the revision column, optionally read-only.
func openSQLiteLayoutForTest(t *testing.T, revision, readOnly bool) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	createSQLiteSchemaFixture(t, dir, revision, false, nil)
	options := []SQLiteStoreOption{WithSQLiteStoreIDPrefix(sqliteGraphPrefix)}
	if readOnly {
		options = append(options, WithSQLiteStoreReadOnly())
	}
	opened, err := OpenSQLiteStore(dir, options...)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// TestSQLiteStoreProbeConditionalWriteCapabilityFollowsSchema proves the
// prober answers from the open layout, not from the static ConditionalWriter
// assertion: a legacy layout refuses every fenced verb and a read-only open
// cannot write, so both must report incapable.
//
// Based on @sjarmak's prober test in #6717.
func TestSQLiteStoreProbeConditionalWriteCapabilityFollowsSchema(t *testing.T) {
	for _, tc := range []struct {
		name               string
		revision, readOnly bool
		wantCapable        bool
	}{
		{name: "revision column", revision: true, wantCapable: true},
		{name: "legacy layout", revision: false},
		{name: "read-only", revision: true, readOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openSQLiteLayoutForTest(t, tc.revision, tc.readOnly)
			capable, reason := store.probeConditionalWriteCapability()
			if capable != tc.wantCapable {
				t.Fatalf("probeConditionalWriteCapability() = (%t, %q), want capable=%t", capable, reason, tc.wantCapable)
			}
			if !capable && reason == "" {
				t.Fatal("probeConditionalWriteCapability() returned false with no reason")
			}
		})
	}
}

// TestSQLiteStoreCarriesConditionalWritesStamp proves a stamped SQLite engine
// resolves by its stamp. Without the carrier every mode resolved as unset, so
// a split city's fenced session writes landed unconditionally even under
// require.
func TestSQLiteStoreCarriesConditionalWritesStamp(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
		t.Run(string(mode), func(t *testing.T) {
			store := openSQLiteLayoutForTest(t, true, false)
			if err := StampOpenedStore(store, "SQLiteStore", mode, nil, nil); err != nil {
				t.Fatalf("StampOpenedStore: %v", err)
			}
			writer, diag, err := ResolveConditionalWriter(store)
			if err != nil || diag != nil || writer != ConditionalWriter(store) {
				t.Fatalf("ResolveConditionalWriter = (%T, %v, %v), want the stamped store itself", writer, diag, err)
			}
		})
	}
}

// TestCachingStoreOverSQLiteReportsConditionalWritesHonestly pins the
// controller's view of a binding engine: the CachingStore it puts over the
// engine must forward the stamp and answer with the engine's real capability.
// Before the prober, a legacy layout read as vacuously capable through the
// cache, so require handed out a writer whose every verb fails.
func TestCachingStoreOverSQLiteReportsConditionalWritesHonestly(t *testing.T) {
	cachedOver := func(t *testing.T, revision bool, mode gate.Mode) *CachingStore {
		t.Helper()
		engine := openSQLiteLayoutForTest(t, revision, false)
		if err := StampOpenedStore(engine, "SQLiteStore", mode, nil, nil); err != nil {
			t.Fatalf("StampOpenedStore: %v", err)
		}
		cache := NewCachingStoreForTest(engine, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cache
	}

	t.Run("capable engine resolves to the cache", func(t *testing.T) {
		cache := cachedOver(t, true, gate.Require)
		writer, _, err := ResolveConditionalWriter(cache)
		if err != nil || writer != ConditionalWriter(cache) {
			t.Fatalf("ResolveConditionalWriter = (%T, %v), want the cache", writer, err)
		}
	})

	t.Run("legacy engine refuses under require", func(t *testing.T) {
		cache := cachedOver(t, false, gate.Require)
		writer, _, err := ResolveConditionalWriter(cache)
		if writer != nil || !IsConditionalWritesRequired(err) {
			t.Fatalf("ResolveConditionalWriter = (%T, %v), want the typed require refusal", writer, err)
		}
		if !strings.Contains(err.Error(), "revision column") {
			t.Fatalf("refusal %q does not carry the engine's reason", err)
		}
	})

	t.Run("legacy engine degrades under auto", func(t *testing.T) {
		cache := cachedOver(t, false, gate.Auto)
		writer, diag, err := ResolveConditionalWriter(cache)
		if writer != nil || diag == nil || err != nil {
			t.Fatalf("ResolveConditionalWriter = (%T, %v, %v), want a loud degrade to legacy", writer, diag, err)
		}
	})
}

// TestSQLiteStoreInspectionMirrorsTheProber pins the status wire's view of a
// binding engine: the side-effect-free inspector reports what the prober
// would, raw and through the controller's cache, under the store's own kind.
// Without it a legacy layout read as unprobed and capable, so gc status said
// active while every fenced write refused.
func TestSQLiteStoreInspectionMirrorsTheProber(t *testing.T) {
	for _, revision := range []bool{true, false} {
		engine := openSQLiteLayoutForTest(t, revision, false)
		if err := StampOpenedStore(engine, "SQLiteStore", gate.Require, nil, nil); err != nil {
			t.Fatalf("StampOpenedStore: %v", err)
		}
		cache := NewCachingStoreForTest(engine, nil)
		for name, store := range map[string]Store{"raw": engine, "cached": cache} {
			insp := InspectConditionalWrites(store)
			wantProbe := ConditionalWriteProbeCapable
			if !revision {
				wantProbe = ConditionalWriteProbeIncapable
			}
			if insp.Mode != gate.Require || insp.Probe != wantProbe || insp.Capable != revision {
				t.Errorf("revision=%v %s: inspection = %+v, want mode=require probe=%s capable=%v", revision, name, insp, wantProbe, revision)
			}
			if name == "raw" && insp.StoreKind != "SQLiteStore" {
				t.Errorf("raw store kind = %q, want SQLiteStore", insp.StoreKind)
			}
		}
	}
}

// TestResolveOnAClosedSQLiteStoreReportsClosed proves a resolve after the
// binding's routes closed reports the store as gone, not as incapable:
// incapable would fire the once-latched degrade event under auto and turn
// into a ConditionalWritesRequiredError under require, and neither describes
// a store that was shut down.
func TestResolveOnAClosedSQLiteStoreReportsClosed(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Auto, gate.Require} {
		t.Run(string(mode), func(t *testing.T) {
			engine := openSQLiteLayoutForTest(t, true, false)
			degrades := 0
			if err := StampOpenedStore(engine, "SQLiteStore", mode, func(ConditionalWritesDegrade) { degrades++ }, nil); err != nil {
				t.Fatalf("StampOpenedStore: %v", err)
			}
			cache := NewCachingStoreForTest(engine, nil)
			if err := engine.CloseStore(); err != nil {
				t.Fatalf("CloseStore: %v", err)
			}
			for name, store := range map[string]Store{"raw": engine, "cached": cache} {
				writer, diag, err := ResolveConditionalWriter(store)
				if writer != nil || diag != nil || !errors.Is(err, ErrStoreClosed) || IsConditionalWritesRequired(err) {
					t.Errorf("%s: ResolveConditionalWriter = (%T, %v, %v), want (nil, nil, ErrStoreClosed)", name, writer, diag, err)
				}
			}
			if degrades != 0 {
				t.Errorf("a closed store fired %d degrade event(s), want none", degrades)
			}
		})
	}
}
