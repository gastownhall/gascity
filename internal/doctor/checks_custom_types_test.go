package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCustomTypesCheck_NoBeadsDir(t *testing.T) {
	dir := t.TempDir()
	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (no .beads dir)", r.Status)
	}
}

func TestCustomTypesCheck_MissingTypes(t *testing.T) {
	// Scrub inherited beads env so the `bd config get` subprocess below
	// resolves to the empty .beads/ in the temp dir instead of an outer
	// gc city's beads database. Without this, bd can reach a live dolt
	// server (via GC_BEADS=bd + BEADS_DOLT_SERVER_PORT), reports all
	// required types as present, and the check returns StatusOK —
	// defeating the assertion. Clearing GC_BEADS and the dolt connection
	// vars prevents bd from connecting even if testenv.init() has not
	// yet added them to its LeakVectorVars scrub list.
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}

	// Scrubbing env vars alone is not enough: bd's config precedence falls
	// through to $HOME/.beads/config.yaml as a last resort, so a machine
	// HOME with dolt.shared-server: true still routes bd to the shared
	// server — which answers with every required type present and turns
	// this check StatusOK, defeating the assertion below. Pin a test-owned
	// HOME so that fallback file doesn't exist. See ga-zxpfic and
	// TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext.
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewCustomTypesCheck(dir, "test")
	// This will fail because bd isn't initialized in the temp dir.
	// The check should report a warning (can't read config).
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status == StatusOK {
		t.Fatal("expected non-OK status when bd config fails")
	}
	if !c.CanFix() {
		t.Fatal("CanFix should return true")
	}
}

// retryRemoveAllForTest retries os.RemoveAll briefly to absorb a lingering
// embedded-dolt background writer that can hold files open a few dozen ms
// past the owning bd subprocess's apparent exit — which otherwise races
// t.TempDir()'s single-shot RemoveAll cleanup with an intermittent
// "directory not empty" error. Falls through silently on final failure so
// TempDir's own best-effort cleanup still gets the last word.
func retryRemoveAllForTest(t *testing.T, dir string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func customTypesTestEnv(base []string, host, port string) []string {
	overrides := map[string]string{
		"GC_DOLT_HOST":                  host,
		"GC_DOLT_PORT":                  port,
		"GC_DOLT_USER":                  "",
		"GC_DOLT_PASSWORD":              "",
		"BEADS_DOLT_SERVER_HOST":        host,
		"BEADS_DOLT_SERVER_PORT":        port,
		"BEADS_DOLT_SERVER_USER":        "",
		"BEADS_DOLT_PASSWORD":           "",
		"BEADS_DOLT_SERVER_TLS":         "",
		"BEADS_DOLT_CREDENTIAL_COMMAND": "",
		"BEADS_DOLT_AUTO_START":         "0",
		"BD_BACKUP_ENABLED":             "false",
		"BEADS_BACKUP_ENABLED":          "false",
		"BD_DOLT_SYNC_CLI_REMOTES":      "false",
		"BEADS_DOLT_SYNC_CLI_REMOTES":   "false",
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			out = append(out, entry)
		}
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func customTypesTestServerPort(t testing.TB, initOutput string) string {
	t.Helper()
	for _, line := range strings.Split(initOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Server:") {
			continue
		}
		_, port, ok := strings.Cut(line, "127.0.0.1:")
		if !ok {
			continue
		}
		port = strings.TrimSpace(port)
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
			return port
		}
	}
	t.Fatalf("bd init output has no loopback server port:\n%s", initOutput)
	return ""
}

func setCustomTypesTestEndpoint(t *testing.T, host, port string) {
	t.Helper()
	t.Setenv("GC_DOLT_HOST", host)
	t.Setenv("GC_DOLT_PORT", port)
	t.Setenv("BEADS_DOLT_SERVER_HOST", host)
	t.Setenv("BEADS_DOLT_SERVER_PORT", port)
	t.Setenv("BEADS_DOLT_AUTO_START", "0")
}

func TestCustomTypesStoreEnvClearsAmbientAuthorityAndPreservesCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}

	authority := map[string]string{
		"BEADS_BACKEND":              "sqlite",
		"BEADS_DB":                   "/poison/beads.db",
		"BEADS_DB_PATH":              "/poison/beads.db",
		"BEADS_DOLT_DATABASE":        "wrong-db",
		"BEADS_DOLT_DATA_DIR":        "/poison/dolt",
		"BEADS_DOLT_PORT":            "9999",
		"BEADS_DOLT_SERVER_DATABASE": "wrong-db",
		"BEADS_DOLT_SERVER_MODE":     "1",
		"BEADS_DOLT_SERVER_SOCKET":   "/poison/dolt.sock",
		"BEADS_DOLT_SHARED_SERVER":   "1",
		"BEADS_SHARED_SERVER_DIR":    "/poison/shared",
		"DOLT_ROOT_PATH":             "/poison/root",
		"GC_BEADS":                   "file",
		"GC_BEADS_BACKEND":           "doltlite",
		"GC_BEADS_PREFIX":            "poison",
		"GC_DOLT_DATABASE":           "wrong-db",
	}
	for key, value := range authority {
		t.Setenv(key, value)
	}
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")

	credentials := map[string]string{
		"BEADS_CREDENTIALS_FILE":        "/secure/credentials.json",
		"BEADS_DOLT_CREDENTIAL_COMMAND": "credential-helper",
		"BEADS_DOLT_PASSWORD":           "beads-password",
		"GC_DOLT_CRED_CMD":              "gc-credential-helper",
		"GC_DOLT_PASSWORD":              "gc-password",
	}
	for key, value := range credentials {
		t.Setenv(key, value)
	}

	environ, err := customTypesStoreEnv(&CheckContext{CityPath: dir}, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}

	if got["BEADS_DIR"] != filepath.Join(dir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want scoped store", got["BEADS_DIR"])
	}
	for key := range authority {
		if got[key] != "" {
			t.Errorf("%s was not cleared", key)
		}
	}
	if got["BEADS_DOLT_SERVER_TLS"] != "" {
		t.Error("BEADS_DOLT_SERVER_TLS was not cleared for an embedded store")
	}
	for key, want := range credentials {
		if got[key] != want {
			t.Errorf("%s was not preserved", key)
		}
	}
}

// TestCustomTypesCheck_TableDrift proves detect+heal of the bug this bead
// fixes: config.yaml's types.custom CSV can list a type (e.g. "step") that
// the normalized custom_types TABLE doesn't have a row for. bd's create
// validation reads the TABLE, not the CSV, so a store in this state rejects
// `bd create --type step ...` with "invalid issue type: step" even though
// `bd config get types.custom` reports the type present. This drift happens
// on stores an older bd wrote (or where the table row was dropped some
// other way) — bd itself keeps CSV and table in sync on `bd config set`,
// but nothing previously re-ran that set for existing stores.
//
// The test manufactures the drift directly (delete the table row via the
// dolt CLI) rather than depending on an old bd binary, then asserts Run
// catches it — even though the CSV alone is complete — and Fix heals it by
// re-running `bd config set types.custom <merged>`, which reinserts the
// missing table row as a side effect of bd's own set-path.
func TestCustomTypesCheck_TableDrift(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}

	// Scrub inherited beads env so the bd subprocesses below resolve to the
	// throwaway store in the temp dir instead of an outer gc city's beads
	// database. See TestCustomTypesCheck_MissingTypes for why each var
	// matters.
	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}

	// Scrubbing env vars alone is not enough: bd's config precedence falls
	// through to $HOME/.beads/config.yaml as a last resort, so on a fleet
	// agent HOME with dolt.shared-server: true set there, bd still routes
	// to the shared server regardless of the vars above. Pin a test-owned
	// HOME so that fallback file doesn't exist. See ga-zxpfic and
	// TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	t.Cleanup(func() { retryRemoveAllForTest(t, dir) })

	runBD := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bd", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	runBD("init", "--non-interactive", "-p", "tst", "--skip-hooks", "--skip-agents")
	runBD("config", "set", "types.custom", strings.Join(RequiredCustomTypes, ","))

	// Locate the embedded dolt DB directory the same way production code
	// does (internal/beads.(*BdStore).embeddedDoltDir), rather than
	// hand-deriving the sanitized database name from the prefix.
	metadataPath := filepath.Join(dir, ".beads", "metadata.json")
	dbName, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		t.Fatalf("ReadDoltDatabase(%s): ok=%v err=%v", metadataPath, ok, err)
	}
	doltDir := filepath.Join(dir, ".beads", "embeddeddolt", dbName)

	// Manufacture table drift: delete the "step" row directly from the
	// custom_types table while leaving config.yaml's CSV untouched.
	deleteCmd := exec.Command("dolt", "sql", "-q", "delete from custom_types where name='step'")
	deleteCmd.Dir = doltDir
	if out, err := deleteCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt sql delete: %v\n%s", err, out)
	}

	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("Run status = %v, want StatusError (table drift); message=%q", r.Status, r.Message)
	}
	if len(c.missing) != 0 {
		t.Fatalf("c.missing = %v, want empty — the CSV is complete, only the table is drifted", c.missing)
	}
	if !slices.Contains(c.tableMissing, "step") {
		t.Fatalf("c.tableMissing = %v, want it to contain %q", c.tableMissing, "step")
	}

	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}

	c2 := NewCustomTypesCheck(dir, "test")
	r2 := c2.Run(&CheckContext{CityPath: dir})
	if r2.Status != StatusOK {
		t.Fatalf("after Fix, Run status = %v, want StatusOK; message=%q", r2.Status, r2.Message)
	}

	out := runBD("create", "--type", "step", "drift healed check")
	if !strings.Contains(out, "Created issue") {
		t.Fatalf("bd create --type step failed after Fix, table still drifted: %s", out)
	}
}

// TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext is a regression
// test for ga-zxpfic: env-var scrubbing alone does not stop a machine-level
// dolt.shared-server config from leaking into the bd subprocesses this
// package's tests spawn. bd's config precedence falls through, as a last
// resort, to $HOME/.beads/config.yaml — so on any HOME that has
// dolt.shared-server: true set there (as fleet agent HOMEs do), scrubbing
// BEADS_DOLT_SERVER_PORT and friends changes nothing: bd still discovers the
// shared server via that config file, not an env var. Pinning a test-owned
// HOME via t.TempDir() removes the fallback file entirely, which is the only
// complete fix — this test asserts that isolation actually holds, not just
// that the drift check's Run/Fix behavior happens to look right.
func TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}

	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT",
		"GC_BEADS", "BEADS_DOLT_SERVER_PORT", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SHARED_SERVER",
		"BEADS_DOLT_SERVER_MODE", "BEADS_SHARED_SERVER_DIR",
	} {
		t.Setenv(key, "")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	t.Cleanup(func() { retryRemoveAllForTest(t, dir) })

	runBD := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("bd", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	initOut := runBD("init", "--non-interactive", "-p", "tst2", "--skip-hooks", "--skip-agents")
	setOut := runBD("config", "set", "types.custom", strings.Join(RequiredCustomTypes, ","))

	homeConfigPath := filepath.Join(home, ".beads", "config.yaml")
	if _, err := os.Stat(homeConfigPath); !os.IsNotExist(err) {
		t.Fatalf("expected no config.yaml under test-owned HOME %s, but Stat returned err=%v", homeConfigPath, err)
	}

	metadataPath := filepath.Join(dir, ".beads", "metadata.json")
	meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState(%s): ok=%v err=%v", metadataPath, ok, err)
	}
	if meta.DoltMode != "embedded" {
		t.Fatalf("metadata.json dolt_mode = %q, want %q", meta.DoltMode, "embedded")
	}
	if meta.DoltDatabase == "" {
		t.Fatal("metadata.json dolt_database is empty, want it to match the embedded store")
	}

	for _, out := range []string{initOut, setOut} {
		if strings.Contains(out, "Dolt server at") {
			t.Fatalf("bd output leaked a shared-server connection: %s", out)
		}
		if strings.Contains(out, "shared-server mode is enabled") {
			t.Fatalf("bd output leaked shared-server mode: %s", out)
		}
	}
}

// TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint protects the
// store-selection boundary for gc doctor. The intended scope records a real
// loopback Dolt server, while the process environment points bd at a second,
// unrelated server. Run and Fix must use the recorded target: repair only the
// intended store, preserve its user-defined type, and leave the ambient store
// unchanged.
func TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}

	for _, key := range []string{
		"BEADS_DIR", "BEADS_ACTOR", "GC_BEADS_SCOPE_ROOT", "GC_BEADS",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER", "BEADS_DOLT_SERVER_TLS",
		"BEADS_DOLT_PASSWORD", "BEADS_DOLT_CREDENTIAL_COMMAND",
		"BEADS_DOLT_SHARED_SERVER", "BEADS_DOLT_SERVER_MODE",
		"BEADS_SHARED_SERVER_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"GC_DOLT_USER", "GC_DOLT_PASSWORD",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BD_BACKUP_ENABLED", "false")
	t.Setenv("BEADS_BACKUP_ENABLED", "false")

	runBD := func(dir string, env []string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("bd", args...)
		cmd.Dir = dir
		if env != nil {
			cmd.Env = env
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	mustRunBD := func(dir string, env []string, args ...string) string {
		t.Helper()
		out, err := runBD(dir, env, args...)
		if err != nil {
			t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}

	targetDir := t.TempDir()
	decoyDir := t.TempDir()
	var targetPort, decoyPort string
	t.Cleanup(func() {
		for _, store := range []struct {
			dir  string
			port string
		}{
			{targetDir, targetPort},
			{decoyDir, decoyPort},
		} {
			env := os.Environ()
			if store.port != "" {
				env = customTypesTestEnv(env, "127.0.0.1", store.port)
			}
			_, _ = runBD(store.dir, env, "dolt", "stop")
			retryRemoveAllForTest(t, store.dir)
		}
	})

	targetInit := mustRunBD(targetDir, nil,
		"init", "--server", "--server-port", "0", "--non-interactive",
		"-p", "target", "--skip-hooks", "--skip-agents")
	targetPort = customTypesTestServerPort(t, targetInit)
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(targetDir, ".beads", "config.yaml"), contract.ConfigState{
		IssuePrefix:    "target",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       targetPort,
	}); err != nil {
		t.Fatal(err)
	}
	targetEnv := customTypesTestEnv(os.Environ(), "127.0.0.1", targetPort)
	mustRunBD(targetDir, targetEnv, "config", "set", "types.custom", "user-defined")

	decoyInit := mustRunBD(decoyDir, nil,
		"init", "--server", "--server-port", "0", "--non-interactive",
		"-p", "decoy", "--skip-hooks", "--skip-agents")
	decoyPort = customTypesTestServerPort(t, decoyInit)
	decoyEnv := customTypesTestEnv(os.Environ(), "127.0.0.1", decoyPort)
	mustRunBD(decoyDir, decoyEnv, "config", "set", "types.custom", "decoy-only")

	setCustomTypesTestEndpoint(t, "127.0.0.1", decoyPort)
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
	t.Setenv("BEADS_BACKEND", "doltlite")
	t.Setenv("BEADS_DB", filepath.Join(decoyDir, "poison.db"))
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(decoyDir, "poison-dolt"))
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrong-db")
	t.Setenv("BEADS_DOLT_SERVER_MODE", "1")
	t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
	t.Setenv("GC_BEADS_BACKEND", "doltlite")
	t.Setenv("GC_DOLT_DATABASE", "wrong-db")
	c := NewCustomTypesCheck(targetDir, "target")
	ctx := &CheckContext{CityPath: targetDir}
	if result := c.Run(ctx); result.Status != StatusError {
		t.Fatalf("Run status = %v, want StatusError for missing required target types; message=%q", result.Status, result.Message)
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result := NewCustomTypesCheck(targetDir, "target").Run(ctx); result.Status != StatusOK {
		t.Fatalf("Run after Fix status = %v, want StatusOK; message=%q", result.Status, result.Message)
	}

	targetOut := mustRunBD(targetDir, targetEnv, "config", "get", "--json", "types.custom")
	targetTypes, err := parseCustomTypesJSON([]byte(targetOut))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(targetTypes, "user-defined") {
		t.Fatalf("target types = %v, want preserved user-defined type", targetTypes)
	}
	if missing := typesNotIn(RequiredCustomTypes, targetTypes); len(missing) != 0 {
		t.Fatalf("target types still missing required entries: %v", missing)
	}

	decoyOut := mustRunBD(decoyDir, decoyEnv, "config", "get", "--json", "types.custom")
	decoyTypes, err := parseCustomTypesJSON([]byte(decoyOut))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoyTypes, []string{"decoy-only"}) {
		t.Fatalf("ambient decoy types = %v, want unchanged [decoy-only]", decoyTypes)
	}
}

func TestCustomTypesCheck_RequiredTypesIncludeSpec(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "spec" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'spec'")
	}
}

// TestCustomTypesCheck_RequiredTypesIncludeConvergence verifies that
// "convergence" is in the required list. gc's convergence handler
// (internal/convergence/create.go) creates beads with Type="convergence"
// on every `gc converge create` call; if the type isn't registered in
// bd's types.custom, every convergence loop fails at creation with
// "invalid issue type: convergence".
func TestCustomTypesCheck_RequiredTypesIncludeConvergence(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "convergence" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'convergence' — gc's convergence handler requires this type")
	}
}

// TestMergeCustomTypes exercises the merge/dedup/preservation logic that
// backs CustomTypesCheck.Fix(). The regression it guards against is
// `--fix` overwriting user-defined types (which was the pre-PR behavior
// and still the failure mode if the merge is ever reverted).
func TestMergeCustomTypes(t *testing.T) {
	cases := []struct {
		name     string
		current  []string
		required []string
		want     []string
	}{
		{
			name:     "empty current gets required only",
			current:  nil,
			required: []string{"a", "b"},
			want:     []string{"a", "b"},
		},
		{
			name:     "preserves extra user types and appends missing required",
			current:  []string{"custom-foo", "molecule"},
			required: []string{"molecule", "spec", "convergence"},
			want:     []string{"custom-foo", "molecule", "spec", "convergence"},
		},
		{
			name:     "dedupes duplicates in current",
			current:  []string{"a", "a", "b", "a"},
			required: []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "drops empty and whitespace-only entries",
			current:  []string{"a", "", "  ", "b"},
			required: []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "trims whitespace around entries",
			current:  []string{" a ", "b\t"},
			required: []string{"a", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "dedupes when required entry already in current",
			current:  []string{"a", "b", "c"},
			required: []string{"b", "c", "d"},
			want:     []string{"a", "b", "c", "d"},
		},
		{
			name:     "preserves order of current entries",
			current:  []string{"z", "y", "x"},
			required: []string{"a"},
			want:     []string{"z", "y", "x", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contract.MergeCustomTypes(tc.current, tc.required)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("contract.MergeCustomTypes(%v, %v) = %v, want %v",
					tc.current, tc.required, got, tc.want)
			}
		})
	}
}

// TestParseCustomTypesJSON guards against the regression where
// `bd config get types.custom` on a store with an unset key returns
// "types.custom (not set)" and the old parser would persist that
// string as a fake custom type when Fix() merges. Switching to
// --json (+ this parser) eliminates the sentinel.
func TestParseCustomTypesJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "unset key returns nil",
			input: `{"key":"types.custom","value":""}`,
			want:  nil,
		},
		{
			name:  "whitespace-only value returns nil",
			input: `{"key":"types.custom","value":"   "}`,
			want:  nil,
		},
		{
			name:  "populated value splits on comma",
			input: `{"key":"types.custom","value":"molecule,spec,convergence"}`,
			want:  []string{"molecule", "spec", "convergence"},
		},
		{
			name:    "malformed JSON errors",
			input:   `not json`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCustomTypesJSON([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseCustomTypesJSON(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestTypesNotIn exercises the set-difference helper shared by the CSV
// completeness check and the custom_types table drift check.
func TestTypesNotIn(t *testing.T) {
	cases := []struct {
		name string
		want []string
		have []string
		out  []string
	}{
		{
			name: "nothing missing",
			want: []string{"a", "b"},
			have: []string{"a", "b", "c"},
			out:  nil,
		},
		{
			name: "some missing, preserves want order",
			want: []string{"a", "b", "c"},
			have: []string{"b"},
			out:  []string{"a", "c"},
		},
		{
			name: "everything missing when have is empty",
			want: []string{"a", "b"},
			have: nil,
			out:  []string{"a", "b"},
		},
		{
			name: "trims whitespace before comparing",
			want: []string{"a"},
			have: []string{" a "},
			out:  nil,
		},
		{
			name: "empty want yields nil regardless of have",
			want: nil,
			have: []string{"a"},
			out:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := typesNotIn(tc.want, tc.have)
			if !reflect.DeepEqual(got, tc.out) {
				t.Errorf("typesNotIn(%v, %v) = %v, want %v", tc.want, tc.have, got, tc.out)
			}
		})
	}
}

func TestCustomTypesCheck_RequiredTypesComplete(t *testing.T) {
	expected := map[string]bool{
		"molecule": true, "convoy": true, "message": true,
		"event": true, "gate": true, "merge-request": true,
		"agent": true, "role": true, "rig": true,
		"session": true, "spec": true, "convergence": true,
		"step": true,
	}
	for _, typ := range RequiredCustomTypes {
		if !expected[typ] {
			t.Errorf("unexpected required type: %q", typ)
		}
		delete(expected, typ)
	}
	for typ := range expected {
		t.Errorf("missing required type: %q", typ)
	}
}
