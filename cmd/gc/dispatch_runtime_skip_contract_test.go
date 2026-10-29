package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestRunControlDispatcherNotOpenSkipContract is the AC-4 pin for issue #6024:
// a control bead whose stored status is not open keeps the one-shot CLI
// contract (nil error, nothing on stdout) while the serve seam reports the
// no-op as errControlNotProcessed, and the seam still closes the scope store
// it opened exactly once.
func TestRunControlDispatcherNotOpenSkipContract(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	newClosedScopeCheck := func(t *testing.T) (*closeCountingStore, string) {
		t.Helper()
		fake := newCloseCountingStore(t, false)
		control, err := fake.Create(beads.Bead{
			Type: "task",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindScopeCheck,
			},
		})
		if err != nil {
			t.Fatalf("seed control bead: %v", err)
		}
		if err := fake.Close(control.ID); err != nil {
			t.Fatalf("close control bead: %v", err)
		}
		return fake, control.ID
	}

	// One-shot path (gc convoy control <id>): a non-open bead prints nothing
	// and exits 0, exactly as before the skip was distinguishable.
	oneShot, oneShotID := newClosedScopeCheck(t)
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, oneShot, oneShotID, nil, &stdout, &stderr); err != nil {
		t.Fatalf("one-shot dispatch of a non-open control: %v (stderr=%q)", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("one-shot dispatch stdout = %q, want empty for a skipped control", stdout.String())
	}

	// Serve seam: the same no-op crosses as the sentinel, and the scope
	// store the seam opened is closed exactly once.
	seam, seamID := newClosedScopeCheck(t)
	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return seam, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })

	var seamStdout, seamStderr bytes.Buffer
	err := runControlDispatcherInStore(cityDir, cityDir, seamID, &seamStdout, &seamStderr, nil)
	if !errors.Is(err, errControlNotProcessed) {
		t.Fatalf("serve seam dispatch of a non-open control: err = %v, want errControlNotProcessed", err)
	}
	if got := seam.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: reporting the skip must not leak the opened store", got)
	}
}
