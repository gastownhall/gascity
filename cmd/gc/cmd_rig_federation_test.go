package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// writeRigFederationCity writes a city with an unguarded rig "alpha" and a
// guarded, exclusive rig "beta", each in its own directory.
func writeRigFederationCity(t *testing.T) (cityPath, alphaPath, betaPath string) {
	t.Helper()
	cityPath = t.TempDir()
	alphaPath = filepath.Join(t.TempDir(), "alpha")
	betaPath = filepath.Join(t.TempDir(), "beta")
	for _, d := range []string{filepath.Join(cityPath, ".gc"), alphaPath, betaPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "coordinator"

[[rigs]]
name = "alpha"
path = "` + alphaPath + `"

[[rigs]]
name = "beta"
path = "` + betaPath + `"
federation = "rig"
namespace = "exclusive"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath, alphaPath, betaPath
}

func rigListRowsByName(t *testing.T, out []byte) map[string]map[string]any {
	t.Helper()
	var payload struct {
		Rigs []map[string]any `json:"rigs"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode rig list JSON: %v\n%s", err, out)
	}
	rows := make(map[string]map[string]any, len(payload.Rigs))
	for _, row := range payload.Rigs {
		name, _ := row["name"].(string)
		rows[name] = row
	}
	return rows
}

func assertRigListFederationRows(t *testing.T, out []byte) {
	t.Helper()
	rows := rigListRowsByName(t, out)
	beta, ok := rows["beta"]
	if !ok {
		t.Fatalf("rig beta missing from output:\n%s", out)
	}
	if beta["federation"] != "rig" || beta["namespace"] != "exclusive" {
		t.Fatalf("beta row = %#v, want federation rig, namespace exclusive", beta)
	}
	alpha, ok := rows["alpha"]
	if !ok {
		t.Fatalf("rig alpha missing from output:\n%s", out)
	}
	for _, key := range []string{"federation", "namespace"} {
		if _, present := alpha[key]; present {
			t.Fatalf("default rig alpha must omit %q: %#v", key, alpha)
		}
		if _, present := rows["test-city"][key]; present {
			t.Fatalf("HQ row must omit %q: %#v", key, rows["test-city"])
		}
	}
}

func TestDoRigListJSONShowsRigFederation(t *testing.T) {
	cityPath, _, _ := writeRigFederationCity(t)
	orig := rigListSessionProvider
	rigListSessionProvider = func() (runtime.Provider, error) { return &fakeAdoptionProvider{}, nil }
	t.Cleanup(func() { rigListSessionProvider = orig })

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}
	assertRigListFederationRows(t, stdout.Bytes())
	validateJSONResultSchema(t, []string{"rig", "list"}, stdout.Bytes())
}

func TestDoRigListTextShowsRigFederation(t *testing.T) {
	cityPath, _, _ := writeRigFederationCity(t)

	var stdout, stderr bytes.Buffer
	if code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "Federation:"); got != 1 {
		t.Fatalf("want exactly one Federation line (rig beta), got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "    Federation: rig (namespace: exclusive) (not enforced by this build)\n") {
		t.Fatalf("missing beta federation line:\n%s", out)
	}
	betaIdx := strings.Index(out, "  beta:")
	fedIdx := strings.Index(out, "Federation:")
	if betaIdx < 0 || fedIdx < betaIdx {
		t.Fatalf("Federation line must belong to rig beta:\n%s", out)
	}
}

func TestRigListFederationOmitsDefaults(t *testing.T) {
	cases := []struct {
		federation, namespace string
		wantFed, wantNS       string
	}{
		{"", "", "", ""},
		{"city", "shared", "", ""},
		{"rig", "", "rig", ""},
		{"rig", "shared", "rig", ""},
		{"rig", "exclusive", "rig", "exclusive"},
	}
	for _, tc := range cases {
		r := config.Rig{Name: "beta", Federation: tc.federation, Namespace: tc.namespace}
		fed, ns := rigListFederation(&r)
		if fed != tc.wantFed || ns != tc.wantNS {
			t.Errorf("rigListFederation(%q, %q) = (%q, %q), want (%q, %q)",
				tc.federation, tc.namespace, fed, ns, tc.wantFed, tc.wantNS)
		}
	}
}

func TestRenderRigListFromAPIShowsRigFederation(t *testing.T) {
	cityPath, alphaPath, betaPath := writeRigFederationCity(t)
	prevHQRunning := rigListHQRunning
	rigListHQRunning = func(string) bool { return true }
	t.Cleanup(func() { rigListHQRunning = prevHQRunning })

	cr := api.CachedRead[[]api.RigView]{
		Body: []api.RigView{
			{Name: "alpha", Path: alphaPath, Prefix: "al"},
			{Name: "beta", Path: betaPath, Prefix: "be", RunningCount: 1},
		},
		AgeSeconds: 2,
	}
	var stdout, stderr bytes.Buffer
	if code := renderRigListFromAPI(fsys.OSFS{}, cityPath, cr, true, &stdout, &stderr); code != 0 {
		t.Fatalf("renderRigListFromAPI returned %d, stderr: %s", code, stderr.String())
	}
	assertRigListFederationRows(t, stdout.Bytes())
	validateJSONResultSchema(t, []string{"rig", "list"}, stdout.Bytes())

	stdout.Reset()
	stderr.Reset()
	if code := renderRigListFromAPI(fsys.OSFS{}, cityPath, cr, false, &stdout, &stderr); code != 0 {
		t.Fatalf("renderRigListFromAPI (text) returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "    Federation: rig (namespace: exclusive) (not enforced by this build)\n") {
		t.Fatalf("API text output missing beta federation line:\n%s", stdout.String())
	}
}

// TestRigListRouteEquivalenceWithRigFederation runs gc rig list --json on
// the local route and through the supervisor API for a city with a guarded,
// exclusive rig and an unguarded one: both routes print federation and
// namespace for the guarded rig only, and both validate against the
// schemas/rig/list result schema.
func TestRigListRouteEquivalenceWithRigFederation(t *testing.T) {
	clearInheritedBeadsEnv(t)
	prevHQRunning := rigListHQRunning
	rigListHQRunning = func(string) bool { return true }
	t.Cleanup(func() { rigListHQRunning = prevHQRunning })

	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")
	for _, dir := range []string{alpha, beta} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityToml := charCityBasic + fmt.Sprintf(`
[[rigs]]
name = "alpha"
path = %q
prefix = "al"

[[rigs]]
name = "beta"
path = %q
prefix = "be"
federation = "rig"
namespace = "exclusive"
`, alpha, beta)
	h := newCharCity(t, cityToml, nil)
	client := registryReadsAPIClient(t, h)

	local, viaAPI := runBothRoutes(t, "rig list", client, func(c *api.Client, nilReason string, stdout, stderr io.Writer) int {
		return routeRigList(h.cityPath, c, nilReason, true, stdout, stderr)
	})
	assertRouteEquivalentJSONModulo(t, []string{"rig", "list"}, local, viaAPI, []string{"_cache_age_s"})
	for route, out := range map[string][]byte{"local": local, "api": viaAPI} {
		var got RigListJSON
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s route: decode rig list: %v\n%s", route, err, out)
		}
		rows := map[string]RigListItem{}
		for _, r := range got.Rigs {
			rows[r.Name] = r
		}
		if b := rows["beta"]; b.Federation != config.RigFederationRig || b.Namespace != "exclusive" {
			t.Errorf("%s route: beta federation/namespace = %q/%q, want rig/exclusive\n%s", route, b.Federation, b.Namespace, out)
		}
		if a := rows["alpha"]; a.Federation != "" || a.Namespace != "" {
			t.Errorf("%s route: alpha federation/namespace = %q/%q, want both omitted\n%s", route, a.Federation, a.Namespace, out)
		}
	}
}

func TestConfigShowValidateWarnsThatRigFederationIsNotEnforced(t *testing.T) {
	clearGCEnv(t)
	cityPath, _, _ := writeRigFederationCity(t)
	t.Chdir(cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)

	var stdout, stderr bytes.Buffer
	code := run([]string{"config", "show", "--validate", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(config show --validate --json) = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var payload struct {
		Validation struct {
			OK       bool     `json:"ok"`
			Warnings []string `json:"warnings"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	found := false
	for _, w := range payload.Validation.Warnings {
		if strings.Contains(w, `rig "beta"`) && strings.Contains(w, "does not enforce") && strings.Contains(w, "NOT protected") {
			found = true
		}
	}
	if !payload.Validation.OK || !found {
		t.Fatalf("validation = %+v, want ok with the not-enforced warning for rig beta", payload.Validation)
	}
	validateConfigShowJSONSchema(t, stdout.Bytes())
}

func TestConfigShowValidateRefusesGuardedRigSharingADirectory(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	shared := filepath.Join(t.TempDir(), "shared")
	for _, d := range []string{filepath.Join(cityPath, ".gc"), shared} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCityToml(t, cityPath, `[workspace]
name = "test-city"

[[rigs]]
name = "alpha"
path = "`+shared+`"
prefix = "al"

[[rigs]]
name = "beta"
path = "`+shared+`"
prefix = "be"
federation = "rig"
`)
	t.Chdir(cityPath)
	t.Setenv("GC_CITY_PATH", cityPath)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "show", "--validate"}, &stdout, &stderr); code == 0 {
		t.Fatalf("run(config show --validate) = 0, want nonzero; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if want := `rig "beta": federation = "rig" needs its own store directory`; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

func TestTryReloadConfigRefusesInvalidRigFederation(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(`[workspace]
name = "test"

[[rigs]]
name = "beta"
path = "/srv/beta"
federation = "bogus"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tryReloadConfig(tomlPath, "test", dir)
	if err == nil || !strings.Contains(err.Error(), `rig "beta": federation "bogus" is not one of "city", "rig"`) {
		t.Fatalf("tryReloadConfig error = %v, want the federation enum error", err)
	}
}

// Reload refuses a rig layout that start and the supervisor would refuse, so
// a running city never adopts a config it cannot restart with.
func TestTryReloadConfigRefusesGuardedRigSharingADirectory(t *testing.T) {
	clearGCEnv(t)
	dir := t.TempDir()
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(`[workspace]
name = "test"

[[rigs]]
name = "alpha"
path = "`+shared+`"
prefix = "al"

[[rigs]]
name = "beta"
path = "`+shared+`"
prefix = "be"
federation = "rig"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"test\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tryReloadConfig(tomlPath, "test", dir)
	if err == nil || !strings.Contains(err.Error(), `validating rigs: rig "beta": federation = "rig" needs its own store directory`) {
		t.Fatalf("tryReloadConfig error = %v, want the shared-directory refusal", err)
	}
}

// Every successful reload, changed or not, carries the not-enforced warning.
func TestTryReloadConfigWarnsThatRigFederationIsNotEnforced(t *testing.T) {
	clearGCEnv(t)
	cityPath, _, _ := writeRigFederationCity(t)
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := tryReloadConfig(filepath.Join(cityPath, "city.toml"), "test-city", cityPath)
		if err != nil {
			t.Fatalf("tryReloadConfig #%d: %v", i+1, err)
		}
		found := false
		for _, w := range result.Warnings {
			if strings.HasPrefix(w, "config reload: ") && strings.Contains(w, `rig "beta"`) && strings.Contains(w, "NOT protected") {
				found = true
			}
		}
		if !found {
			t.Fatalf("reload #%d warnings = %v, want the not-enforced warning for rig beta", i+1, result.Warnings)
		}
	}
}

func TestPrintRigFederationStartWarnings(t *testing.T) {
	clearGCEnv(t)
	cityPath, _, _ := writeRigFederationCity(t)
	var stderr bytes.Buffer
	printRigFederationStartWarnings(cityPath, &stderr)
	out := stderr.String()
	if strings.Count(out, "gc start: warning: ") != 1 || !strings.Contains(out, `rig "beta"`) || !strings.Contains(out, "NOT protected") {
		t.Fatalf("stderr = %q, want one not-enforced warning for rig beta", out)
	}
	// The start output proxy counts it as a warning line.
	proxy := newStartOutputProxy(&bytes.Buffer{}, startOutputProxyOptions{})
	if _, err := proxy.Write([]byte(out)); err != nil {
		t.Fatal(err)
	}
	if got := proxy.WarningCount(); got != 1 {
		t.Fatalf("start output proxy WarningCount = %d, want 1", got)
	}

	// A config that does not load prints nothing; the supervisor reports it.
	stderr.Reset()
	printRigFederationStartWarnings(t.TempDir(), &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing for a city without config", stderr.String())
	}
}
