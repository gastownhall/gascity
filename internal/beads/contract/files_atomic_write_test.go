package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// interruptedRenameFS is a durable write that never lands: the data reaches a
// temp file and the rename that would publish it fails. It stands in for the
// SIGKILL/OOM/power loss `gc beads city migrate-proxied` has to survive, and it
// is only observable at all because the writer uses tmp+rename. A writer that
// opens the real path with O_TRUNC has already destroyed the original by the
// time anything can fail, which is the point of this test.
type interruptedRenameFS struct {
	fsys.FS
}

func (interruptedRenameFS) Rename(_, _ string) error {
	return errors.New("simulated interruption before the rename")
}

// gc rewrites three of bd's own files on the migrate path — metadata.json
// twice and config.yaml once — and an interrupted truncate-in-place leaves an
// empty one behind. Every later gc command then refuses the scope
// (`invalid metadata.json: unexpected end of JSON input` through
// LoadMetadataState, and scopeBindingIsProviderOwnedProxied on `gc start`), and
// no gc verb regenerates it: the dolt_database, project_id and dolt_mode are
// gone and the operator hand-writes the file. bd's own sidecar writer and gc's
// ownership journal both use tmp+rename for exactly this reason.
func TestCanonicalWritersLeaveTheOriginalIntactWhenAWriteIsInterrupted(t *testing.T) {
	const metadataSeed = `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq","project_id":"pid-1"}` + "\n"
	const configSeed = "issue_prefix: gc\n"

	for name, tc := range map[string]struct {
		file  string
		seed  string
		write func(fs fsys.FS, path string) error
	}{
		"SetMetadataDoltDataDir": {
			file: "metadata.json",
			seed: metadataSeed,
			write: func(fs fsys.FS, path string) error {
				return SetMetadataDoltDataDir(fs, path, "../../.beads/dolt")
			},
		},
		"EnsureCanonicalMetadata": {
			file: "metadata.json",
			seed: metadataSeed,
			write: func(fs fsys.FS, path string) error {
				_, err := EnsureCanonicalMetadata(fs, path, MetadataState{
					Database:     "dolt",
					Backend:      "dolt",
					DoltMode:     "proxied-server",
					DoltDatabase: "hq",
				})
				return err
			},
		},
		"EnsureCanonicalConfig": {
			file: "config.yaml",
			seed: configSeed,
			write: func(fs fsys.FS, path string) error {
				_, err := EnsureCanonicalConfig(fs, path, ConfigState{
					IssuePrefix:    "gc",
					EndpointOrigin: EndpointOriginManagedCity,
					EndpointStatus: EndpointStatusVerified,
					DoltMode:       "server",
				})
				return err
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			scope := t.TempDir()
			beadsDir := filepath.Join(scope, ".beads")
			if err := os.MkdirAll(beadsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(beadsDir, tc.file)
			if err := os.WriteFile(path, []byte(tc.seed), 0o644); err != nil { //nolint:gosec // fixture
				t.Fatal(err)
			}

			if err := tc.write(interruptedRenameFS{fsys.OSFS{}}, path); err == nil {
				t.Fatalf("an interrupted write reported success")
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the interrupted write removed the original: %v", err)
			}
			if string(after) != tc.seed {
				t.Fatalf("the interrupted write changed the original: %q, want %q", after, tc.seed)
			}
			if tc.file == "metadata.json" {
				var meta map[string]any
				if err := json.Unmarshal(after, &meta); err != nil {
					t.Fatalf("metadata.json no longer parses after an interrupted write: %v", err)
				}
			}
		})
	}
}
