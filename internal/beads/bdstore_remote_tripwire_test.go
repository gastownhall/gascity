package beads

import (
	"errors"
	"testing"
)

// rawVerbTripwireRunner fails the test if BdStore ever shells out to a raw
// verb (`bd sql`, `bd blocked`, `dolt sql`) for a remote scope. Every other
// command answers with a benign empty result.
func rawVerbTripwireRunner(t *testing.T) CommandRunner {
	t.Helper()
	return func(_, name string, args ...string) ([]byte, error) {
		if name == "dolt" {
			t.Fatalf("dolt %v run against a remote scope", args)
		}
		if len(args) > 0 && (args[0] == "sql" || args[0] == "blocked") {
			t.Fatalf("bd %v run against a remote scope: the remote backend serves no raw path", args)
		}
		if len(args) > 0 && args[0] == "version" {
			return []byte("bd version 1.3.1 (test)\n"), nil
		}
		return []byte("[]"), nil
	}
}

// TestBdStoreRemoteScopeNeverSpendsRawVerbs is the BdStore half of the
// raw-method tripwire: under native_transport=off a remote scope is served by
// the bd CLI, and neither the ready projection (`bd sql`, then `bd blocked`)
// nor the conditional-release fallback (`bd sql`) may run there.
func TestBdStoreRemoteScopeNeverSpendsRawVerbs(t *testing.T) {
	registerFakeRemoteBackend(t)
	scope := writeRemoteTestScope(t, remoteMetadata())
	store := NewBdStore(scope, rawVerbTripwireRunner(t))

	_, err := store.enrichReadyProjectionForCache([]Bead{{ID: "gr-1", Status: "open", Type: "task"}})
	if !errors.Is(err, ErrReadyProjectionUnsupported) {
		t.Fatalf("ready projection on a remote scope = %v, want ErrReadyProjectionUnsupported", err)
	}
	// A second call is answered by the latch, still without a raw verb.
	if _, err := store.enrichReadyProjectionForCache([]Bead{{ID: "gr-2", Status: "open", Type: "task"}}); !errors.Is(err, ErrReadyProjectionUnsupported) {
		t.Fatalf("second ready projection = %v, want the latched refusal", err)
	}

	store.latchConditionalReleaseUnsupported()
	if _, err := store.ReleaseIfCurrent("gr-1", "worker"); !errors.Is(err, ErrConditionalReleaseRemoteUnsupported) {
		t.Fatalf("ReleaseIfCurrent on a remote scope without the verb = %v, want ErrConditionalReleaseRemoteUnsupported", err)
	}
}

// TestBdStoreRigInheritingRemoteCityNeverSpendsRawVerbs: a rig with no
// metadata of its own under a remote city is served by the city's remote
// backend (RemoteBackendActivationRoot), so its BdStore refuses the same raw
// paths once it knows its city (WithBdStoreCityPath).
func TestBdStoreRigInheritingRemoteCityNeverSpendsRawVerbs(t *testing.T) {
	registerFakeRemoteBackend(t)
	city := writeRemoteTestScope(t, remoteMetadata())
	rig := t.TempDir()
	store := NewBdStore(rig, rawVerbTripwireRunner(t), WithBdStoreCityPath(city))

	if _, err := store.enrichReadyProjectionForCache([]Bead{{ID: "gr-1", Status: "open", Type: "task"}}); !errors.Is(err, ErrReadyProjectionUnsupported) {
		t.Fatalf("ready projection on an inheriting rig = %v, want ErrReadyProjectionUnsupported", err)
	}
	store.latchConditionalReleaseUnsupported()
	if _, err := store.ReleaseIfCurrent("gr-1", "worker"); !errors.Is(err, ErrConditionalReleaseRemoteUnsupported) {
		t.Fatalf("ReleaseIfCurrent on an inheriting rig = %v, want ErrConditionalReleaseRemoteUnsupported", err)
	}
}
