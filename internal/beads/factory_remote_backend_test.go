package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	beadsbackend "github.com/steveyegge/beads/backend"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// fakeRemoteBackendName is a backend name registered as REMOTE with the beads
// library registry for the duration of one test. It is deliberately not the
// http backend's name: the factory must route on the registry's Remote bit,
// not on a spelling.
const fakeRemoteBackendName = "gcfakeremote"

// registerFakeRemoteBackend registers fakeRemoteBackendName as a remote
// backend and removes it when the test ends. Callers must not run in
// parallel: the registry is process-global.
func registerFakeRemoteBackend(t *testing.T) {
	t.Helper()
	open := func(context.Context, string) (beadsbackend.DoltStorage, error) {
		return nil, errors.New("fake remote backend opens nothing")
	}
	beadsbackend.Register(fakeRemoteBackendName, beadsbackend.Backend{
		Open:                open,
		OpenReadOnly:        open,
		WorkspaceIsBeadsDir: true,
		Remote:              true,
	})
	t.Cleanup(func() { beadsbackend.Deregister(fakeRemoteBackendName) })
}

// writeScopeMetadata writes .beads/metadata.json for a real scope directory
// (decideMetadataBackend reads the OS filesystem) and returns the scope.
func writeRemoteTestScope(t *testing.T, metadata string) string {
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

// fullRemoteCapabilities is every token of the class requirement table.
func fullRemoteCapabilities() []string {
	var tokens []string
	for _, row := range contract.RemoteCapabilityRequirements() {
		tokens = append(tokens, row.Token)
	}
	return tokens
}

// remotePreflightChecker is a checker for a remote scope whose Dolt readers
// fail the test if anything calls them, and whose handshake answers with the
// given snapshot (or error).
func remotePreflightChecker(t *testing.T, snapshot contract.PreflightWireSnapshot, handshakeErr error) contract.PreflightChecker {
	t.Helper()
	return contract.PreflightChecker{
		Provider: "bd",
		BDContext: func(string) (contract.PreflightBDContext, error) {
			t.Fatal("bd context consulted for a remote scope")
			return contract.PreflightBDContext{}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			t.Fatal("database project id probed for a remote scope")
			return "", false, nil
		},
		DatabaseSchemaCursors: func(string) (contract.PreflightSchemaCursors, bool, error) {
			t.Fatal("schema cursors probed for a remote scope")
			return contract.PreflightSchemaCursors{}, false, nil
		},
		WireHandshake: func(string) (contract.PreflightWireHandshake, error) {
			if handshakeErr != nil {
				return contract.PreflightWireHandshake{}, handshakeErr
			}
			return contract.PreflightWireHandshake{Snapshot: snapshot, TargetProjectID: "gc-remote"}, nil
		},
	}
}

func goodRemoteSnapshot() contract.PreflightWireSnapshot {
	return contract.PreflightWireSnapshot{APIVersion: "v0", BdVersion: "1.3.1", WireRevision: 2, ProjectID: "gc-remote", Capabilities: fullRemoteCapabilities()}
}

func remoteMetadata() string {
	return `{"backend": "` + fakeRemoteBackendName + `", "project_id": "gc-remote"}`
}

// TestDecideMetadataBackendRoutingTable pins the route table: a dolt scope
// takes the preflight, a registered remote backend takes the remote route,
// an unregistered backend goes to BdStore without a probe, and metadata
// naming nothing stays with the preflight that reports it.
func TestDecideMetadataBackendRoutingTable(t *testing.T) {
	registerFakeRemoteBackend(t)
	for _, tc := range []struct {
		name     string
		metadata string
		want     metadataBackendRoute
	}{
		{"dolt", `{"backend": "dolt", "project_id": "p"}`, metadataBackendRoutePreflight},
		{"remote", remoteMetadata(), metadataBackendRouteRemote},
		{"unknown", `{"backend": "examplekv", "project_id": "p"}`, metadataBackendRouteBdStore},
		{"doltlite", `{"backend": "doltlite", "project_id": "p"}`, metadataBackendRouteBdStore},
		{"unnamed", `{"project_id": "p"}`, metadataBackendRoutePreflight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := writeRemoteTestScope(t, tc.metadata)
			got := decideMetadataBackend(scope, "")
			if got.Route != tc.want {
				t.Fatalf("route = %v, want %v", got.Route, tc.want)
			}
			if got.Route == metadataBackendRouteBdStore && got.Gate != string(contract.PreflightCheckMetadataBackend) {
				t.Fatalf("BdStore gate = %q, want metadata_backend", got.Gate)
			}
			if remote := ScopeUsesRemoteBackend(scope); remote != (tc.want == metadataBackendRouteRemote) {
				t.Fatalf("ScopeUsesRemoteBackend = %v", remote)
			}
		})
	}
	// An unregistered name is never remote, whatever it is called: the http
	// backend's own name is remote only once a composition root registered it.
	if !beadsbackend.IsRemote("http") {
		if got := decideMetadataBackend(writeRemoteTestScope(t, `{"backend": "http"}`), "").Route; got != metadataBackendRouteBdStore {
			t.Fatalf("unregistered http backend route = %v, want BdStore", got)
		}
	}
}

func TestDecideNativeTransportTable(t *testing.T) {
	for _, tc := range []struct {
		mode                 NativeTransportMode
		remote               bool
		allow, requireNative bool
	}{
		{NativeTransportOff, true, false, false},
		{NativeTransportOff, false, false, false},
		{NativeTransportAuto, true, true, true},
		// An unthreaded open never requires native for a remote backend: it
		// takes BdStore, the way the control plane and store paths outside any
		// city always have.
		{NativeTransportUnset, true, false, false},
		{NativeTransportAuto, false, true, false},
		{NativeTransportUnset, false, true, false},
	} {
		got := decideNativeTransport(tc.mode, tc.remote)
		if got.AllowNative != tc.allow || got.RequireNative != tc.requireNative {
			t.Errorf("decideNativeTransport(%q, remote=%v) = %+v, want allow=%v require=%v", tc.mode, tc.remote, got, tc.allow, tc.requireNative)
		}
	}
}

// TestOpenStoreAtForCityRemoteAutoOpensNativeWithoutDoltProbes: under auto a
// remote scope opens natively through the remote opener, takes no Dolt probe,
// and touches neither the Dolt-specific native opener nor BdStore.
func TestOpenStoreAtForCityRemoteAutoOpensNativeWithoutDoltProbes(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	registerFakeRemoteBackend(t)
	scope := writeRemoteTestScope(t, remoteMetadata())
	native := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		NativeTransport:  NativeTransportAuto,
		PreflightChecker: remotePreflightChecker(t, goodRemoteSnapshot(), nil),
		OpenBdStore: func() (Store, error) {
			t.Fatal("OpenBdStore called for a remote scope under auto")
			return nil, nil
		},
		OpenNativeStore: func() (Store, error) {
			t.Fatal("the Dolt native opener was used for a remote scope")
			return nil, nil
		},
		OpenRemoteNativeStore: func() (Store, error) { return native, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != native || result.Diagnostic.Store != storeNameNativeDoltStore || !result.Diagnostic.NativeStoreEligible {
		t.Fatalf("result = %+v, want the remote native store", result.Diagnostic)
	}
}

// TestOpenStoreAtForCityRemoteAutoFailuresAreTerminal: every failure on the
// remote route under auto is a terminal HTTPNativeOpenRequiredError naming
// the off escape hatch, and none of them reaches BdStore.
func TestOpenStoreAtForCityRemoteAutoFailuresAreTerminal(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	registerFakeRemoteBackend(t)
	missing := goodRemoteSnapshot()
	missing.Capabilities = nil
	for _, tc := range []struct {
		name         string
		provider     string
		snapshot     contract.PreflightWireSnapshot
		handshakeErr error
		openErr      error
		hooks        bool
		wantGate     string
	}{
		{name: "handshake", snapshot: goodRemoteSnapshot(), handshakeErr: &contract.PreflightWireError{Reason: contract.PreflightWireUnreachable, Err: errors.New("dial tcp: connection refused")}, wantGate: "wire_compat"},
		{name: "capabilities", snapshot: missing, wantGate: "wire_compat"},
		{name: "open", snapshot: goodRemoteSnapshot(), openErr: errors.New("server went away"), wantGate: "native_open"},
		{name: "hooks", snapshot: goodRemoteSnapshot(), hooks: true, wantGate: nativeHooksGate},
		{name: "provider", provider: "custom", snapshot: goodRemoteSnapshot(), wantGate: "provider_contract"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := writeRemoteTestScope(t, remoteMetadata())
			if tc.hooks {
				hook := filepath.Join(scope, ".beads", "hooks", "on_create")
				if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			provider := tc.provider
			if provider == "" {
				provider = "bd"
			}
			_, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:        scope,
				Provider:         provider,
				NativeTransport:  NativeTransportAuto,
				PreflightChecker: remotePreflightChecker(t, tc.snapshot, tc.handshakeErr),
				OpenBdStore: func() (Store, error) {
					t.Fatal("OpenBdStore called: a remote scope must never fall back silently under auto")
					return nil, nil
				},
				OpenExecStore: func() (Store, error) {
					t.Fatal("OpenExecStore called for a remote scope under auto")
					return nil, nil
				},
				OpenRemoteNativeStore: func() (Store, error) {
					if tc.openErr != nil {
						return nil, tc.openErr
					}
					return NewMemStore(), nil
				},
			})
			var required *HTTPNativeOpenRequiredError
			if !errors.As(err, &required) {
				t.Fatalf("error = %v, want *HTTPNativeOpenRequiredError", err)
			}
			if !IsHTTPNativeOpenRequired(err) {
				t.Fatal("IsHTTPNativeOpenRequired = false")
			}
			if required.Gate != tc.wantGate {
				t.Fatalf("gate = %q, want %q (err %v)", required.Gate, tc.wantGate, err)
			}
			if !strings.Contains(err.Error(), `native_transport = "off"`) {
				t.Fatalf("error %q does not name the native_transport=\"off\" escape hatch", err)
			}
		})
	}
}

// TestOpenStoreAtForCityRemoteOffFallsBackToBdStore: off is the rollback
// lever. The bd CLI speaks to the remote backend itself; nothing native is
// attempted and no probe runs. An unthreaded open (unset) lands in the same
// place: only an explicit auto requires native.
func TestOpenStoreAtForCityRemoteOffFallsBackToBdStore(t *testing.T) {
	registerFakeRemoteBackend(t)
	scope := writeRemoteTestScope(t, remoteMetadata())
	for _, tc := range []struct {
		name  string
		mode  NativeTransportMode
		force string
		gate  string
	}{
		{"off", NativeTransportOff, "", nativeTransportOffGate},
		{"force-fallback-alias", NativeTransportAuto, "1", nativeForceFallbackGate},
		{"unset", NativeTransportUnset, "", nativeTransportUnsetGate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(nativeForceFallbackEnv, tc.force)
			bd := NewMemStore()
			result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
				ScopeRoot:        scope,
				Provider:         "bd",
				NativeTransport:  tc.mode,
				PreflightChecker: remotePreflightChecker(t, contract.PreflightWireSnapshot{}, errors.New("handshake must not run under off")),
				OpenBdStore:      func() (Store, error) { return bd, nil },
				OpenRemoteNativeStore: func() (Store, error) {
					t.Fatal("native open attempted under off")
					return nil, nil
				},
			})
			if err != nil {
				t.Fatalf("OpenStoreAtForCity() error = %v", err)
			}
			if result.Store != bd || result.Diagnostic.Store != storeNameBdStore || result.Diagnostic.PreflightGate != tc.gate {
				t.Fatalf("diagnostic = %+v, want BdStore under gate %q", result.Diagnostic, tc.gate)
			}
		})
	}
}

// TestOpenStoreAtForCityUnknownBackendFallsBackWithoutProbes: a backend that
// is neither served locally nor registered remote reaches BdStore from
// metadata alone, and is never mistaken for a remote one.
func TestOpenStoreAtForCityUnknownBackendFallsBackWithoutProbes(t *testing.T) {
	t.Setenv(nativeForceFallbackEnv, "")
	scope := writeRemoteTestScope(t, `{"backend": "examplekv", "project_id": "p"}`)
	checker := remotePreflightChecker(t, contract.PreflightWireSnapshot{}, nil)
	checker.WireHandshake = func(string) (contract.PreflightWireHandshake, error) {
		t.Fatal("wire handshake for an unregistered backend")
		return contract.PreflightWireHandshake{}, nil
	}
	bd := NewMemStore()
	result, err := OpenStoreAtForCity(context.Background(), StoreOpenOptions{
		ScopeRoot:        scope,
		Provider:         "bd",
		PreflightChecker: checker,
		OpenBdStore:      func() (Store, error) { return bd, nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity() error = %v", err)
	}
	if result.Store != bd || result.Diagnostic.PreflightGate != string(contract.PreflightCheckMetadataBackend) {
		t.Fatalf("diagnostic = %+v, want BdStore under metadata_backend", result.Diagnostic)
	}
}
