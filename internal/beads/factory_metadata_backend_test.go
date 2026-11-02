package beads

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// countingProbeChecker returns a preflight checker whose every probe (bd
// context, database identity, database schema) is a counted fake dialer.
func countingProbeChecker(scope, metadata string, calls *int) contract.PreflightChecker {
	files := fsys.NewFake()
	files.Dirs[filepath.Join(scope, ".beads")] = true
	files.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(metadata)
	return contract.PreflightChecker{
		FS:                  files,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			*calls++
			return contract.PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.0.4"}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			*calls++
			return "gc-local", true, nil
		},
		SchemaLatestVersion:        1,
		SchemaLatestIgnoredVersion: 1,
		DatabaseSchemaCursors: func(string) (contract.PreflightSchemaCursors, bool, error) {
			*calls++
			return contract.PreflightSchemaCursors{Main: 1, Ignored: 1, IgnoredChecked: true}, true, nil
		},
	}
}

func writeFactoryScopeMetadata(t *testing.T, metadata string) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	return scope
}

// TestOpenStoreAtForCityUnsupportedMetadataBackendSkipsEveryProbe pins the
// probe-free pre-check: a scope whose metadata names a backend outside the
// native store's set reaches the BdStore fallback without the preflight
// dialing anything. The backend name is arbitrary on purpose — the decision
// reads the native store's supported set, not a list of refused names.
func TestOpenStoreAtForCityUnsupportedMetadataBackendSkipsEveryProbe(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	for _, backend := range []string{"examplekv", "doltlite", "some-remote"} {
		t.Run(backend, func(t *testing.T) {
			metadata := `{"backend":"` + backend + `","dolt_mode":"server","project_id":"gc-local"}`
			scope := writeFactoryScopeMetadata(t, metadata)
			dials := 0
			checker := countingProbeChecker(scope, metadata, &dials)
			// A checker whose metadata read would fail proves preflight was
			// never entered at all, not just that it skipped its probes.
			checker.FS = fsys.NewFake()
			bdOpened := false
			result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:        scope,
				Provider:         "bd",
				PreflightChecker: checker,
				OpenBdStore: func() (Store, error) {
					bdOpened = true
					return NewMemStore(), nil
				},
				OpenNativeStore: func() (Store, error) {
					t.Fatal("OpenNativeStore called for a backend the native store does not serve")
					return nil, nil
				},
			})
			if err != nil {
				t.Fatalf("OpenStoreAtForCity() error = %v", err)
			}
			if dials != 0 {
				t.Fatalf("preflight probes ran %d time(s), want 0", dials)
			}
			if !bdOpened || result.Diagnostic.Store != storeNameBdStore {
				t.Fatalf("store = %q (bd opened %v), want BdStore fallback", result.Diagnostic.Store, bdOpened)
			}
			if result.Diagnostic.PreflightGate != string(contract.PreflightCheckMetadataBackend) {
				t.Fatalf("gate = %q, want %q", result.Diagnostic.PreflightGate, contract.PreflightCheckMetadataBackend)
			}
			if want := contract.UnsupportedMetadataBackendSummary(backend); result.Diagnostic.PreflightReason != want {
				t.Fatalf("reason = %q, want %q", result.Diagnostic.PreflightReason, want)
			}
		})
	}
}

// TestOpenStoreAtForCityDoltMetadataStillRunsPreflightProbes pins that the
// pre-check leaves dolt scopes on the full preflight path.
func TestOpenStoreAtForCityDoltMetadataStillRunsPreflightProbes(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	metadata := factoryPreflightDoltMetadata()
	scope := writeFactoryScopeMetadata(t, metadata)
	dials := 0
	native := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: countingProbeChecker(scope, metadata, &dials),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for a native-eligible dolt scope")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) { return native, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if dials != 3 {
		t.Fatalf("preflight probes ran %d time(s), want 3 (bd context, identity, schema)", dials)
	}
	if result.Store != native || result.Diagnostic.Store != storeNameNativeDoltStore {
		t.Fatalf("store = %q, want native", result.Diagnostic.Store)
	}
}

func TestDecideMetadataBackendTable(t *testing.T) {
	missing := t.TempDir()
	cases := []struct {
		name  string
		scope string
		want  metadataBackendRoute
	}{
		{"no metadata file", missing, metadataBackendRoutePreflight},
		{"no backend named", writeFactoryScopeMetadata(t, `{"project_id":"x"}`), metadataBackendRoutePreflight},
		{"unparseable", writeFactoryScopeMetadata(t, `{`), metadataBackendRoutePreflight},
		{"dolt", writeFactoryScopeMetadata(t, `{"backend":"dolt"}`), metadataBackendRoutePreflight},
		{"other backend", writeFactoryScopeMetadata(t, `{"backend":"examplekv"}`), metadataBackendRouteBdStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideMetadataBackend(tc.scope).Route; got != tc.want {
				t.Fatalf("route = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOpenStoreAtForCityProxiedSpellingsOfDoltStillReachProxiedArm pins the
// ordering between the probe-free metadata pre-check and the persisted
// topology check: the latter accepts contract.IsDoltBackend's wider spellings
// ("bd", any case), so a proxied-server scope spelled that way must still be
// served by the proxied opener, not short-circuited to BdStore.
func TestOpenStoreAtForCityProxiedSpellingsOfDoltStillReachProxiedArm(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	t.Setenv(proxiedNativeEnv, "1")
	for _, backend := range []string{"Dolt", "DOLT", "bd"} {
		t.Run(backend, func(t *testing.T) {
			scope := writeFactoryScopeMetadata(t, `{"backend":"`+backend+`","dolt_mode":"proxied-server"}`)
			proxied := NewMemStore()
			calls := 0
			result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:        scope,
				Provider:         "bd",
				PreflightChecker: refusingPreflightChecker(t),
				OpenBdStore: func() (Store, error) {
					t.Fatal("the bd fallback opened for a proxied scope the proxied opener serves")
					return nil, nil
				},
				OpenNativeStore: func() (Store, error) {
					t.Fatal("the DIRECT native opener ran for a proxied scope")
					return nil, nil
				},
				OpenProxiedStore: func(context.Context, bool) (Store, ProxiedOpenReport, error) {
					calls++
					return proxied, proxiedOpenReportFixture(), nil
				},
			})
			if err != nil {
				t.Fatalf("OpenStoreAtForCity: %v", err)
			}
			if calls != 1 {
				t.Fatalf("the proxied opener ran %d times, want 1", calls)
			}
			if result.Diagnostic.Store != storeNameNativeDoltStore || result.Diagnostic.Proxied == nil {
				t.Fatalf("diagnostic = %+v, want the proxied native store", result.Diagnostic)
			}
		})
	}
}
