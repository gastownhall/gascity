package acceptancehelpers

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/schema"
)

// The version bd reports is the one it ended at. "already at v66" and
// "v65 -> v66" both mean v66, which is why the highest number wins rather than
// the first.
func TestParseBdSchemaVersionTakesTheVersionBdEndedAt(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  string
		want int
	}{
		{"already at", "✓ Schema already at v66\n", 66},
		{"migrated to", "Applied 1 migration\n✓ Schema migrated to v66\n", 66},
		{"range", "migrating v65 -> v66\n", 66},
		{"with warnings above", "warning: no beads configuration found in /tmp/x\n✓ Schema already at v66\n", 66},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseBdSchemaVersion(tt.out)
			if !ok {
				t.Fatalf("parseBdSchemaVersion(%q) found no version", tt.out)
			}
			if got != tt.want {
				t.Errorf("parseBdSchemaVersion(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}

// Reporting no version is not the same as reporting v0: the caller has to tell
// "bd said something I cannot parse" from "bd is at v0", because the first is a
// broken probe and the second would be a real skew.
func TestParseBdSchemaVersionReportsWhenThereIsNoVersion(t *testing.T) {
	for _, out := range []string{"", "Error: database is locked\n", "done\n"} {
		if got, ok := parseBdSchemaVersion(out); ok {
			t.Errorf("parseBdSchemaVersion(%q) = %d, true; want no version", out, got)
		}
	}
}

// stubBd writes a bd that reports the schema version it is told to, so the
// guard can be driven in both directions without two real beads builds.
func stubBd(t *testing.T, version int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "bd")
	body := "#!/bin/sh\necho '✓ Schema already at v" + strconv.Itoa(version) + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	return path
}

// A bd one migration behind the linked library is the case that cost this
// suite a full triage cycle: it surfaces on the external shapes as a gc store
// fallback or as bd locked out of its own database, never as a version error.
// The guard has to name both numbers and both binaries.
func TestRequireBdSchemaParityRejectsABdBehindTheLibrary(t *testing.T) {
	behind := schema.LatestVersion() - 1
	err := RequireBdSchemaParity(stubBd(t, behind))
	if err == nil {
		t.Fatalf("a bd at v%d passed parity against a library at v%d", behind, schema.LatestVersion())
	}
	for _, want := range []string{
		"v" + strconv.Itoa(behind),
		"v" + strconv.Itoa(schema.LatestVersion()),
		"behind",
		"GC_ACCEPTANCE_BD_BIN",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// The other direction is not benign either: a bd ahead of the library writes a
// schema gc's native open cannot read, so parity is required both ways rather
// than "bd must be at least as new".
func TestRequireBdSchemaParityRejectsABdAheadOfTheLibrary(t *testing.T) {
	ahead := schema.LatestVersion() + 1
	err := RequireBdSchemaParity(stubBd(t, ahead))
	if err == nil {
		t.Fatalf("a bd at v%d passed parity against a library at v%d", ahead, schema.LatestVersion())
	}
	if !strings.Contains(err.Error(), "ahead of") {
		t.Errorf("error does not say the binary is ahead: %v", err)
	}
}

// And a matched pair is silent — the guard must not become a tax on every
// acceptance run that already has the right bd.
func TestRequireBdSchemaParityAcceptsAMatchedPair(t *testing.T) {
	if err := RequireBdSchemaParity(stubBd(t, schema.LatestVersion())); err != nil {
		t.Fatalf("a matched bd failed parity: %v", err)
	}
}

// TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig pins a
// synthetic HOME whose .beads/config.yaml declares dolt.shared-server: true
// — the shape ga-1037rg named as the gate host's ambient config — and proves
// the probe still migrates its own throwaway --db cleanly. Before HOME was
// pinned in the probe's subprocess env, it inherited this polluted ambient
// HOME unfiltered: bd resolved the migration against whatever database the
// shared-server config named instead of the probe's own throwaway file,
// surfacing that database's real state instead of a clean migration
// (ga-wapfnm).
func TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig(t *testing.T) {
	bdPath := RequireBD(t)

	pollutedHome := t.TempDir()
	beadsDir := filepath.Join(pollutedHome, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("creating polluted HOME .beads dir: %v", err)
	}
	cfg := "no-db: true\ndolt:\n    shared-server: true\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing polluted HOME config.yaml: %v", err)
	}
	t.Setenv("HOME", pollutedHome)

	if _, err := bdLatestSchemaVersion(bdPath); err != nil {
		t.Fatalf("bdLatestSchemaVersion under a shared-server HOME: %v", err)
	}
}
