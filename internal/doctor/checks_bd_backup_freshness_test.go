package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// testScopeDoltDatabase is the Dolt database the freshness tests' scope names in
// metadata.json, and therefore the directory its managed backup lands in.
const testScopeDoltDatabase = "alpha_db"

func TestBulkDeleteSafe(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	maxAge := 24 * time.Hour

	t.Run("all scopes fresh → safe", func(t *testing.T) {
		scope1 := t.TempDir()
		scope2 := t.TempDir()
		writeBackupStateForFreshness(t, scope1, now.Add(-1*time.Hour).Format(time.RFC3339))
		writeBackupStateForFreshness(t, scope2, now.Add(-2*time.Hour).Format(time.RFC3339))
		cfg := &config.City{Rigs: []config.Rig{
			{Path: scope1},
			{Path: scope2},
		}}
		safe, reason := BulkDeleteSafe(scope1, cfg, maxAge, now)
		if !safe {
			t.Fatalf("all fresh: want safe=true, got safe=false, reason=%q", reason)
		}
		if reason != "" {
			t.Fatalf("all fresh: want empty reason, got %q", reason)
		}
	})

	t.Run("one stale scope → unsafe, reason contains scope label", func(t *testing.T) {
		fresh := t.TempDir()
		stale := t.TempDir()
		writeBackupStateForFreshness(t, fresh, now.Add(-1*time.Hour).Format(time.RFC3339))
		writeBackupStateForFreshness(t, stale, now.Add(-48*time.Hour).Format(time.RFC3339))
		cfg := &config.City{Rigs: []config.Rig{
			{Path: fresh},
			{Path: stale},
		}}
		safe, reason := BulkDeleteSafe(fresh, cfg, maxAge, now)
		if safe {
			t.Fatalf("stale scope: want safe=false, got safe=true")
		}
		if !strings.Contains(reason, stale) {
			t.Fatalf("stale scope: reason should name the stale scope %q, got %q", stale, reason)
		}
	})

	t.Run("no backup_state.json in any scope → safe (unconfigured is not this check's job)", func(t *testing.T) {
		scope1 := t.TempDir()
		scope2 := t.TempDir()
		cfg := &config.City{Rigs: []config.Rig{
			{Path: scope1},
			{Path: scope2},
		}}
		safe, reason := BulkDeleteSafe(scope1, cfg, maxAge, now)
		if !safe {
			t.Fatalf("no backup config: want safe=true, got safe=false, reason=%q", reason)
		}
		if reason != "" {
			t.Fatalf("no backup config: want empty reason, got %q", reason)
		}
	})

	t.Run("migrated scope with a never-synced dolt backup → unsafe", func(t *testing.T) {
		scope := t.TempDir()
		writeDoltBackupRegistration(t, scope) // no dolt-backup-state.json
		cfg := &config.City{Rigs: []config.Rig{{Path: scope}}}
		safe, reason := BulkDeleteSafe(scope, cfg, maxAge, now)
		if safe {
			t.Fatalf("never-synced dolt backup: want safe=false, got safe=true")
		}
		if !strings.Contains(reason, "never synced") {
			t.Fatalf("reason should say the backup never synced, got %q", reason)
		}
	})

	// With no config in hand the gate must discover scopes from disk. Narrowing
	// to the city root would leave the rig unscanned and return safe=true —
	// failing this gate OPEN on a destructive operation.
	t.Run("nil config still scans rigs discovered on disk", func(t *testing.T) {
		city := t.TempDir()
		rig := filepath.Join(city, "rigs", "alpha")
		if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
			t.Fatalf("mkdir rig .beads: %v", err)
		}
		if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatalf("write metadata.json: %v", err)
		}
		writeBackupStateForFreshness(t, rig, now.Add(-48*time.Hour).Format(time.RFC3339))

		safe, reason := BulkDeleteSafe(city, nil, maxAge, now)
		if safe {
			t.Fatalf("nil config with a stale rig: want safe=false, got safe=true")
		}
		if !strings.Contains(reason, "ago") {
			t.Fatalf("reason should describe the stale age, got %q", reason)
		}
	})
}

func writeBackupStateForFreshness(t *testing.T, scopeRoot, timestamp string) {
	t.Helper()
	dir := filepath.Join(scopeRoot, ".beads", "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir backup dir: %v", err)
	}
	body := `{"last_dolt_commit":"abc123","timestamp":"` + timestamp + `"}`
	if err := os.WriteFile(filepath.Join(dir, "backup_state.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write backup_state.json: %v", err)
	}
}

// writeDoltBackupRegistration marks a scope as migrated to a Dolt backup
// destination, which is what makes the Dolt state file authoritative for it.
func writeDoltBackupRegistration(t *testing.T, scopeRoot string) {
	t.Helper()
	dir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	body := `{"backup_url":"file:///tmp/backup-dest","backup_name":"default"}`
	if err := os.WriteFile(filepath.Join(dir, "dolt-backup.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write dolt-backup.json: %v", err)
	}
}

// managedDoltBackupChunk names a table file in the managed destination.
const managedDoltBackupChunk = "vt6h1k9qpc0m3s8dnb2gafr7lx4jwe5y.darc"

// writeManagedDoltBackupSync stages a sync of db into <city>/.dolt-backup/<db>
// that completed at syncedAt: its chunk, then the manifest that adopts it.
func writeManagedDoltBackupSync(t *testing.T, cityPath, db string, syncedAt time.Time) {
	t.Helper()
	writeManagedDoltBackupFile(t, cityPath, db, managedDoltBackupChunk, syncedAt)
	writeManagedDoltBackupFile(t, cityPath, db, "manifest", syncedAt)
}

// writeManagedDoltBackupFile writes one file into <city>/.dolt-backup/<db> and
// dates it, so a test can stage a sync that stopped partway. The path is joined
// the way the check joins it, so a db that climbs out of .dolt-backup lands
// where an unvalidated read would look.
func writeManagedDoltBackupFile(t *testing.T, cityPath, db, name string, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(cityPath, ".dolt-backup", db)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir managed dolt backup dir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("backup artifact"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

// writeManagedCityRuntime makes the city's managed Dolt server resolvable — its
// runtime state names this live process and a port that accepts connections —
// so a rig that inherits the city's endpoint resolves as gc-managed.
func writeManagedCityRuntime(t *testing.T, cityPath string) {
	t.Helper()
	writeDoctorRuntimeState(t, fsys.OSFS{}, cityPath, listenLoopbackPort(t))
}

// writeScopeDoltDatabase names the scope's Dolt database, which is what maps a
// scope root onto its directory under <city>/.dolt-backup.
func writeScopeDoltDatabase(t *testing.T, scopeRoot string) {
	t.Helper()
	dir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	body := `{"backend":"dolt","dolt_mode":"server","dolt_database":"` + testScopeDoltDatabase + `"}`
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

// writeDoltBackupState stamps the file a successful Dolt backup sync writes.
func writeDoltBackupState(t *testing.T, scopeRoot, lastSync string) {
	t.Helper()
	dir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	body := `{"last_sync":"` + lastSync + `","duration":"25ms"}`
	if err := os.WriteFile(filepath.Join(dir, "dolt-backup-state.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write dolt-backup-state.json: %v", err)
	}
}

func TestBdBackupFreshnessCheck(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	maxAge := 24 * time.Hour

	t.Run("fresh sync is OK", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, now.Add(-1*time.Hour).Format(time.RFC3339Nano))
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusOK {
			t.Fatalf("fresh backup: want StatusOK, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("stale sync warns and reports the age", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, now.Add(-72*time.Hour).Format(time.RFC3339Nano))
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("stale backup: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "ago") {
			t.Fatalf("stale message should describe the age, got %q", r.Message)
		}
		if r.FixHint == "" {
			t.Fatalf("stale finding should carry a FixHint")
		}
	})

	t.Run("missing backup_state.json is skipped (OK, not this check's job)", func(t *testing.T) {
		scope := t.TempDir() // no .beads/backup at all
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusOK {
			t.Fatalf("no backup: want StatusOK, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("missing timestamp warns", func(t *testing.T) {
		scope := t.TempDir()
		dir := filepath.Join(scope, ".beads", "backup")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "backup_state.json"), []byte(`{"last_dolt_commit":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("missing timestamp: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("unparseable timestamp warns", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, "not-a-timestamp")
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("bad timestamp: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
	})

	t.Run("one stale scope among fresh ones still warns", func(t *testing.T) {
		fresh := t.TempDir()
		stale := t.TempDir()
		writeBackupStateForFreshness(t, fresh, now.Add(-2*time.Hour).Format(time.RFC3339Nano))
		writeBackupStateForFreshness(t, stale, now.Add(-100*time.Hour).Format(time.RFC3339Nano))
		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{fresh, stale}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("mixed: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
	})

	// A scope that has migrated to a Dolt backup destination must be judged on
	// the Dolt state file. Reading the legacy backup_state.json there produces a
	// warning no operator can clear, because a successful sync never writes it.
	t.Run("migrated scope is judged on the dolt backup state, not the frozen legacy file", func(t *testing.T) {
		scope := t.TempDir()
		// Legacy file frozen at migration time — a week stale, and stays that way.
		writeBackupStateForFreshness(t, scope, now.Add(-168*time.Hour).Format(time.RFC3339Nano))
		writeDoltBackupRegistration(t, scope)
		writeDoltBackupState(t, scope, now.Add(-1*time.Minute).Format(time.RFC3339Nano))

		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusOK {
			t.Fatalf("fresh dolt backup alongside frozen legacy state: want StatusOK, got %v (%s)", r.Status, r.Message)
		}
	})

	// The falsifiable case: the check must still fire on a genuinely stale Dolt
	// backup. A check that only ever passes is worse than the false positive it
	// replaced, so this failing case is what makes the OK above meaningful.
	t.Run("stale dolt backup still warns", func(t *testing.T) {
		scope := t.TempDir()
		writeDoltBackupRegistration(t, scope)
		writeDoltBackupState(t, scope, now.Add(-72*time.Hour).Format(time.RFC3339Nano))

		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("stale dolt backup: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "dolt backup") {
			t.Fatalf("finding must name the store it describes, got %q", r.Message)
		}
	})

	// A registered destination that has never completed a sync is a real gap,
	// not a scope to skip — the absent state file is the only evidence of it.
	t.Run("registered dolt backup that never synced warns", func(t *testing.T) {
		scope := t.TempDir()
		writeDoltBackupRegistration(t, scope) // no dolt-backup-state.json

		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("never-synced dolt backup: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "never synced") {
			t.Fatalf("message should say the backup never synced, got %q", r.Message)
		}
	})

	// Unmigrated scopes must keep their existing behavior.
	t.Run("unmigrated scope still reads the legacy file", func(t *testing.T) {
		scope := t.TempDir()
		writeBackupStateForFreshness(t, scope, now.Add(-72*time.Hour).Format(time.RFC3339Nano))

		r := NewBdBackupFreshnessCheckForScopeRoots("", []string{scope}, maxAge, clock).Run(nil)
		if r.Status != StatusWarning {
			t.Fatalf("stale legacy backup: want StatusWarning, got %v (%s)", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "embedded-store backup") {
			t.Fatalf("finding must name the store it describes, got %q", r.Message)
		}
	})

	t.Run("Name and CanFix are stable", func(t *testing.T) {
		c := NewBdBackupFreshnessCheckForScopeRoots("", nil, maxAge, clock)
		if c.Name() != "bd-backup-freshness" {
			t.Fatalf("unexpected name %q", c.Name())
		}
		if c.CanFix() {
			t.Fatalf("CanFix should be false (report-only)")
		}
	})
}

// TestBackupFreshnessManagedDestination covers a scope mol-dog-backup has not
// registered yet: the dog syncs its database into <city>/.dolt-backup/<db>
// while its legacy backup_state.json stays frozen. A completed sync of the
// scope's own database within maxAge withdraws the frozen legacy finding;
// nothing else about the managed destination changes the legacy verdict, and it
// never raises a finding of its own. Every row runs through both
// BdBackupFreshnessCheck and BulkDeleteSafe, which must agree.
func TestBackupFreshnessManagedDestination(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	maxAge := 24 * time.Hour
	recent := now.Add(-2 * time.Hour)
	stale := now.Add(-72 * time.Hour)
	frozen := now.Add(-168 * time.Hour).Format(time.RFC3339)
	// legacyFinding is the frozen legacy file's verdict, which stands whenever
	// the managed destination does not withdraw it.
	const legacyFinding = "embedded-store backup: last sync"

	// frozenLegacyRig stages the shape this test is about: a rig whose
	// metadata.json names its database and whose legacy file froze a week ago,
	// under a city whose managed Dolt server resolves.
	frozenLegacyRig := func(t *testing.T, city, scope string) {
		t.Helper()
		writeScopeDoltDatabase(t, scope)
		writeBackupStateForFreshness(t, scope, frozen)
		writeManagedCityRuntime(t, city)
	}

	cases := []struct {
		name    string
		arrange func(t *testing.T, city, scope string)
		// cityless runs the check and the gate with no city path, from inside
		// the city.
		cityless bool
		// want is a substring the finding must contain; "" wants no finding.
		want string
	}{
		{
			name: "a sync completed within maxAge withdraws the frozen legacy finding",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
			},
		},
		{
			// Doubt about the legacy file is withdrawn too, not only a stale
			// timestamp: the managed sync is a known recovery point whatever the
			// legacy file holds.
			name: "a sync completed within maxAge withdraws an unparseable legacy finding",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				if err := os.WriteFile(filepath.Join(scope, ".beads", "backup", "backup_state.json"), []byte(`{not json`), 0o644); err != nil {
					t.Fatalf("write unparseable backup_state.json: %v", err)
				}
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
			},
		},
		{
			name: "a sync completed exactly maxAge ago still counts",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, now.Add(-maxAge))
			},
		},
		{
			name: "a sync older than maxAge leaves the legacy finding standing",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, stale)
			},
			want: legacyFinding,
		},
		{
			// The chunk belongs to a later sync that never rewrote the manifest
			// to adopt it, so the last completed sync is still the stale one.
			name: "a chunk newer than the manifest does not freshen it",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, stale)
				writeManagedDoltBackupFile(t, city, testScopeDoltDatabase, "0p3s7mlk2c9q4v1b8d5a6r0e3u7n2f4h.darc", recent)
			},
			want: legacyFinding,
		},
		{
			name: "chunks without a manifest are no backup",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupFile(t, city, testScopeDoltDatabase, managedDoltBackupChunk, recent)
			},
			want: legacyFinding,
		},
		{
			name: "an empty destination dir is no backup",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				if err := os.MkdirAll(filepath.Join(city, ".dolt-backup", testScopeDoltDatabase), 0o755); err != nil {
					t.Fatalf("mkdir managed dolt backup dir: %v", err)
				}
			},
			want: legacyFinding,
		},
		{
			name: "a manifest that is not a regular file is no backup",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				manifest := filepath.Join(city, ".dolt-backup", testScopeDoltDatabase, "manifest")
				if err := os.MkdirAll(manifest, 0o755); err != nil {
					t.Fatalf("mkdir manifest: %v", err)
				}
				if err := os.Chtimes(manifest, recent, recent); err != nil {
					t.Fatalf("chtimes manifest: %v", err)
				}
			},
			want: legacyFinding,
		},
		{
			// The stat fails with ENOTDIR, which no permission bit can mask, so
			// the row holds when the tests run as root.
			name: "an unreadable destination leaves the legacy finding standing",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				if err := os.MkdirAll(filepath.Join(city, ".dolt-backup"), 0o755); err != nil {
					t.Fatalf("mkdir .dolt-backup: %v", err)
				}
				if err := os.WriteFile(filepath.Join(city, ".dolt-backup", testScopeDoltDatabase), []byte("not a directory"), 0o644); err != nil {
					t.Fatalf("write a file over the destination dir: %v", err)
				}
			},
			want: legacyFinding,
		},
		{
			name: "a stale destination raises no finding of its own",
			arrange: func(t *testing.T, city, scope string) {
				writeScopeDoltDatabase(t, scope)
				writeManagedCityRuntime(t, city)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, stale)
			},
		},
		{
			// "beads" is the name the endpoint resolver falls back to, so a
			// guess would find this sync.
			name: "a scope with no metadata.json is not guessed at",
			arrange: func(t *testing.T, city, scope string) {
				writeBackupStateForFreshness(t, scope, frozen)
				writeManagedCityRuntime(t, city)
				writeManagedDoltBackupSync(t, city, "beads", recent)
			},
			want: legacyFinding,
		},
		{
			name: "a scope with unparseable metadata.json is not guessed at",
			arrange: func(t *testing.T, city, scope string) {
				writeDoctorRawMetadata(t, scope, `{"dolt_database":`)
				writeBackupStateForFreshness(t, scope, frozen)
				writeManagedCityRuntime(t, city)
				writeManagedDoltBackupSync(t, city, "beads", recent)
			},
			want: legacyFinding,
		},
		{
			name: "a scope whose metadata.json names no database is not guessed at",
			arrange: func(t *testing.T, city, scope string) {
				writeDoctorRawMetadata(t, scope, `{"backend":"dolt","dolt_mode":"server"}`)
				writeBackupStateForFreshness(t, scope, frozen)
				writeManagedCityRuntime(t, city)
				writeManagedDoltBackupSync(t, city, "beads", recent)
			},
			want: legacyFinding,
		},
		{
			// Joined unvalidated, the name would read <city>/escape, outside
			// .dolt-backup, where this sync waits to be mistaken for the scope's.
			name: "a database name that climbs out of .dolt-backup is not read",
			arrange: func(t *testing.T, city, scope string) {
				writeDoctorRawMetadata(t, scope, `{"backend":"dolt","dolt_mode":"server","dolt_database":"../escape"}`)
				writeBackupStateForFreshness(t, scope, frozen)
				writeManagedCityRuntime(t, city)
				writeManagedDoltBackupSync(t, city, "../escape", recent)
			},
			want: legacyFinding,
		},
		{
			name: "a city handed to bd keeps its backups outside the managed destination",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
				writeHandoffJournal(t, city, `{"phase":"committed","owner":"bd"}`)
			},
			want: legacyFinding,
		},
		{
			name: "an external endpoint keeps its backups outside the managed destination",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
				writeScopeConfig(t, city, "issue_prefix: gc\ngc.endpoint_origin: managed_city\ndolt.auto-start: false\n")
				writeScopeConfig(t, scope, "issue_prefix: fe\ngc.endpoint_origin: explicit\ndolt.host: db.example.com\ndolt.port: \"3307\"\ndolt.auto-start: false\n")
			},
			want: legacyFinding,
		},
		{
			// No runtime state: the city is stopped, so its managed endpoint
			// does not resolve. DoltBackupCheck still reads the directory for
			// presence; withdrawing a finding needs the scope proven to back up
			// there.
			name: "a stopped city's unresolvable endpoint is doubt, not evidence",
			arrange: func(t *testing.T, city, scope string) {
				writeScopeDoltDatabase(t, scope)
				writeBackupStateForFreshness(t, scope, frozen)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
			},
			want: legacyFinding,
		},
		{
			// bd stamps a registered destination's own state file on every sync,
			// so that file decides even when the managed destination is fresher.
			name: "a registered destination outranks a fresh managed sync",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeDoltBackupRegistration(t, scope)
				writeDoltBackupState(t, scope, stale.Format(time.RFC3339))
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
			},
			want: ": dolt backup: last sync",
		},
		{
			name: "without a city path there is no managed destination to read",
			arrange: func(t *testing.T, city, scope string) {
				frozenLegacyRig(t, city, scope)
				writeManagedDoltBackupSync(t, city, testScopeDoltDatabase, recent)
			},
			cityless: true,
			want:     legacyFinding,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			scope := filepath.Join(city, "rigs", "alpha")
			tc.arrange(t, city, scope)
			cityPath := city
			if tc.cityless {
				cityPath = ""
				// From inside the city, a read relative to the missing path
				// would find the city's fresh sync.
				t.Chdir(city)
			}

			r := NewBdBackupFreshnessCheckForScopeRoots(cityPath, []string{scope}, maxAge, func() time.Time { return now }).Run(nil)
			cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: scope}}}
			safe, reason := BulkDeleteSafe(cityPath, cfg, maxAge, now)

			if tc.want == "" {
				if r.Status != StatusOK {
					t.Errorf("check: want StatusOK, got %v (%s)", r.Status, r.Message)
				}
				if !safe || reason != "" {
					t.Errorf("BulkDeleteSafe: want safe with no reason, got safe=%v reason=%q", safe, reason)
				}
				return
			}
			if r.Status != StatusWarning || !strings.Contains(r.Message, tc.want) {
				t.Errorf("check: want StatusWarning naming %q, got %v (%s)", tc.want, r.Status, r.Message)
			}
			if safe || !strings.Contains(reason, tc.want) {
				t.Errorf("BulkDeleteSafe: want unsafe naming %q, got safe=%v reason=%q", tc.want, safe, reason)
			}
		})
	}
}
