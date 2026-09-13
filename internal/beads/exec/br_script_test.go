package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestGcBeadsBrReadyIncludeEphemeralFailsLoudlyUntilSupported(t *testing.T) {
	s := NewStore(findGcBeadsBrScript(t))

	_, err := s.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err == nil {
		t.Fatal("Ready(TierBoth) succeeded, want gc-beads-br to reject --include-ephemeral until br can support it")
	}
	if !strings.Contains(err.Error(), "ready: --include-ephemeral is not supported by gc-beads-br") {
		t.Fatalf("Ready(TierBoth) error = %q, want clear --include-ephemeral unsupported message", err)
	}
}

func findGcBeadsBrScript(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			scriptPath := filepath.Join(dir, "contrib", "beads-scripts", "gc-beads-br")
			if _, err := os.Stat(scriptPath); err != nil {
				t.Fatalf("gc-beads-br not found at %s: %v", scriptPath, err)
			}
			return scriptPath
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (no go.mod)")
		}
		dir = parent
	}
}
