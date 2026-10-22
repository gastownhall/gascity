package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeCommittedDoltDatabase creates <dataDir>/<db>/.dolt/noms/manifest, the
// marker of a database with committed data.
func writeCommittedDoltDatabase(t *testing.T, dataDir, db string) {
	t.Helper()
	writeFakeFile(t, filepath.Join(dataDir, db, ".dolt", "noms", "manifest"), 64)
}

// writePaBackups registers backups by name in the repo_state.json of the
// unbound database pa.
func writePaBackups(t *testing.T, dataDir string, urls map[string]string) {
	t.Helper()
	backups := map[string]map[string]string{}
	for name, url := range urls {
		backups[name] = map[string]string{"name": name, "url": url}
	}
	data, err := json.Marshal(map[string]any{"head": "refs/heads/main", "remotes": map[string]any{}, "backups": backups})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "pa", ".dolt", "repo_state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// populatedBackupURL returns a file:// URL for a non-empty backup directory
// whose last path element is name.
func populatedBackupURL(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	writeFakeFile(t, filepath.Join(dir, "manifest"), 16)
	return "file://" + dir
}

// writeManagedRig binds a rig scope at <cityDir>/<rel> to db on the city's
// managed Dolt server.
func writeManagedRig(t *testing.T, cityDir, rel, db string) string {
	t.Helper()
	rigDir := filepath.Join(cityDir, rel)
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "rg",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, db)
	return rigDir
}

func TestDoltUnboundDatabasesCheck(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares the city's data dir and returns the rigs to bind.
		setup      func(t *testing.T, cityDir, dataDir string) []config.Rig
		wantStatus CheckStatus
		wantNamed  []string
		notNamed   []string
		wantText   string
	}{
		{
			name: "unbound database without a backup warns",
			setup: func(_ *testing.T, _, _ string) []config.Rig {
				return nil
			},
			wantStatus: StatusWarning,
			wantNamed:  []string{"pa"},
			notNamed:   []string{"hq"},
		},
		{
			name: "legacy db-backup restores it",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				writePaBackups(t, dataDir, map[string]string{"pa-backup": populatedBackupURL(t, "anything")})
				return nil
			},
			wantStatus: StatusOK,
		},
		{
			name: "bd default backup naming the database restores it",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				writePaBackups(t, dataDir, map[string]string{"default": populatedBackupURL(t, "pa")})
				return nil
			},
			wantStatus: StatusOK,
		},
		{
			name: "non-file backup is not a local restore source",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				writePaBackups(t, dataDir, map[string]string{"default": "aws://[table:bucket]/pa"})
				return nil
			},
			wantStatus: StatusWarning,
			wantNamed:  []string{"pa"},
		},
		{
			name: "empty backup dir is not a restore source",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				empty := filepath.Join(t.TempDir(), "pa")
				if err := os.MkdirAll(empty, 0o755); err != nil {
					t.Fatal(err)
				}
				writePaBackups(t, dataDir, map[string]string{"pa-backup": "file://" + empty})
				return nil
			},
			wantStatus: StatusWarning,
			wantNamed:  []string{"pa"},
		},
		{
			name: "default backup of another database is not a restore source",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				writePaBackups(t, dataDir, map[string]string{"default": populatedBackupURL(t, "other")})
				return nil
			},
			wantStatus: StatusWarning,
			wantNamed:  []string{"pa"},
		},
		{
			name: "phantom, renamed, hidden and probe directories are not databases",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				writePaBackups(t, dataDir, map[string]string{"pa-backup": populatedBackupURL(t, "pa")})
				if err := os.MkdirAll(filepath.Join(dataDir, "ghost", ".dolt"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeCommittedDoltDatabase(t, dataDir, "pa.replaced-20261022T010203Z")
				writeCommittedDoltDatabase(t, dataDir, ".quarantine")
				writeCommittedDoltDatabase(t, dataDir, "__gc_probe")
				return nil
			},
			wantStatus: StatusOK,
			notNamed:   []string{"ghost", "replaced", "quarantine", "__gc_probe"},
		},
		{
			name: "rig bound to a hyphenated name owns the underscored store",
			setup: func(t *testing.T, cityDir, dataDir string) []config.Rig {
				writePaBackups(t, dataDir, map[string]string{"pa-backup": populatedBackupURL(t, "pa")})
				writeCommittedDoltDatabase(t, dataDir, "my_rig")
				return []config.Rig{{Name: "my-rig", Path: writeManagedRig(t, cityDir, "my-rig", "my-rig")}}
			},
			wantStatus: StatusOK,
			notNamed:   []string{"my_rig"},
		},
		{
			name: "suspended rig still binds its database",
			setup: func(t *testing.T, cityDir, dataDir string) []config.Rig {
				writeCommittedDoltDatabase(t, dataDir, "fe")
				return []config.Rig{{Name: "fe", Path: writeManagedRig(t, cityDir, "fe", "fe"), SuspendedOnStart: true}}
			},
			wantStatus: StatusWarning,
			wantNamed:  []string{"pa"},
			notNamed:   []string{"fe"},
		},
		{
			name: "missing data dir is OK",
			setup: func(t *testing.T, _, dataDir string) []config.Rig {
				if err := os.RemoveAll(dataDir); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			wantStatus: StatusOK,
		},
		{
			name: "unresolved scope config skips instead of naming its database",
			setup: func(t *testing.T, cityDir, dataDir string) []config.Rig {
				writeCommittedDoltDatabase(t, dataDir, "fe")
				rigDir := writeManagedRig(t, cityDir, "fe", "fe")
				cfgPath := filepath.Join(rigDir, ".beads", "config.yaml")
				data, err := os.ReadFile(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				bogus := strings.Replace(string(data), "gc.endpoint_origin: inherited_city", "gc.endpoint_origin: bogus", 1)
				if bogus == string(data) {
					t.Fatalf("config.yaml has no inherited_city origin to replace:\n%s", data)
				}
				if err := os.WriteFile(cfgPath, []byte(bogus), 0o644); err != nil {
					t.Fatal(err)
				}
				return []config.Rig{{Name: "fe", Path: rigDir}}
			},
			wantStatus: StatusOK,
			notNamed:   []string{"fe", "pa"},
			wantText:   "skipped",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := setupManagedDoltCity(t)
			dataDir := filepath.Join(cityDir, ".beads", "dolt")
			writeCommittedDoltDatabase(t, dataDir, "hq")
			writeCommittedDoltDatabase(t, dataDir, "pa")
			rigs := tc.setup(t, cityDir, dataDir)

			r := NewDoltUnboundDatabasesCheckForConfig(cityDir, &config.City{Rigs: rigs}, nil).Run(&CheckContext{})
			if r.Status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; msg = %s", r.Status, tc.wantStatus, r.Message)
			}
			// The data dir path is a temp dir whose name could contain a
			// database name; check names in the rest of the message.
			names := strings.ReplaceAll(r.Message, dataDir, "<data_dir>")
			for _, db := range tc.wantNamed {
				if !strings.Contains(names, db) {
					t.Errorf("message = %q, want it to name %q", r.Message, db)
				}
			}
			for _, db := range tc.notNamed {
				if strings.Contains(names, db) {
					t.Errorf("message = %q, must not name %q", r.Message, db)
				}
			}
			if !strings.Contains(r.Message, tc.wantText) {
				t.Errorf("message = %q, want %q", r.Message, tc.wantText)
			}
		})
	}
}
