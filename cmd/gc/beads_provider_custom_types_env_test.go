package main

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestProviderOwnedCustomTypesEnvMatchesTheLifecycleProjection pins the env the
// types.custom registration runs under.
//
// It is the only registration a provider-owned scope gets — the owned branch of
// initAndHookDir writes no config.yaml — and it is best-effort, so a wrong
// binary produces no failure, just a scope whose every gc bead type fails
// validation. The `bd init` it follows ran through the workspace's pinned BD_BIN
// and the scope's proxied selectors; running the follow-up against whatever bd
// PATH resolves to would talk to a different binary than the one that created
// the store, and for a proxied scope a different one than owns the proxy.
func TestProviderOwnedCustomTypesEnvMatchesTheLifecycleProjection(t *testing.T) {
	city := t.TempDir()
	pinnedBd := filepath.Join(city, "pinned-bd")
	if err := os.WriteFile(pinnedBd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"pinned\"\n\n[workspace.env]\nBD_BIN = \"" + pinnedBd + "\"\n" +
		"[beads]\nprovider = \"exec:" + gcBeadsBdScriptPath(city) + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, city); err != nil {
		t.Fatal(err)
	}

	env, err := providerOwnedScopeCustomTypesEnv(city, city)
	if err != nil {
		t.Fatalf("providerOwnedScopeCustomTypesEnv: %v", err)
	}

	want, err := providerLifecycleProcessEnvForScopeInitWithError(city, city, beadsProvider(city))
	if err != nil {
		t.Fatalf("lifecycle env: %v", err)
	}
	wantMap := runtimeEnvEntriesToMap(want)
	// Everything the lifecycle projects, projected the same way. BEADS_DIR is
	// the scope's own and BD_BIN is the workspace pin, both asserted below;
	// export suppression is layered on top and is additive.
	overridden := map[string]bool{"BEADS_DIR": true, "BD_BIN": true}
	for key, value := range wantMap {
		if overridden[key] {
			continue
		}
		if got, ok := env[key]; !ok || got != value {
			t.Errorf("%s = %q (present=%t), want the lifecycle projection's %q", key, got, ok, value)
		}
	}
	if env["BEADS_DOLT_PROXIED_SERVER"] != "1" {
		t.Errorf("BEADS_DOLT_PROXIED_SERVER = %q, want the proxied scope's selector", env["BEADS_DOLT_PROXIED_SERVER"])
	}
	if env["BD_BIN"] != pinnedBd {
		t.Errorf("BD_BIN = %q, want the workspace pin %q", env["BD_BIN"], pinnedBd)
	}
	if env["BEADS_DIR"] != filepath.Join(city, ".beads") {
		t.Errorf("BEADS_DIR = %q, want the scope's own .beads", env["BEADS_DIR"])
	}
	if env["BD_EXPORT_AUTO"] == "" {
		t.Error("export suppression was dropped from the custom-types env")
	}
}

// TestRegisterProviderOwnedScopeCustomTypesRunsTheWorkspacePinnedBd drives the
// real registration rather than its env builder: a city.toml `[workspace.env]
// BD_BIN` pin is the supported versioned-pin shape, and the registration used to
// ignore it and run whatever `bd` PATH resolved to — against a store the pinned
// binary had just created.
func TestRegisterProviderOwnedScopeCustomTypesRunsTheWorkspacePinnedBd(t *testing.T) {
	city := t.TempDir()
	marker := filepath.Join(city, "pinned-bd-invoked")
	pinnedBd := filepath.Join(city, "pinned-bd")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + marker + "\n" +
		"if [ \"$2\" = get ]; then printf '{\"value\":\"\"}\\n'; fi\n" +
		"if [ \"$1\" = types ]; then printf '{\"core_types\":[]}\\n'; fi\nexit 0\n"
	if err := os.WriteFile(pinnedBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"pinned\"\n\n[workspace.env]\nBD_BIN = \"" + pinnedBd + "\"\n" +
		"[beads]\nprovider = \"exec:" + gcBeadsBdScriptPath(city) + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}

	registerProviderOwnedScopeCustomTypes(city, city)

	invocations, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the workspace-pinned bd was never invoked: %v", err)
	}
	for _, want := range []string{"config get --json types.custom", "config set types.custom"} {
		if !strings.Contains(string(invocations), want) {
			t.Errorf("pinned bd invocations = %q, want one containing %q", invocations, want)
		}
	}
}

// customTypesBdStub is a fake pinned bd for registerProviderOwnedScopeCustomTypes.
// It answers `config get --json types.custom` with row, `types --json` with
// types (an empty string makes the read fail), and fails `config set` when
// failSet is true. Every invocation is appended to the returned marker file.
type customTypesBdStub struct {
	row     string
	types   string
	failSet bool
}

func setupProviderOwnedCustomTypesScope(t *testing.T, stub customTypesBdStub) (city, marker string) {
	t.Helper()
	city = t.TempDir()
	marker = filepath.Join(city, "pinned-bd-invoked")
	pinnedBd := filepath.Join(city, "pinned-bd")
	rowFile := filepath.Join(city, "stub-row.json")
	typesFile := filepath.Join(city, "stub-types.json")
	if err := os.WriteFile(rowFile, []byte(`{"key":"types.custom","value":`+strconv.Quote(stub.row)+"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stub.types != "" {
		if err := os.WriteFile(typesFile, []byte(stub.types+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setExit := "0"
	if stub.failSet {
		setExit = "1"
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + marker + "\n" +
		"case \"$1 $2\" in\n" +
		"  'config get') cat " + rowFile + " ;;\n" +
		"  'types --json') cat " + typesFile + " || exit 1 ;;\n" +
		"  'config set') echo 'set refused' >&2; exit " + setExit + " ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(pinnedBd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"pinned\"\n\n[workspace.env]\nBD_BIN = \"" + pinnedBd + "\"\n" +
		"[beads]\nprovider = \"exec:" + gcBeadsBdScriptPath(city) + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	return city, marker
}

// customTypesStubSetValue returns the value the stub's `config set
// types.custom` received, or "" when no set was issued.
func customTypesStubSetValue(t *testing.T, marker string) string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the workspace-pinned bd was never invoked: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "config set types.custom "); ok {
			return v
		}
	}
	return ""
}

func captureStdLog(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// v142CustomTypes is the RequiredCustomTypes list gc v1.4.2 registered: every
// current required type except startup-health-episode.
var v142CustomTypes = []string{
	"molecule", "convoy", "message", "event", "gate",
	"merge-request", "agent", "role", "rig", "session", "spec",
	"convergence", "step",
}

// TestRegisterProviderOwnedScopeCustomTypesMergesAnExistingList is the #6495
// regression for provider-owned scopes: a scope migrated from a legacy city (or
// registered by an older gc) already carries a types list, and the old
// "write only when unset" rule left startup-health-episode unregistered on it
// forever. The write must add the missing type while keeping every entry the
// row and the custom_types table already accept.
func TestRegisterProviderOwnedScopeCustomTypesMergesAnExistingList(t *testing.T) {
	row := strings.Join(append(append([]string{}, v142CustomTypes...), "ops-extra"), ",")
	types := `{"core_types":[],"custom_types":["` + strings.Join(append(append([]string{}, v142CustomTypes...), "ops-extra", "table-only"), `","`) + `"]}`
	city, marker := setupProviderOwnedCustomTypesScope(t, customTypesBdStub{row: row, types: types})

	registerProviderOwnedScopeCustomTypes(city, city)

	got := customTypesStubSetValue(t, marker)
	if got == "" {
		t.Fatalf("no `bd config set types.custom` was issued for a scope missing startup-health-episode")
	}
	want := strings.Join(append(append([]string{}, v142CustomTypes...), "ops-extra", "table-only", "startup-health-episode"), ",")
	if got != want {
		t.Fatalf("types.custom set to %q, want the row ∪ table ∪ required merge %q", got, want)
	}
}

// TestRegisterProviderOwnedScopeCustomTypesHealsATableOnlyGap covers the
// upgraded shape where the row is already complete and only the custom_types
// table (what bd validates against) lacks the new type.
func TestRegisterProviderOwnedScopeCustomTypesHealsATableOnlyGap(t *testing.T) {
	row := strings.Join(doctor.RequiredCustomTypes, ",")
	types := `{"custom_types":["` + strings.Join(v142CustomTypes, `","`) + `"]}`
	city, marker := setupProviderOwnedCustomTypesScope(t, customTypesBdStub{row: row, types: types})

	registerProviderOwnedScopeCustomTypes(city, city)

	if got, want := customTypesStubSetValue(t, marker), row; got != want {
		t.Fatalf("types.custom set to %q, want %q", got, want)
	}
}

// TestRegisterProviderOwnedScopeCustomTypesSkipsACompleteScope pins the cost of
// the every-start path: a registered scope gets two reads and no write.
func TestRegisterProviderOwnedScopeCustomTypesSkipsACompleteScope(t *testing.T) {
	all := append(append([]string{}, doctor.RequiredCustomTypes...), "ops-extra")
	types := `{"custom_types":["` + strings.Join(all, `","`) + `"]}`
	city, marker := setupProviderOwnedCustomTypesScope(t, customTypesBdStub{row: strings.Join(all, ","), types: types})

	registerProviderOwnedScopeCustomTypes(city, city)

	if got := customTypesStubSetValue(t, marker); got != "" {
		t.Fatalf("complete scope was rewritten with %q; want no write", got)
	}
}

// TestRegisterProviderOwnedScopeCustomTypesTableReadFailureWritesNothing: a
// `bd config set` replaces the custom_types table wholesale, so writing a list
// built without the table could delete table-only types. An unreadable table
// must skip the write, log the doctor hint, and not fail start.
func TestRegisterProviderOwnedScopeCustomTypesTableReadFailureWritesNothing(t *testing.T) {
	city, marker := setupProviderOwnedCustomTypesScope(t, customTypesBdStub{row: strings.Join(v142CustomTypes, ",")})
	logs := captureStdLog(t)

	registerProviderOwnedScopeCustomTypes(city, city)

	if got := customTypesStubSetValue(t, marker); got != "" {
		t.Fatalf("types.custom was written as %q without reading the custom_types table", got)
	}
	if !strings.Contains(logs.String(), "gc doctor --fix") {
		t.Fatalf("log = %q, want the `gc doctor --fix` hint", logs.String())
	}
}

// TestRegisterProviderOwnedScopeCustomTypesSetFailureIsBestEffort: a refused
// write is logged with the doctor hint and does not escape (start continues).
func TestRegisterProviderOwnedScopeCustomTypesSetFailureIsBestEffort(t *testing.T) {
	city, marker := setupProviderOwnedCustomTypesScope(t, customTypesBdStub{
		row:     strings.Join(v142CustomTypes, ","),
		types:   `{"custom_types":["` + strings.Join(v142CustomTypes, `","`) + `"]}`,
		failSet: true,
	})
	logs := captureStdLog(t)

	registerProviderOwnedScopeCustomTypes(city, city)

	if got := customTypesStubSetValue(t, marker); got == "" {
		t.Fatal("expected a `bd config set types.custom` attempt")
	}
	if !strings.Contains(logs.String(), "gc doctor --fix") {
		t.Fatalf("log = %q, want the `gc doctor --fix` hint", logs.String())
	}
}

// TestInitAndHookDirNeverWritesCustomTypesToABoundScope: a scope bound to a
// store gc does not own (a complete storage binding) gets hooks and nothing
// else. The provider-owned registration must never reach it -- gc does not
// write types into someone else's database; the release note sends operators
// to `gc doctor` for those scopes instead.
func TestInitAndHookDirNeverWritesCustomTypesToABoundScope(t *testing.T) {
	clearInheritedBeadsEnv(t)
	city := t.TempDir()
	marker := filepath.Join(city, "calls")
	stub := filepath.Join(city, "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"bound\"\n\n[workspace.env]\nBD_BIN = \"" + stub + "\"\n" +
		"[beads]\nprovider = \"exec:" + stub + "\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	binding := `{"database":"beads","backend":"mysql","storage_endpoint":"opaque-remote","storage_database":"someone_elses"}`
	if err := os.WriteFile(filepath.Join(city, ".beads", "metadata.json"), []byte(binding), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := initAndHookDir(city, city, "gc"); err != nil {
		t.Fatalf("initAndHookDir(bound scope): %v", err)
	}
	calls, _ := os.ReadFile(marker)
	for _, forbidden := range []string{"config set", "types.custom", "custom_types"} {
		if strings.Contains(string(calls), forbidden) {
			t.Fatalf("bound scope received a custom-types write (%q); calls:\n%s", forbidden, calls)
		}
	}
}
