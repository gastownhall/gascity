package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

// writeCredentialCity writes a city whose workspace pins a fake bd, whose
// city.toml carries beadsTOML, and whose scope is attached to the beads http
// backend (127.0.0.1:9) the way `bd connect` writes it.
func writeCredentialCity(t *testing.T, beadsTOML, rigsTOML string) string {
	t.Helper()
	cityPath := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityTOML := fmt.Sprintf("[workspace]\nname = %q\n[workspace.env]\nPATH = %q\n%s\n%s", filepath.Base(cityPath),
		binDir+string(os.PathListSeparator)+"$PATH", beadsTOML, rigsTOML)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	attachRemoteScope(t, cityPath)
	return cityPath
}

// captureBdChildEnv replaces the bd exec runner with a fake that records the
// env overrides each bd child would receive, keyed by its working directory.
func captureBdChildEnv(t *testing.T) func(dir string) map[string]string {
	t.Helper()
	var mu sync.Mutex
	captured := map[string]map[string]string{}
	capture := func(env map[string]string) beads.CommandRunner {
		snapshot := map[string]string{}
		for k, v := range env {
			snapshot[k] = v
		}
		return func(dir, _ string, _ ...string) ([]byte, error) {
			mu.Lock()
			captured[dir] = snapshot
			mu.Unlock()
			return []byte("[]"), nil
		}
	}
	origRunner, origHosted := beadsExecCommandRunnerWithEnv, beadsExecCommandRunnerWithEnvWithoutAmbientBeads
	beadsExecCommandRunnerWithEnv = capture
	beadsExecCommandRunnerWithEnvWithoutAmbientBeads = capture
	t.Cleanup(func() {
		beadsExecCommandRunnerWithEnv, beadsExecCommandRunnerWithEnvWithoutAmbientBeads = origRunner, origHosted
	})
	return func(dir string) map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return captured[dir]
	}
}

func installCityRemoteCredentialLookup(t *testing.T) {
	t.Helper()
	resetHostedCredentialProbeCache()
	beads.SetRemoteCredentialLookup(cityRemoteCredentialLookup)
	t.Cleanup(func() {
		beads.SetRemoteCredentialLookup(nil)
		resetHostedCredentialProbeCache()
	})
}

// TestBdStoreSubprocessEnvCarriesOnlyItsOwnScopesCredential: two cities (and
// a rig with its own beads_credential) on ONE server, with bd's ambient
// ladder configured for that very server. Each scope's BdStore bd child gets
// its own token in BEADS_HTTP_TOKEN (scoped to the server's host:port), the
// ambient token command blanked, and never another scope's token; nothing
// goes on argv. A city with no credential keeps today's ambient env.
func TestBdStoreSubprocessEnvCarriesOnlyItsOwnScopesCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BEADS_HTTP_TOKEN", "127.0.0.1:9=ambient-token")
	t.Setenv("BEADS_HTTP_TOKEN_COMMAND", "127.0.0.1:9=echo ambient-command-token")
	t.Setenv("CITY_A_TOKEN", "token-a")
	t.Setenv("RIG_TOKEN", "token-rig")
	installCityRemoteCredentialLookup(t)
	childEnv := captureBdChildEnv(t)

	cityA := writeCredentialCity(t, "[beads]\ncredential = \"env:CITY_A_TOKEN\"\n",
		"[[rigs]]\nname = \"own\"\npath = \"rigs/own\"\nbeads_credential = \"env:RIG_TOKEN\"\n")
	rigOwn := filepath.Join(cityA, "rigs", "own")
	attachRemoteScope(t, rigOwn)
	cityB := writeCredentialCity(t, "[beads]\ncredential = \"file:.gc/beads-token\"\n", "")
	if err := os.MkdirAll(filepath.Join(cityB, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityB, ".gc", "beads-token"), []byte("token-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ambientCity := writeCredentialCity(t, "", "")

	for _, run := range []struct {
		name   string
		dir    string
		runner beads.CommandRunner
		want   string
	}{
		{"city A", cityA, bdCommandRunnerForCity(cityA), "token-a"},
		{"city A control", cityA, controlBdCommandRunnerForCity(cityA), "token-a"},
		{"city B", cityB, bdCommandRunnerForCity(cityB), "token-b"},
		{"rig with its own credential", rigOwn, bdCommandRunnerForRig(cityA, nil, rigOwn), "token-rig"},
		{"rig control", rigOwn, controlBdCommandRunnerForRig(cityA, nil, rigOwn), "token-rig"},
	} {
		if _, err := run.runner(run.dir, "bd", "list", "--json"); err != nil {
			t.Fatalf("%s: bd runner error = %v", run.name, err)
		}
		env := childEnv(run.dir)
		if got := env["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9="+run.want {
			t.Errorf("%s: BEADS_HTTP_TOKEN = %q, want its own token scoped to 127.0.0.1:9", run.name, got)
		}
		if got, ok := env["BEADS_HTTP_TOKEN_COMMAND"]; !ok || got != "" {
			t.Errorf("%s: BEADS_HTTP_TOKEN_COMMAND = %q (set %v), want blanked", run.name, got, ok)
		}
		for key, value := range env {
			for _, foreign := range []string{"token-a", "token-b", "token-rig", "ambient-token"} {
				if foreign != run.want && strings.Contains(value, foreign) {
					t.Errorf("%s: env[%s] carries another scope's credential", run.name, key)
				}
			}
		}
	}

	if _, err := bdCommandRunnerForCity(ambientCity)(ambientCity, "bd", "list", "--json"); err != nil {
		t.Fatalf("ambient city: %v", err)
	}
	if got, ok := childEnv(ambientCity)["BEADS_HTTP_TOKEN"]; ok {
		t.Errorf("city without a credential: BEADS_HTTP_TOKEN override = %q, want bd's ambient ladder untouched", got)
	}
}

// TestBdStoreSubprocessRefusesAFailedScopeCredential: a configured source that
// fails stops the bd child before it runs (no fallback to the ambient
// ladder), with a typed error that names the source but not a value.
func TestBdStoreSubprocessRefusesAFailedScopeCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BEADS_HTTP_TOKEN", "127.0.0.1:9=ambient-token")
	installCityRemoteCredentialLookup(t)
	childEnv := captureBdChildEnv(t)
	city := writeCredentialCity(t, "[beads]\ncredential = \"env:UNSET_CITY_TOKEN\"\n", "")

	_, err := bdCommandRunnerForCity(city)(city, "bd", "list", "--json")
	if !beads.IsRemoteCredentialError(err) || !strings.Contains(err.Error(), "env:UNSET_CITY_TOKEN") {
		t.Fatalf("error = %v, want a *RemoteCredentialError naming env:UNSET_CITY_TOKEN", err)
	}
	if strings.Contains(err.Error(), "ambient-token") {
		t.Fatalf("error %q leaks the ambient token", err)
	}
	if env := childEnv(city); env != nil {
		t.Fatalf("bd child ran with env %v after a credential failure", env)
	}
}

// stubInvokingGC points resolveInvokingExecutable at an executable fixture, as
// bdCommandEnv pins GC_BIN to the invoking gc.
func stubInvokingGC(t *testing.T) {
	t.Helper()
	invokingGC := filepath.Join(t.TempDir(), "gc")
	writeExecutable(t, invokingGC, "#!/bin/sh\nexit 0\n")
	oldResolve := resolveInvokingExecutable
	resolveInvokingExecutable = func() (string, error) { return invokingGC, nil }
	t.Cleanup(func() { resolveInvokingExecutable = oldResolve })
}

// TestGcBdCommandEnvCarriesOnlyItsOwnScopesCredential: `gc bd` (and the
// show-watch runner, which reuses its env) gives the bd child the scope's own
// credential the way the BdStore runners do, for city and rig scopes and for
// a second city with a different token on the same server; it never falls
// back to the machine-wide ladder.
func TestGcBdCommandEnvCarriesOnlyItsOwnScopesCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_HTTP_TOKEN", "127.0.0.1:9=ambient-token")
	t.Setenv("BEADS_HTTP_TOKEN_COMMAND", "127.0.0.1:9=echo ambient-command-token")
	t.Setenv("GCBD_CITY_A_TOKEN", "token-a")
	t.Setenv("GCBD_RIG_TOKEN", "token-rig")
	stubInvokingGC(t)
	installCityRemoteCredentialLookup(t)

	cityA := writeCredentialCity(t, "[beads]\ncredential = \"env:GCBD_CITY_A_TOKEN\"\n",
		"[[rigs]]\nname = \"own\"\npath = \"rigs/own\"\nbeads_credential = \"env:GCBD_RIG_TOKEN\"\n")
	rigOwn := filepath.Join(cityA, "rigs", "own")
	attachRemoteScope(t, rigOwn)
	cityB := writeCredentialCity(t, "[beads]\ncredential = \"file:.gc/beads-token\"\n", "")
	if err := os.MkdirAll(filepath.Join(cityB, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityB, ".gc", "beads-token"), []byte("token-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, run := range []struct {
		name   string
		city   string
		target execStoreTarget
		want   string
	}{
		{"city A", cityA, execStoreTarget{ScopeRoot: cityA, ScopeKind: "city", Prefix: "a"}, "token-a"},
		{"city B on the same server", cityB, execStoreTarget{ScopeRoot: cityB, ScopeKind: "city", Prefix: "b"}, "token-b"},
		{"rig with its own credential", cityA, execStoreTarget{ScopeRoot: rigOwn, ScopeKind: "rig", Prefix: "own", RigName: "own"}, "token-rig"},
	} {
		envList, err := bdCommandEnv(run.city, nil, run.target)
		if err != nil {
			t.Fatalf("%s: bdCommandEnv: %v", run.name, err)
		}
		env := listToMap(envList)
		if got := env["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9="+run.want {
			t.Errorf("%s: BEADS_HTTP_TOKEN = %q, want its own token scoped to 127.0.0.1:9", run.name, got)
		}
		if got, ok := env["BEADS_HTTP_TOKEN_COMMAND"]; !ok || got != "" {
			t.Errorf("%s: BEADS_HTTP_TOKEN_COMMAND = %q (set %v), want blanked", run.name, got, ok)
		}
		for _, entry := range envList {
			for _, foreign := range []string{"token-a", "token-b", "token-rig", "ambient-token"} {
				if foreign != run.want && strings.Contains(entry, foreign) {
					t.Errorf("%s: env entry %q carries another scope's credential", run.name, strings.SplitN(entry, "=", 2)[0])
				}
			}
		}
	}

	failing := writeCredentialCity(t, "[beads]\ncredential = \"env:GCBD_UNSET_TOKEN\"\n", "")
	if _, err := bdCommandEnv(failing, nil, execStoreTarget{ScopeRoot: failing, ScopeKind: "city", Prefix: "f"}); !beads.IsRemoteCredentialError(err) {
		t.Fatalf("bdCommandEnv with a failing source = %v, want a *RemoteCredentialError (no ambient fallback)", err)
	}
}

// writeEnvDumpingCredentialCity is writeCredentialCity whose workspace bd
// records its environment in dump and prints an empty list.
func writeEnvDumpingCredentialCity(t *testing.T, beadsTOML, dump string) string {
	t.Helper()
	cityPath := t.TempDir()
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "bd"), "#!/bin/sh\nenv > "+dump+"\nprintf '[]\\n'\n")
	cityTOML := fmt.Sprintf("[workspace]\nname = %q\n[workspace.env]\nPATH = %q\n%s\n", filepath.Base(cityPath),
		binDir+string(os.PathListSeparator)+"$PATH", beadsTOML)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	attachRemoteScope(t, cityPath)
	return cityPath
}

// TestScopedBdStoreForCityCarriesTheScopesCredential: the throwaway,
// ctx-bound city store's bd child carries the city's own credential, scoped
// to its server, with the ambient token command blanked.
func TestScopedBdStoreForCityCarriesTheScopesCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("BEADS_HTTP_TOKEN_COMMAND", "127.0.0.1:9=echo ambient-command-token")
	installCityRemoteCredentialLookup(t)
	dump := filepath.Join(t.TempDir(), "bd-env")
	city := writeEnvDumpingCredentialCity(t, "[beads]\ncredential = \"file:.gc/beads-token\"\n", dump)
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".gc", "beads-token"), []byte("token-scoped\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := scopedBdStoreForCity(context.Background(), city)
	if err != nil {
		t.Fatalf("scopedBdStoreForCity: %v", err)
	}
	_, _ = store.List(beads.ListQuery{AllowScan: true})
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("bd child did not run: %v", err)
	}
	env := listToMap(strings.Split(strings.TrimSpace(string(data)), "\n"))
	if got := env["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9=token-scoped" {
		t.Errorf("scoped store bd child BEADS_HTTP_TOKEN = %q, want the city's own token", got)
	}
	if got := env["BEADS_HTTP_TOKEN_COMMAND"]; got != "" {
		t.Errorf("scoped store bd child BEADS_HTTP_TOKEN_COMMAND = %q, want blanked", got)
	}
}

// TestCityConfigLoadMovesEnvCredentialOutOfTheEnvironment: loading a city's
// config (the composition root's call, and the credential probe every store
// open reads) moves each env: source's variable out of the process
// environment, so agents and bd children spawned afterwards inherit nothing;
// the scope's own bd child still gets its token through BEADS_HTTP_TOKEN.
func TestCityConfigLoadMovesEnvCredentialOutOfTheEnvironment(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("SEQ_CITY_TOKEN", "bearer-seq-city")
	t.Setenv("SEQ_RIG_TOKEN", "bearer-seq-rig")
	installCityRemoteCredentialLookup(t)
	childEnv := captureBdChildEnv(t)
	city := writeCredentialCity(t, "[beads]\ncredential = \"env:SEQ_CITY_TOKEN\"\n",
		"[[rigs]]\nname = \"own\"\npath = \"rigs/own\"\nbeads_credential = \"env:SEQ_RIG_TOKEN\"\n")

	if _, err := cityCredentialProbe(city); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SEQ_CITY_TOKEN", "SEQ_RIG_TOKEN"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("%s still in the process environment after the city config load", name)
		}
	}
	// What any child process gc spawns inherits now: the raw process env (the
	// runtime providers), which only the sequestration keeps them out of, and
	// the filtered bd env, which also drops them by their secret-marked names.
	for _, entry := range append(os.Environ(), mergeRuntimeEnv(os.Environ(), nil)...) {
		if strings.Contains(entry, "bearer-seq") {
			t.Errorf("a child env inherits an env: credential (%s)", strings.SplitN(entry, "=", 2)[0])
		}
	}
	if _, err := bdCommandRunnerForCity(city)(city, "bd", "list", "--json"); err != nil {
		t.Fatalf("bd runner: %v", err)
	}
	if got := childEnv(city)["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9=bearer-seq-city" {
		t.Errorf("city bd child BEADS_HTTP_TOKEN = %q, want its own token after sequestration", got)
	}
}

// loadCredentialCityConfig loads the city.toml a credential fixture wrote, as
// the controller holds it.
func loadCredentialCityConfig(t *testing.T, cityPath string) *config.City {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loading %s: %v", cityPath, err)
	}
	return cfg
}

// credentialFixtureOrder is a minimal city-scoped exec order.
var credentialFixtureOrder = orders.Order{Name: "sweep", Trigger: "cooldown", Interval: "1m", Exec: "true"}

func credentialFixtureOrderEnv(t *testing.T, cityPath string, cfg *config.City, order orders.Order) map[string]string {
	t.Helper()
	env, err := orderExecEnvWithError(cityPath, cfg, execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city", Prefix: "pc"}, order, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError(%s): %v", cityPath, err)
	}
	return listToMap(env)
}

func credentialFixtureSessionEnv(t *testing.T, cityPath string, cfg *config.City, agent *config.Agent) TemplateParams {
	t.Helper()
	params := &agentBuildParams{
		cityName:   filepath.Base(cityPath),
		cityPath:   cityPath,
		city:       cfg,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  map[string]string{},
		stderr:     io.Discard,
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(%s): %v", agent.Name, err)
	}
	return tp
}

// TestCityOwnOrdersAndSessionsGetItsSequesteredEnvCredential: once gc moves
// an env: source's variable out of its own environment, the city's own exec
// orders and agent sessions get every one of the city's back (a gc they run
// may open the city's or any rig's scope), so that gc resolves the same
// source. Another city's never reach them, the bd env base does not carry
// them (a bd child gets the token itself), an [order.env] or [agent.env] entry
// still overrides them, and they stay out of the operator env the session
// fingerprint reads.
func TestCityOwnOrdersAndSessionsGetItsSequesteredEnvCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("PROJ_CITY_TOKEN", "bearer-proj-city")
	t.Setenv("PROJ_RIG_TOKEN", "bearer-proj-rig")
	t.Setenv("PROJ_OTHER_TOKEN", "bearer-proj-other")
	installCityRemoteCredentialLookup(t)
	city := writeCredentialCity(t, "[beads]\ncredential = \"env:PROJ_CITY_TOKEN\"\n",
		"[[rigs]]\nname = \"own\"\npath = \"rigs/own\"\nbeads_credential = \"env:PROJ_RIG_TOKEN\"\n")
	other := writeCredentialCity(t, "[beads]\ncredential = \"env:PROJ_OTHER_TOKEN\"\n", "")
	cityCfg, otherCfg := loadCredentialCityConfig(t, city), loadCredentialCityConfig(t, other)
	sequesterCityCredentialEnv(city, cityCfg)
	sequesterCityCredentialEnv(other, otherCfg)
	for _, name := range []string{"PROJ_CITY_TOKEN", "PROJ_RIG_TOKEN", "PROJ_OTHER_TOKEN"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s still in the process environment after the city config load", name)
		}
	}
	own := map[string]string{"PROJ_CITY_TOKEN": "bearer-proj-city", "PROJ_RIG_TOKEN": "bearer-proj-rig"}
	carriesOnly := func(what string, env map[string]string, want map[string]string, foreign ...string) {
		t.Helper()
		for name, value := range want {
			if got := env[name]; got != value {
				t.Errorf("%s: %s = %q, want %q", what, name, got, value)
			}
		}
		for key, value := range env {
			for _, token := range foreign {
				if strings.Contains(value, token) {
					t.Errorf("%s: env[%s] carries %s", what, key, token)
				}
			}
		}
	}

	carriesOnly("city order", credentialFixtureOrderEnv(t, city, cityCfg, credentialFixtureOrder), own, "bearer-proj-other")
	carriesOnly("other city's order", credentialFixtureOrderEnv(t, other, otherCfg, credentialFixtureOrder),
		map[string]string{"PROJ_OTHER_TOKEN": "bearer-proj-other"}, "bearer-proj-city", "bearer-proj-rig")
	scoped := credentialFixtureOrder
	scoped.Env = map[string]string{"PROJ_CITY_TOKEN": "order-scoped"}
	carriesOnly("order with its own [order.env] entry", credentialFixtureOrderEnv(t, city, cityCfg, scoped),
		map[string]string{"PROJ_CITY_TOKEN": "order-scoped", "PROJ_RIG_TOKEN": "bearer-proj-rig"}, "bearer-proj-city")

	base, err := bdRuntimeEnvWithError(city)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	carriesOnly("bd env base", base, nil, "bearer-proj")

	session := credentialFixtureSessionEnv(t, city, cityCfg, &config.Agent{Name: "worker"})
	carriesOnly("city session", session.Env, own, "bearer-proj-other")
	carriesOnly("city session operator env", session.OperatorEnv, nil, "bearer-proj")
	carriesOnly("other city's session", credentialFixtureSessionEnv(t, other, otherCfg, &config.Agent{Name: "worker"}).Env,
		map[string]string{"PROJ_OTHER_TOKEN": "bearer-proj-other"}, "bearer-proj-city", "bearer-proj-rig")
	denied := credentialFixtureSessionEnv(t, city, cityCfg, &config.Agent{Name: "denied", Env: map[string]string{"PROJ_CITY_TOKEN": ""}})
	carriesOnly("session whose [agent.env] blanks the city's name", denied.Env,
		map[string]string{"PROJ_CITY_TOKEN": "", "PROJ_RIG_TOKEN": "bearer-proj-rig"}, "bearer-proj-city")
}

// TestOrderChildGCResolvesTheCitysSequesteredEnvCredential: an exec order
// that runs gc against the city's remote store (the core pack's maintenance
// scripts do) still resolves the city's env: source after gc moved the
// variable out of its own environment: that gc hands its bd child the city's
// token, scoped to the server.
func TestOrderChildGCResolvesTheCitysSequesteredEnvCredential(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("CHILD_GC_TOKEN", "bearer-child-gc")
	installCityRemoteCredentialLookup(t)
	dump := filepath.Join(t.TempDir(), "bd-env")
	city := writeEnvDumpingCredentialCity(t, "[beads]\ncredential = \"env:CHILD_GC_TOKEN\"\n", dump)
	cfg := loadCredentialCityConfig(t, city)
	sequesterCityCredentialEnv(city, cfg)
	if _, ok := os.LookupEnv("CHILD_GC_TOKEN"); ok {
		t.Fatal("CHILD_GC_TOKEN still in the process environment after the city config load")
	}

	orderEnv, err := orderExecEnvWithError(city, cfg, execStoreTarget{ScopeRoot: city, ScopeKind: "city", Prefix: "pc"}, credentialFixtureOrder, nil)
	if err != nil {
		t.Fatalf("orderExecEnvWithError: %v", err)
	}
	gc := reexecGCTestBinaryForTests(t)
	out, err := shellExecRunner(context.Background(), shellSingleQuote(gc)+" bd list --json", city, orderEnv)
	if err != nil {
		t.Fatalf("the order's gc bd list: %v\n%s", err, out)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the order's gc never ran bd: %v\n%s", err, out)
	}
	bdEnv := listToMap(strings.Split(strings.TrimSpace(string(data)), "\n"))
	if got := bdEnv["BEADS_HTTP_TOKEN"]; got != "127.0.0.1:9=bearer-child-gc" {
		t.Errorf("the order's gc gave its bd child BEADS_HTTP_TOKEN = %q, want the city's token scoped to 127.0.0.1:9", got)
	}
}

// TestOrderRunAndDispatchRedactTheCitysProjectedEnvCredential: an exec order
// that prints the city's projected env: credential never shows it, whether
// `gc order run` runs it (failing or succeeding) or the controller dispatches
// it. gc moved the value out of its own environment, so only the order's own
// env, which carries it under its secret-marked name, can redact it.
func TestOrderRunAndDispatchRedactTheCitysProjectedEnvCredential(t *testing.T) {
	const secret = "bearer-redact-city"
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("REDACT_CITY_TOKEN", secret)
	installCityRemoteCredentialLookup(t)
	city := writeCredentialCity(t, "[beads]\ncredential = \"env:REDACT_CITY_TOKEN\"\n", "")
	cfg := loadCredentialCityConfig(t, city)
	sequesterCityCredentialEnv(city, cfg)

	for _, tc := range []struct {
		name, exec string
		wantCode   int
	}{
		{"failing gc order run", `printf '%s\n' "$REDACT_CITY_TOKEN"; exit 1`, 1},
		{"succeeding gc order run", `printf '%s\n' "$REDACT_CITY_TOKEN"`, 0},
	} {
		order := credentialFixtureOrder
		order.Exec = tc.exec
		var stdout, stderr bytes.Buffer
		if got := doOrderRunExecResult(order, city, cfg, nil, &stdout, &stderr).code; got != tc.wantCode {
			t.Fatalf("%s: exit = %d, want %d; stdout=%q stderr=%q", tc.name, got, tc.wantCode, stdout.String(), stderr.String())
		}
		if out := stdout.String() + stderr.String(); strings.Contains(out, secret) || !strings.Contains(out, execenv.Redacted) {
			t.Errorf("%s: output = %q, want the printed credential redacted", tc.name, out)
		}
	}

	store := beads.NewMemStore()
	tracking, err := store.Create(beads.Bead{Title: "order:sweep", Labels: []string{"order-run:sweep", labelOrderTracking}})
	if err != nil {
		t.Fatal(err)
	}
	printsCredential := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		value := listToMap(env)["REDACT_CITY_TOKEN"]
		return []byte("token " + value + "\n"), fmt.Errorf("rejected %s", value)
	}
	var rec memRecorder
	var stderr bytes.Buffer
	mad := buildOrderDispatcherFromListExec([]orders.Order{credentialFixtureOrder}, store, nil, printsCredential, &rec).(*memoryOrderDispatcher)
	mad.cfg, mad.stderr = cfg, &stderr
	logs := captureCmdOrderLogs(t, func() {
		mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}),
			execStoreTarget{ScopeRoot: city, ScopeKind: "city", Prefix: "pc"}, credentialFixtureOrder, city, tracking.ID, nil)
	})
	if out := logs + stderr.String(); strings.Contains(out, secret) || !strings.Contains(out, execenv.Redacted) {
		t.Errorf("dispatched order logs = %q, want the printed credential redacted", out)
	}
	sawFailed := false
	for _, event := range rec.events {
		sawFailed = sawFailed || event.Type == events.OrderFailed
		if strings.Contains(event.Message, secret) {
			t.Errorf("event %s carries the credential: %q", event.Type, event.Message)
		}
	}
	if !sawFailed {
		t.Errorf("events = %#v, want an %s", rec.events, events.OrderFailed)
	}
}
