package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateJournalFileMatchesPinnedBeads reads the name out of the pinned
// beads module rather than restating it.
//
// The previous spelling of this constant was a plausible-looking guess
// ("migrate-dolt-mode.json"). Nothing failed: the product's post-migration
// verification stat'd a file that could never exist, so it always passed, and so
// did the tests asserting the residue was gone. A constant that is only ever
// compared against itself proves nothing, so this one is compared against bd.
//
// It resolves the module directory from go.mod plus GOMODCACHE rather than
// shelling out to `go list`: a subprocess here would grow the source resource
// census, and the ratchet is shrink-only.
func TestMigrateJournalFileMatchesPinnedBeads(t *testing.T) {
	modCache := strings.TrimSpace(os.Getenv("GOMODCACHE"))
	if modCache == "" {
		t.Skip("GOMODCACHE is unset; cannot locate the pinned beads module")
	}
	version := pinnedBeadsVersion(t)
	source, err := os.ReadFile(filepath.Join(modCache, "github.com/steveyegge/beads@"+version, "cmd", "bd", "migrate_dolt_mode.go"))
	if err != nil {
		t.Skipf("pinned beads %s is not unpacked in the local module cache: %v", version, err)
	}

	const decl = `migrateJournalFileName = "`
	idx := strings.Index(string(source), decl)
	if idx < 0 {
		t.Fatalf("pinned beads %s no longer declares migrateJournalFileName; re-derive %s by hand", version, MigrateDoltModeJournalFile)
	}
	rest := string(source)[idx+len(decl):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("could not parse migrateJournalFileName out of the pinned beads source")
	}
	if got := rest[:end]; got != MigrateDoltModeJournalFile {
		t.Fatalf("MigrateDoltModeJournalFile = %q, pinned bd writes %q", MigrateDoltModeJournalFile, got)
	}
}

// pinnedBeadsVersion reads the beads version this module requires.
func pinnedBeadsVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "github.com/steveyegge/beads" {
			return fields[1]
		}
		if len(fields) >= 3 && fields[0] == "require" && fields[1] == "github.com/steveyegge/beads" {
			return fields[2]
		}
	}
	t.Fatal("go.mod does not require github.com/steveyegge/beads")
	return ""
}

// repositoryRoot walks up from the package directory to the module root.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
