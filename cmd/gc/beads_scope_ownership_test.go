package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestProviderScopeOwnershipPersistsOnlyPendingIntent(t *testing.T) {
	city := t.TempDir()
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatalf("persist provider ownership: %v", err)
	}
	entry, ok, err := providerScopeOwnership(city, city)
	if err != nil || !ok {
		t.Fatalf("providerScopeOwnership = (%+v, %v, %v), want pending entry", entry, ok, err)
	}
	if entry.LifecycleOwner != providerScopeLifecycleOwner || entry.State != providerScopeInitializing || entry.Intent.Transport != "proxied" || entry.Intent.Target != "local" {
		t.Fatalf("entry = %+v", entry)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	entry, ok, err = providerScopeOwnership(city, city)
	if err != nil || !ok {
		t.Fatalf("providerScopeOwnership after ready = (%+v, %v, %v)", entry, ok, err)
	}
	if entry.State != providerScopeReady || entry.Intent != (providerScopeIntent{}) {
		t.Fatalf("ready entry must not retain topology intent: %+v", entry)
	}
	data, err := os.ReadFile(filepath.Join(city, ".gc", "scope-ownership.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "proxied") || strings.Contains(string(data), "local") {
		t.Fatalf("ready ownership journal persisted topology: %s", data)
	}
}

func TestProviderScopeOwnershipFailsClosedOnPathDrift(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, ".gc", "scope-ownership.json")
	data := `{"version":1,"scopes":{"city":{"scope_path":"/wrong","lifecycle_owner":"provider","state":"ready"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := providerScopeOwnership(city, city)
	if err == nil || !strings.Contains(err.Error(), "path drift") {
		t.Fatalf("providerScopeOwnership error = %v, want path drift", err)
	}
}

func TestProviderScopeOwnershipFailsClosedOnRigPathDrift(t *testing.T) {
	city := t.TempDir()
	oldRig := filepath.Join(city, "old-rig")
	newRig := filepath.Join(city, "new-rig")
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[[rig]]\nname = \"repo\"\npath = \"new-rig\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := `{"version":1,"scopes":{"rig:repo":{"scope_path":"` + oldRig + `","lifecycle_owner":"provider","state":"ready"}}}`
	if err := os.WriteFile(filepath.Join(city, ".gc", "scope-ownership.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderScopeOwnership(city, &config.City{Rigs: []config.Rig{{Name: "repo", Path: newRig}}}); err == nil || !strings.Contains(err.Error(), "path drift") {
		t.Fatalf("validateProviderScopeOwnership = %v, want path drift", err)
	}
}

func TestRemoveProviderScopeOwnershipRecordAfterRigRemoval(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "removed")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[[rigs]]\nname = \"removed\"\npath = \"rigs/removed\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rig); err != nil {
		t.Fatal(err)
	}
	if err := removeProviderScopeOwnershipRecord(city, "rig:removed"); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderScopeOwnership(city, &config.City{}); err != nil {
		t.Fatalf("removed rig ownership blocked city startup: %v", err)
	}
	key, entry, owned, err := providerScopeOwnershipRecord(city, rig)
	if err != nil || !owned || key != "path:"+rig || entry.State != providerScopeReady {
		t.Fatalf("removed rig ownership = (%q, %+v, %t, %v), want detached ready record", key, entry, owned, err)
	}
}

func TestProviderScopeOwnershipReattachesDetachedPathAcrossRenamedRig(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "readded")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[[rigs]]\nname = \"old\"\npath = \"rigs/readded\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rig); err != nil {
		t.Fatal(err)
	}
	if err := removeProviderScopeOwnershipRecord(city, "rig:old"); err != nil {
		t.Fatal(err)
	}
	// cmdRigRemove detaches before writing city.toml. A failed config write
	// therefore leaves the still-configured rig valid and retryable.
	if err := validateProviderScopeOwnership(city, &config.City{Rigs: []config.Rig{{Name: "old", Path: rig}}}); err != nil {
		t.Fatalf("pre-write detached ownership validation: %v", err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[[rigs]]\nname = \"renamed\"\npath = \"rigs/readded\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	key, entry, owned, err := providerScopeOwnershipRecord(city, rig)
	if err != nil || !owned || key != "path:"+rig || entry.State != providerScopeReady {
		t.Fatalf("readded ownership = (%q, %+v, %t, %v), want detached ready record", key, entry, owned, err)
	}
	if err := validateProviderScopeOwnership(city, &config.City{Rigs: []config.Rig{{Name: "renamed", Path: rig}}}); err != nil {
		t.Fatalf("renamed re-add ownership validation: %v", err)
	}
}

func TestProviderScopeOwnershipDoesNotGiveReusedNameDetachedScope(t *testing.T) {
	city := t.TempDir()
	oldRoot := filepath.Join(city, "rigs", "old")
	newRoot := filepath.Join(city, "rigs", "new")
	for _, root := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[[rigs]]\nname = \"repo\"\npath = \"rigs/old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	intent := providerScopeIntent{Transport: "proxied", Target: "local"}
	if err := persistProviderScopeOwnership(city, oldRoot, intent); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := removeProviderScopeOwnershipRecord(city, "rig:repo"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[[rigs]]\nname = \"repo\"\npath = \"rigs/new\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, newRoot, intent); err != nil {
		t.Fatal(err)
	}
	oldKey, oldEntry, oldOwned, err := providerScopeOwnershipRecord(city, oldRoot)
	if err != nil || !oldOwned || oldKey != "path:"+oldRoot || oldEntry.State != providerScopeReady {
		t.Fatalf("old detached ownership = (%q, %+v, %t, %v)", oldKey, oldEntry, oldOwned, err)
	}
	newKey, newEntry, newOwned, err := providerScopeOwnershipRecord(city, newRoot)
	if err != nil || !newOwned || newKey != "rig:repo" || newEntry.State != providerScopeInitializing {
		t.Fatalf("new ownership = (%q, %+v, %t, %v)", newKey, newEntry, newOwned, err)
	}
}

func TestProviderScopeOwnershipRejectsDuplicateOrMismatchedDetachedPaths(t *testing.T) {
	city := t.TempDir()
	root := filepath.Join(city, "rig")
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		data string
		want string
	}{
		{
			name: "duplicate physical path",
			data: `{"version":1,"scopes":{"rig:repo":{"scope_path":"` + root + `","lifecycle_owner":"provider","state":"ready"},"path:` + root + `":{"scope_path":"` + root + `","lifecycle_owner":"provider","state":"ready"}}}`,
			want: "duplicate scope ownership paths",
		},
		{
			name: "mismatched detached path key",
			data: `{"version":1,"scopes":{"path:/wrong":{"scope_path":"` + root + `","lifecycle_owner":"provider","state":"ready"}}}`,
			want: "path key mismatch",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(city, ".gc", scopeOwnershipFile), []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := providerScopeOwnership(city, root); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("providerScopeOwnership error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestProviderScopeOwnershipRemoveAndAdoptReaddKeepsProviderLifecycle(t *testing.T) {
	for _, readdName := range []string{"old", "renamed"} {
		t.Run(readdName, func(t *testing.T) {
			testProviderScopeOwnershipRemoveAndAdoptReadd(t, readdName)
		})
	}
}

func testProviderScopeOwnershipRemoveAndAdoptReadd(t *testing.T, readdName string) {
	t.Helper()
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	resetFlags(t)
	city := setupCity(t, "provider-readd")
	rig := filepath.Join(t.TempDir(), "provider-rig")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "config.yaml"), []byte("issue_prefix: provider_rig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	providerLog := filepath.Join(city, "provider.log")
	provider := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", providerLog)
	writeRigAnywhereCityToml(t, city, "[workspace]\nname = \"provider-readd\"\n[beads]\nprovider = \"exec:"+provider+"\"\n[[rigs]]\nname = \"old\"\npath = \""+rig+"\"\n")
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rig); err != nil {
		t.Fatal(err)
	}
	registerCityForRigResolution(t, gcHome, city, "provider-readd")
	cityFlag = city
	var removeOut, removeErr bytes.Buffer
	if code := cmdRigRemove("old", &removeOut, &removeErr); code != 0 {
		t.Fatalf("gc rig remove = %d, stderr: %s", code, removeErr.String())
	}
	var addOut, addErr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, city, rig, nil, readdName, "provider_rig", "", false, true, &addOut, &addErr); code != 0 {
		t.Fatalf("gc rig add --adopt = %d, stderr: %s", code, addErr.String())
	}
	data, err := os.ReadFile(providerLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Fields(string(data))
	legacyInit := false
	for _, call := range calls {
		legacyInit = legacyInit || call == "init"
	}
	if len(calls) == 0 || legacyInit {
		t.Fatalf("provider calls after adopted re-add = %v, want provider lifecycle without legacy init", calls)
	}
	key, entry, owned, err := providerScopeOwnershipRecord(city, rig)
	if err != nil || !owned || key != "rig:"+readdName || entry.State != providerScopeReady {
		t.Fatalf("adopted re-add ownership = (%q, %+v, %t, %v)", key, entry, owned, err)
	}
}

func TestDetachedProviderScopeIsExcludedFromLifecycleAndReadiness(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "removed")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "provider.log")
	provider := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"exec:"+provider+"\"\n[[rigs]]\nname = \"removed\"\npath = \"rigs/removed\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rig); err != nil {
		t.Fatal(err)
	}
	if err := removeProviderScopeOwnershipRecord(city, "rig:removed"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"exec:"+provider+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOwnedScopesLifecycleOp(city, "health"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("detached scope participated in lifecycle: %v", err)
	}
	if err := validateProviderScopeOwnership(city, &config.City{}); err != nil {
		t.Fatalf("detached scope blocked readiness: %v", err)
	}
}

func TestRemoveProviderScopeOwnershipRecordLeavesLegacyCityUntouched(t *testing.T) {
	city := t.TempDir()
	if err := removeProviderScopeOwnershipRecord(city, "rig:removed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(city, ".gc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy removal created .gc: %v", err)
	}
}

func TestProviderScopeOwnershipMissingIsLegacy(t *testing.T) {
	entry, ok, err := providerScopeOwnership(t.TempDir(), t.TempDir())
	if err != nil || ok || entry != (providerScopeOwnershipEntry{}) {
		t.Fatalf("missing journal = (%+v, %v, %v), want no legacy entry", entry, ok, err)
	}
}

func TestInitDirIfReadySkipRecordsFreshRigIntentForProviderOwnedCity(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "fresh")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "provider.log")
	provider := filepath.Join(city, "gc-beads-bd")
	script := `#!/bin/sh
printf '%s\n' "$1" >> "$GC_TEST_PROVIDER_LOG"
if [ "$1" = init ]; then
  mkdir -p "$2/.beads"
  printf '{"backend":"dolt","dolt_mode":"server"}\n' > "$2/.beads/metadata.json"
  printf 'dolt.mode: server\n' > "$2/.beads/config.yaml"
fi
exit 0
`
	if err := os.WriteFile(provider, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("dolt.mode: server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"exec:"+provider+"\"\n[[rigs]]\nname = \"fresh\"\npath = \"rigs/fresh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	t.Setenv("GC_DOLT", "skip")
	deferred, err := initDirIfReady(city, rig, "fr")
	if err != nil || !deferred {
		t.Fatalf("skip init = (%t, %v), want deferred without error", deferred, err)
	}
	entry, owned, err := providerScopeOwnership(city, rig)
	if err != nil || !owned || entry.State != providerScopeInitializing || entry.Intent != (providerScopeIntent{Transport: "direct", Target: "local"}) {
		t.Fatalf("pending rig ownership = (%+v, %t, %v)", entry, owned, err)
	}
	if _, err := os.Stat(filepath.Join(rig, ".beads")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skip created rig beads artifacts: %v", err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skip invoked provider: %v", err)
	}
	t.Setenv("GC_DOLT", "")
	deferred, err = initDirIfReady(city, rig, "fr")
	if err != nil || deferred {
		t.Fatalf("retry init = (%t, %v), want initialized", deferred, err)
	}
	entry, owned, err = providerScopeOwnership(city, rig)
	if err != nil || !owned || entry.State != providerScopeReady || entry.Intent != (providerScopeIntent{}) {
		t.Fatalf("ready rig ownership = (%+v, %t, %v)", entry, owned, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), "init") || !strings.Contains(string(data), "health") {
		t.Fatalf("provider retry operations = %q, %v", data, err)
	}
}

func TestInitDirIfReadySkipDefersExistingRigOwnershipWithoutCityOwnership(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "owned")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "provider.log")
	provider := filepath.Join(city, "gc-beads-bd")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig, ".beads", "config.yaml"), []byte("dolt.mode: server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"exec:"+provider+"\"\n[[rigs]]\nname = \"owned\"\npath = \"rigs/owned\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	t.Setenv("GC_DOLT", "skip")
	deferred, err := initDirIfReady(city, rig, "ow")
	if err != nil || !deferred {
		t.Fatalf("skip init = (%t, %v), want deferred", deferred, err)
	}
	entry, owned, err := providerScopeOwnership(city, rig)
	if err != nil || !owned || entry.State != providerScopeInitializing || entry.Intent != (providerScopeIntent{Transport: "direct", Target: "local"}) {
		t.Fatalf("rig ownership changed = (%+v, %t, %v)", entry, owned, err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skip invoked provider: %v", err)
	}
}

// Re-running init over a city that already exists records a fresh rig only
// when the city's own lifecycle belongs to bd. A grandfathered GC-managed city
// keeps its rigs on the legacy inherited-city path: converting an existing city
// is `bd migrate`'s job, not a side effect of `gc init` (D6).
func TestPersistFreshProviderOwnershipRecordsFreshRigOnlyUnderAProviderOwnedCity(t *testing.T) {
	for _, tt := range []struct {
		name      string
		metadata  string
		wantOwned bool
	}{
		{name: "legacy managed city", metadata: `{"backend":"dolt"}`, wantOwned: false},
		{name: "bd-owned proxied city", metadata: `{"backend":"dolt","dolt_mode":"proxied-server"}`, wantOwned: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "fresh")
			if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(tt.metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(rig, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"bd\"\n[[rigs]]\nname = \"fresh\"\npath = \"rigs/fresh\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := persistFreshProviderOwnership(city, hostedDoltInitOptions{}); err != nil {
				t.Fatal(err)
			}
			entry, owned, err := providerScopeOwnership(city, rig)
			if err != nil {
				t.Fatalf("fresh rig ownership: %v", err)
			}
			if owned != tt.wantOwned {
				t.Fatalf("fresh rig ownership = (%+v, %t), want owned=%t", entry, owned, tt.wantOwned)
			}
			if tt.wantOwned && entry.State != providerScopeInitializing {
				t.Fatalf("fresh rig state = %q, want pending", entry.State)
			}
		})
	}
}

// A newly added rig inherits the city binding that bd already persisted — but
// only from a city whose lifecycle bd already owns. A direct/server city is
// grandfathered: its rigs are databases on its one managed server, so they stay
// on the legacy inherited-city path and are not journaled at all. Stale
// compatibility fields in city.toml must not turn any of them into the new
// proxied-local default.
func TestEnsureFreshRigProviderOwnershipInheritsPersistedCityTopology(t *testing.T) {
	for _, tt := range []struct {
		name        string
		metadata    string
		beadsConfig string
		sidecar     string
		staleDolt   string
		journalCity *providerScopeIntent
		wantLegacy  bool
		want        providerScopeIntent
	}{
		{
			name:        "legacy direct local owned canonical endpoint",
			metadata:    `{"backend":"dolt","dolt_mode":"server"}`,
			beadsConfig: "gc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 3306\ndolt.auto-start: true\n",
			staleDolt:   "[dolt]\nhost = \"stale.example\"\nport = 3306\n",
			wantLegacy:  true,
		},
		{
			name:        "legacy transferred direct local",
			metadata:    `{"backend":"dolt","dolt_mode":"server"}`,
			beadsConfig: "gc.endpoint_origin: managed_city\ndolt.auto-start: true\n",
			staleDolt:   "[dolt]\nhost = \"remote.example\"\nport = 3306\n",
			wantLegacy:  true,
		},
		{
			name:        "legacy GC managed direct local",
			metadata:    `{"backend":"dolt","dolt_mode":"server"}`,
			beadsConfig: "gc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.auto-start: false\n",
			staleDolt:   "[dolt]\nhost = \"stale.example\"\nport = 3306\n",
			wantLegacy:  true,
		},
		{
			name:        "legacy direct external",
			metadata:    `{"backend":"dolt","dolt_mode":"server"}`,
			beadsConfig: "gc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 3306\ndolt.auto-start: false\n",
			staleDolt:   "",
			wantLegacy:  true,
		},
		{
			name:        "bd-owned direct local escape hatch",
			metadata:    `{"backend":"dolt","dolt_mode":"server"}`,
			beadsConfig: "gc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n",
			staleDolt:   "[dolt]\nhost = \"stale.example\"\nport = 3306\n",
			journalCity: &providerScopeIntent{Transport: "direct", Target: "local"},
			want:        providerScopeIntent{Transport: "direct", Target: "local"},
		},
		{
			name:        "bd-owned direct external",
			metadata:    `{"backend":"dolt","dolt_mode":"server"}`,
			beadsConfig: "gc.endpoint_origin: city_canonical\ndolt.host: 127.0.0.1\ndolt.port: 3306\ndolt.auto-start: false\n",
			journalCity: &providerScopeIntent{Transport: "direct", Target: "external"},
			want:        providerScopeIntent{Transport: "direct", Target: "external"},
		},
		{
			name:        "proxied local",
			metadata:    `{"backend":"dolt","dolt_mode":"proxied-server"}`,
			beadsConfig: "gc.endpoint_origin: managed_city\ndolt.mode: proxied-server\ndolt.auto-start: true\n",
			staleDolt:   "[dolt]\nhost = \"stale.example\"\nport = 3306\n",
			want:        providerScopeIntent{Transport: "proxied", Target: "local"},
		},
		{
			name:        "proxied external tcp",
			metadata:    `{"backend":"dolt","dolt_mode":"proxied-server"}`,
			beadsConfig: "gc.endpoint_origin: managed_city\ndolt.mode: proxied-server\ndolt.auto-start: true\n",
			sidecar:     `{"external":{"host":"127.0.0.1","port":3306}}`,
			staleDolt:   "[dolt]\nhost = \"stale.example\"\nport = 3306\n",
			want:        providerScopeIntent{Transport: "proxied", Target: "external"},
		},
		{
			name:        "proxied external unix socket",
			metadata:    `{"backend":"dolt","dolt_mode":"proxied-server"}`,
			beadsConfig: "gc.endpoint_origin: managed_city\ndolt.mode: proxied-server\ndolt.auto-start: true\n",
			sidecar:     `{"external":{"socket":"/run/dolt.sock"}}`,
			staleDolt:   "[dolt]\nhost = \"stale.example\"\nport = 3306\n",
			want:        providerScopeIntent{Transport: "proxied", Target: "external"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "fresh")
			if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(rig, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(tt.metadata), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(tt.beadsConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.sidecar != "" {
				if err := os.WriteFile(filepath.Join(city, ".beads", "proxied_server_client_info.json"), []byte(tt.sidecar), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cityTOML := "[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\n" + tt.staleDolt + "[[rigs]]\nname = \"fresh\"\npath = \"rigs/fresh\"\n"
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityTOML), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.journalCity != nil {
				if err := persistProviderScopeOwnership(city, city, *tt.journalCity); err != nil {
					t.Fatal(err)
				}
				if err := markProviderScopeOwnershipReady(city, city); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := loadCityConfig(city, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureFreshRigProviderOwnership(city, cfg); err != nil {
				t.Fatal(err)
			}
			entry, owned, err := providerScopeOwnership(city, rig)
			if err != nil {
				t.Fatalf("fresh rig ownership: %v", err)
			}
			if tt.wantLegacy {
				if owned {
					t.Fatalf("grandfathered city journaled a fresh rig: %+v", entry)
				}
				return
			}
			if !owned {
				t.Fatalf("fresh rig ownership = (%+v, %t)", entry, owned)
			}
			if entry.Intent != tt.want {
				t.Fatalf("fresh rig intent = %+v, want %+v", entry.Intent, tt.want)
			}
		})
	}
}

func TestEnsureFreshRigProviderOwnershipLeavesEmbeddedCityWithoutFreshRigsUntouched(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"embedded"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("gc.endpoint_origin: managed_city\ndolt.mode: embedded\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshRigProviderOwnership(city, cfg); err != nil {
		t.Fatalf("existing embedded city without a fresh rig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(city, ".gc", scopeOwnershipFile)); !os.IsNotExist(err) {
		t.Fatalf("unchanged city wrote a provider ownership journal: %v", err)
	}
}

// An embedded city is not provider-owned, so a fresh rig under it stays on the
// legacy inherited path and the city's own artifacts are untouched. Embedded
// scopes remain authoritative and are never automatically converted
// (ga-p9iuv.30).
func TestEnsureFreshRigProviderOwnershipLeavesEmbeddedDoltCityOnTheLegacyPath(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "fresh")
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rig, 0o700); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(city, ".beads", "metadata.json")
	configPath := filepath.Join(city, ".beads", "config.yaml")
	metadata := []byte(`{"backend":"dolt","dolt_mode":"embedded"}`)
	beadsConfig := []byte("gc.endpoint_origin: managed_city\ndolt.mode: embedded\ndolt.auto-start: false\n")
	if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, beadsConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\n[[rigs]]\nname = \"fresh\"\npath = \"rigs/fresh\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshRigProviderOwnership(city, cfg); err != nil {
		t.Fatalf("ensure fresh rig ownership: %v", err)
	}
	entry, owned, err := providerScopeOwnership(city, rig)
	if err != nil || owned {
		t.Fatalf("fresh rig ownership = (%+v, %t, %v), want the legacy inherited path", entry, owned, err)
	}
	for path, want := range map[string][]byte{metadataPath: metadata, configPath: beadsConfig} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("embedded city artifact %s = %q, %v; want unchanged %q", path, got, err, want)
		}
	}
}

func TestEnsureProviderScopeOwnershipBeforeInitInheritsPendingCityIntent(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "fresh")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n[beads]\nprovider = \"bd\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := providerScopeIntent{Transport: "direct", Target: "external"}
	if err := persistProviderScopeOwnership(city, city, want); err != nil {
		t.Fatal(err)
	}
	if err := ensureProviderScopeOwnershipBeforeInit(city, rig); err != nil {
		t.Fatalf("ensureProviderScopeOwnershipBeforeInit: %v", err)
	}
	entry, owned, err := providerScopeOwnership(city, rig)
	if err != nil || !owned || entry.State != providerScopeInitializing || entry.Intent != want {
		t.Fatalf("fresh rig ownership = (%+v, %t, %v), want pending %+v", entry, owned, err, want)
	}
}

func TestProviderOwnedLifecycleRejectsProviderExitTwo(t *testing.T) {
	city := t.TempDir()
	script := writeTestScript(t, "start", 2, "provider unavailable")
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := ensureBeadsProvider(city); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("ensureBeadsProvider = %v, want provider exit-two failure", err)
	}
}

func TestProviderOwnedStartInitializesAndMarksReady(t *testing.T) {
	city := t.TempDir()
	logPath := filepath.Join(city, "provider-ops")
	script := filepath.Join(city, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	entry, ok, err := providerScopeOwnership(city, city)
	if err != nil || !ok || entry.State != providerScopeReady {
		t.Fatalf("scope ownership after start = (%+v, %t, %v), want ready", entry, ok, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, []string{"init", "health"}) {
		t.Fatalf("provider operation order = %v, want init health", got)
	}
}

func TestProviderOwnedExternalInitPassesOneShotDatabaseToBD(t *testing.T) {
	city := t.TempDir()
	logPath := filepath.Join(city, "provider-ops")
	script := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "external"}); err != nil {
		t.Fatal(err)
	}
	opts := hostedDoltInitOptions{Transport: "direct", Target: "external", Host: "127.0.0.1", Port: "3306", Database: "bd_external", ProjectID: "project"}
	if err := opts.registerSelectorEndpointForInit(city); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		selectorExternalInitOptions.Delete(normalizePathForCompare(city))
		clearCityDoltConfig(city)
	})
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, []string{"init", city, config.EffectiveHQPrefix(cfg), "bd_external", "health"}) {
		t.Fatalf("provider arguments = %#v, want one-shot external database", got)
	}
	if got := selectorExternalInitDatabase(city, city); got != "" {
		t.Fatalf("ready scope retained one-shot selector database %q", got)
	}
}

func TestProviderOwnedPendingExternalScopeResumesOnlyWithExplicitEnvironment(t *testing.T) {
	city := t.TempDir()
	logPath := filepath.Join(city, "provider-ops")
	script := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "external"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "GC_DOLT_HOST, GC_DOLT_PORT, and GC_DOLT_DATABASE") {
		t.Fatalf("pending external scope without endpoint = %v, want actionable gc start environment", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("missing endpoint invoked provider: %v", err)
	}
	t.Setenv(envDoltHost, "127.0.0.1")
	t.Setenv(envDoltPort, "3306")
	t.Setenv(envDoltDatabase, "bd_resume")
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
		t.Fatalf("pending external scope with explicit environment: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, []string{"init", city, config.EffectiveHQPrefix(cfg), "bd_resume", "health"}) {
		t.Fatalf("resumed provider arguments = %#v", got)
	}
	entry, owned, err := providerScopeOwnership(city, city)
	if err != nil || !owned || entry.State != providerScopeReady {
		t.Fatalf("resumed scope ownership = (%+v, %t, %v), want ready", entry, owned, err)
	}
}

// TestPendingProviderExternalRetryEndpointOverridesStaleCityConfig keeps an
// incomplete generic external selector bound to its explicit retry endpoint.
// A retained legacy [dolt] setting is compatibility fallback only; it must
// never shadow the retry input that bd will use to finish the pending scope.
func TestPendingProviderExternalRetryEndpointOverridesStaleCityConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		host       string
		port       string
		socket     string
		want       map[string]string
		absentKeys []string
	}{
		{
			name:   "tcp",
			host:   "retry.example.test",
			port:   "4411",
			socket: "/tmp/stale-os.sock",
			want: map[string]string{
				"GC_DOLT_HOST": "retry.example.test", "GC_DOLT_PORT": "4411",
				"BEADS_DOLT_SERVER_HOST": "retry.example.test", "BEADS_DOLT_SERVER_PORT": "4411",
			},
			absentKeys: []string{"BEADS_DOLT_SERVER_SOCKET"},
		},
		{
			name:   "unix socket",
			socket: "/tmp/retry-dolt.sock",
			want:   map[string]string{"BEADS_DOLT_SERVER_SOCKET": "/tmp/retry-dolt.sock"},
			absentKeys: []string{
				"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "external"}); err != nil {
				t.Fatal(err)
			}
			registerCityDoltConfig(city, config.DoltConfig{Host: "stale.example.test", Port: 4406})
			t.Cleanup(func() { clearCityDoltConfig(city) })
			t.Setenv(envDoltHost, tc.host)
			t.Setenv(envDoltPort, tc.port)
			t.Setenv("BEADS_DOLT_SERVER_SOCKET", tc.socket)
			env, err := providerLifecycleProcessEnvFromBase(city, "exec:"+gcBeadsBdScriptPath(city), []string{
				"GC_DOLT_HOST=parent-stale.example.test", "GC_DOLT_PORT=4406",
				"BEADS_DOLT_SERVER_HOST=parent-stale.example.test", "BEADS_DOLT_SERVER_PORT=4406",
				"BEADS_DOLT_SERVER_SOCKET=/tmp/parent-stale.sock",
			})
			if err != nil {
				t.Fatal(err)
			}
			got := runtimeEnvEntriesToMap(env)
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("%s = %q, want retry endpoint %q; env=%v", key, got[key], want, env)
				}
			}
			for _, key := range tc.absentKeys {
				if _, ok := got[key]; ok {
					t.Fatalf("%s should be absent for %s retry; env=%v", key, tc.name, env)
				}
			}
			for _, stale := range []string{"stale.example.test", "parent-stale.example.test", "4406", "/tmp/parent-stale.sock", "/tmp/stale-os.sock"} {
				for key, value := range got {
					if value == stale {
						t.Fatalf("stale endpoint value %q remained in %s; env=%v", stale, key, env)
					}
				}
			}
		})
	}
}

func TestProviderOwnedFreshRigInheritsReadyExternalBinding(t *testing.T) {
	for _, tt := range []struct {
		name         string
		intent       providerScopeIntent
		cityConfig   string
		sidecar      string
		wantEndpoint string
	}{
		{
			name:         "direct tcp",
			intent:       providerScopeIntent{Transport: "direct", Target: "external"},
			cityConfig:   "gc.endpoint_origin: city_canonical\ndolt.mode: server\ndolt.host: db.example.test\ndolt.port: 4406\ndolt.auto-start: false\n",
			wantEndpoint: "db.example.test|4406||||",
		},
		{
			name:         "direct raw bd canonical tcp",
			intent:       providerScopeIntent{Transport: "direct", Target: "external"},
			cityConfig:   "dolt.mode: server\ndolt.host: raw-bd.example.test\ndolt.port: 4407\ndolt.auto-start: false\n",
			wantEndpoint: "raw-bd.example.test|4407||||",
		},
		{
			name:         "direct unix",
			intent:       providerScopeIntent{Transport: "direct", Target: "external"},
			cityConfig:   "gc.endpoint_origin: city_canonical\ndolt.mode: server\ndolt.socket: /var/run/dolt/direct.sock\ndolt.auto-start: false\n",
			wantEndpoint: "|||||/var/run/dolt/direct.sock",
		},
		{
			name:         "proxied tcp",
			intent:       providerScopeIntent{Transport: "proxied", Target: "external"},
			sidecar:      `{"external":{"host":"proxy-upstream.example.test","port":5506}}`,
			wantEndpoint: "||proxy-upstream.example.test|5506||",
		},
		{
			name:         "proxied unix",
			intent:       providerScopeIntent{Transport: "proxied", Target: "external"},
			sidecar:      `{"external":{"socket":"/var/run/dolt/upstream.sock"}}`,
			wantEndpoint: "||||/var/run/dolt/upstream.sock|",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "fresh")
			if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(rig, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte(tt.cityConfig), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.sidecar != "" {
				if err := os.WriteFile(filepath.Join(city, ".beads", "proxied_server_client_info.json"), []byte(tt.sidecar), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(city, "provider-ops")
			script := filepath.Join(city, "provider.sh")
			contents := "#!/bin/sh\nprintf 'args=%s\\n' \"$*\" >> \"$GC_TEST_PROVIDER_LOG\"\nprintf 'endpoint=%s|%s|%s|%s|%s|%s\\n' \"$GC_DOLT_HOST\" \"$GC_DOLT_PORT\" \"$GC_BEADS_PROXY_EXTERNAL_HOST\" \"$GC_BEADS_PROXY_EXTERNAL_PORT\" \"$GC_BEADS_PROXY_EXTERNAL_SOCKET\" \"$BEADS_DOLT_SERVER_SOCKET\" >> \"$GC_TEST_PROVIDER_LOG\"\n"
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
			for _, key := range []string{envDoltHost, envDoltPort, "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_SOCKET", "GC_BEADS_PROXY_EXTERNAL_HOST", "GC_BEADS_PROXY_EXTERNAL_PORT", "GC_BEADS_PROXY_EXTERNAL_SOCKET"} {
				t.Setenv(key, "")
			}
			t.Setenv(envDoltDatabase, "city_database_that_must_not_escape")
			if err := persistProviderScopeOwnership(city, city, tt.intent); err != nil {
				t.Fatal(err)
			}
			if err := markProviderScopeOwnershipReady(city, city); err != nil {
				t.Fatal(err)
			}
			if err := persistProviderScopeOwnership(city, rig, tt.intent); err != nil {
				t.Fatal(err)
			}
			if _, err := runProviderOwnedScopeInit(city, rig, "fresh", script); err != nil {
				t.Fatalf("runProviderOwnedScopeInit: %v", err)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			wantArgs := "args=init " + rig + " fresh " + canonicalScopeDoltDatabase(city, rig, "fresh")
			if !reflect.DeepEqual(lines, []string{wantArgs, "endpoint=" + tt.wantEndpoint}) {
				t.Fatalf("provider init = %q, want %q", lines, []string{wantArgs, "endpoint=" + tt.wantEndpoint})
			}
		})
	}
}

func TestProviderOwnedReadyScopeReopensWithoutInitializationIntent(t *testing.T) {
	city := t.TempDir()
	logPath := filepath.Join(city, "provider-ops")
	script := filepath.Join(city, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycle(city, "", cfg, io.Discard); err != nil {
		t.Fatalf("startBeadsLifecycle: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, []string{"start", "start", "health"}) {
		t.Fatalf("provider operation order = %v, want reopen without init", got)
	}
}

func TestProviderOwnedReadyScopeIgnoresAmbientTransportSelectors(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(`{"backend":"dolt","dolt_mode":"server"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("gc.endpoint_origin: city_canonical\ndolt.host: external.example.test\ndolt.port: 4406\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "bd.log")
	bdPath := filepath.Join(city, "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "direct", Target: "external"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	t.Setenv("BD_BIN", bdPath)
	t.Setenv("GC_BEADS_TRANSPORT", "proxied")
	t.Setenv("GC_BEADS_TARGET", "local")
	t.Setenv("BEADS_DOLT_PROXIED_SERVER", "1")
	if err := runProviderOwnedScopeLifecycleOpContext(context.Background(), city, city, "stop"); err != nil {
		t.Fatalf("stop ready external direct scope: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("ambient selector made ready external direct scope control bd: %v", err)
	}
}

func TestProviderOwnedHealthAndRecoveryCoverEachOwnedRig(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "repo")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "provider-ops")
	failedOnce := filepath.Join(city, "rig-health-failed")
	script := filepath.Join(city, "provider.sh")
	contents := fmt.Sprintf(`#!/bin/sh
printf '%%s:%%s\n' "$1" "$BEADS_DIR" >> %q
if [ "$1" = health ] && [ "$BEADS_DIR" = %q ] && [ ! -e %q ]; then
  touch %q
  exit 1
fi
`, logPath, filepath.Join(rig, ".beads"), failedOnce, failedOnce)
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test\"\n[beads]\nprovider = \"exec:"+script+"\"\n[[rigs]]\nname = \"repo\"\npath = \"rigs/repo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{city, rig} {
		if err := persistProviderScopeOwnership(city, scope, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(city, scope); err != nil {
			t.Fatal(err)
		}
	}
	if err := healthBeadsProviderContext(context.Background(), city, false); err != nil {
		t.Fatalf("healthBeadsProviderContext: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	want := []string{
		"health:" + filepath.Join(city, ".beads"),
		"health:" + filepath.Join(rig, ".beads"),
		"recover:" + filepath.Join(city, ".beads"),
		"recover:" + filepath.Join(rig, ".beads"),
		"health:" + filepath.Join(city, ".beads"),
		"health:" + filepath.Join(rig, ".beads"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider operation order = %#v, want %#v", got, want)
	}
}

func TestProviderOwnedScopeInitScrubsPendingLocalEnvironmentAndPreservesExternalIdentity(t *testing.T) {
	for _, tt := range []struct {
		name       string
		intent     providerScopeIntent
		wantTarget string
		wantHost   string
		wantPort   string
		wantSocket string
		wantDB     string
	}{
		{name: "direct local", intent: providerScopeIntent{Transport: "direct", Target: "local"}, wantTarget: "local"},
		{name: "proxied local", intent: providerScopeIntent{Transport: "proxied", Target: "local"}, wantTarget: "local"},
		{name: "direct external", intent: providerScopeIntent{Transport: "direct", Target: "external"}, wantTarget: "external", wantHost: "db.example.test", wantPort: "4406", wantDB: "bd_external"},
		{name: "proxied external", intent: providerScopeIntent{Transport: "proxied", Target: "external"}, wantTarget: "external", wantHost: "db.example.test", wantPort: "4406", wantDB: "bd_external"},
		{name: "direct external unix", intent: providerScopeIntent{Transport: "direct", Target: "external"}, wantTarget: "external", wantSocket: "/var/run/dolt/external.sock", wantDB: "bd_external"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			logPath := filepath.Join(city, "provider-env")
			script := filepath.Join(city, "gc-beads-bd.sh")
			contents := "#!/bin/sh\nprintf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \"${GC_BEADS_TRANSPORT:-}\" \"${GC_BEADS_TARGET:-}\" \"${GC_DOLT_HOST:-}\" \"${GC_DOLT_PORT:-}\" \"${BEADS_DOLT_SERVER_HOST:-}\" \"${BEADS_DOLT_SERVER_PORT:-}\" \"${BEADS_DOLT_SERVER_SOCKET:-}\" \"${BEADS_DOLT_AUTO_START:-}\" \"${GC_DOLT_DATA_DIR:-}\" \"${GC_DOLT_LOG_FILE:-}\" \"${GC_DOLT_STATE_FILE:-}\" \"${GC_DOLT_PID_FILE:-}\" \"${GC_DOLT_LOCK_FILE:-}\" \"${GC_DOLT_CONFIG_FILE:-}\" \"${GC_DOLT_DATABASE:-}\" \"${GC_BIN:-}\" > \"$GC_TEST_PROVIDER_LOG\"\n"
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"pending-env\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
			for key, value := range map[string]string{
				envDoltHost: "poison-host", envDoltPort: "3333", "BEADS_DOLT_SERVER_HOST": "poison-bd-host", "BEADS_DOLT_SERVER_PORT": "3334", "BEADS_DOLT_SERVER_SOCKET": "/tmp/poison.sock", "BEADS_DOLT_AUTO_START": "0",
				"GC_DOLT_DATA_DIR": "/tmp/poison-data", "GC_DOLT_LOG_FILE": "/tmp/poison.log", "GC_DOLT_STATE_FILE": "/tmp/poison.state",
				"GC_DOLT_PID_FILE": "/tmp/poison.pid", "GC_DOLT_LOCK_FILE": "/tmp/poison.lock", "GC_DOLT_CONFIG_FILE": "/tmp/poison.yaml",
				"GC_BIN": "/tmp/keep-gc",
			} {
				t.Setenv(key, value)
			}
			if tt.intent.Target == "external" {
				if tt.wantSocket != "" {
					t.Setenv(envDoltHost, "")
					t.Setenv(envDoltPort, "")
					t.Setenv("BEADS_DOLT_SERVER_SOCKET", tt.wantSocket)
				} else {
					t.Setenv(envDoltHost, "db.example.test")
					t.Setenv(envDoltPort, "4406")
				}
				t.Setenv(envDoltDatabase, tt.wantDB)
			}
			if err := persistProviderScopeOwnership(city, city, tt.intent); err != nil {
				t.Fatal(err)
			}
			if _, err := runProviderOwnedScopeInit(city, city, "pending", script); err != nil {
				t.Fatalf("runProviderOwnedScopeInit: %v", err)
			}
			got, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Split(strings.TrimSpace(string(got)), "|")
			host, port := tt.wantHost, tt.wantPort
			if tt.intent.Transport == "proxied" || tt.wantSocket != "" {
				host, port = "", ""
			}
			want := []string{tt.intent.Transport, tt.wantTarget, host, port, host, port, tt.wantSocket, "", "", "", "", "", "", "", tt.wantDB, "/tmp/keep-gc"}
			if !reflect.DeepEqual(fields, want) {
				t.Fatalf("provider child env = %q, want %q", fields, want)
			}
		})
	}
}
