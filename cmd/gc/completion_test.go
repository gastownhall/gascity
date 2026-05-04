package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func TestCompleteSessionIDs_EarlyExitOnExtraArgs(t *testing.T) {
	// When the positional is already satisfied, the completer must return no
	// candidates and must not attempt to open the city store — otherwise it
	// would error out or emit noise for every keystroke after the ID is typed.
	got, dir := completeSessionIDs(nil, []string{"gc-42"}, "anything")
	if len(got) != 0 {
		t.Errorf("expected no candidates with args set, got %v", got)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
}

func TestCompleteRigNames_EarlyExitOnExtraArgs(t *testing.T) {
	got, dir := completeRigNames(nil, []string{"myrig"}, "x")
	if len(got) != 0 {
		t.Errorf("expected no candidates, got %v", got)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
}

func TestCompleteOrderNames_EarlyExitOnExtraArgs(t *testing.T) {
	got, dir := completeOrderNames(nil, []string{"some-order"}, "x")
	if len(got) != 0 {
		t.Errorf("expected no candidates, got %v", got)
	}
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
}

func TestSessionCompletionDescription(t *testing.T) {
	cases := []struct {
		name string
		in   session.Info
		want string
	}{
		{"alias + state", session.Info{Alias: "mayor", State: session.State("asleep")}, "mayor (asleep)"},
		{"template fallback", session.Info{Template: "gascity/claude", State: session.State("active")}, "gascity/claude (active)"},
		{"empty state renders as closed", session.Info{Alias: "a"}, "a (closed)"},
		{"no alias and no template", session.Info{State: session.State("suspended")}, "- (suspended)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionCompletionDescription(tc.in)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestOrderCompletionDescription(t *testing.T) {
	cases := []struct {
		name string
		in   orders.Order
		want string
	}{
		{"formula + interval", orders.Order{Formula: "f", Interval: "5m"}, "formula, 5m"},
		{"exec + schedule", orders.Order{Exec: "s", Schedule: "0 0 * * *"}, "exec, 0 0 * * *"},
		{"formula + event", orders.Order{Formula: "f", On: "bead.closed"}, "formula, bead.closed"},
		{"rig scoped", orders.Order{Formula: "f", Interval: "5m", Rig: "frontend"}, "formula, 5m (rig: frontend)"},
		{"no timing", orders.Order{Formula: "f"}, "formula, -"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := orderCompletionDescription(tc.in)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestQuietDefaultLogger_RestoresOutput(t *testing.T) {
	// The default logger's writer must be restored after fn returns, even if
	// fn panics or writes to it — otherwise a single noisy completion call
	// would leave the logger silenced for the rest of the process.
	origWriter := log.Default().Writer()
	t.Cleanup(func() { log.SetOutput(origWriter) })

	var before bytes.Buffer
	log.SetOutput(&before)

	quietDefaultLogger(func() {
		log.Print("silenced")
	})
	if strings.Contains(before.String(), "silenced") {
		t.Errorf("expected log output to be suppressed inside quietDefaultLogger, got %q", before.String())
	}

	log.Print("audible")
	if !strings.Contains(before.String(), "audible") {
		t.Errorf("expected log output restored after quietDefaultLogger, got %q", before.String())
	}
}

func TestResolveCityForCompletion_UsesExplicitRigBindingOutsideCity(t *testing.T) {
	gcHome := t.TempDir()
	cityPath := t.TempDir()
	rigDir := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", gcHome)
	registerRigBindingForResolution(t, gcHome, cityPath, "completion-city", "frontend", rigDir)

	isolateCompletionContext(t, "")
	rigFlag = "frontend"
	t.Chdir(t.TempDir())

	got, err := resolveCityForCompletion()
	if err != nil {
		t.Fatalf("resolveCityForCompletion: %v", err)
	}
	if !samePath(got, cityPath) {
		t.Fatalf("city path = %q, want %q", got, cityPath)
	}
}

func TestRigNameCandidates_LoadsAndFilters(t *testing.T) {
	// Integration check for the rig source-of-truth — exercises resolveCity
	// (via t.Chdir into a temp city), loadCityConfigFS, and the prefix filter.
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"/tmp/alpha\"\n\n[[rigs]]\nname = \"beta\"\npath = \"/tmp/beta\"\n"
	writeCompletionCity(t, cityPath, cityToml)
	isolateCompletionContext(t, "")
	t.Chdir(cityPath)
	t.Setenv("GC_RIG", "ambient-rig-from-agent-session")
	t.Setenv("GC_RIG_ROOT", "/does/not/matter")

	got := rigNameCandidates("")
	if len(got) != 2 {
		t.Fatalf("expected 2 rig candidates, got %d: %v", len(got), got)
	}
	names := make([]string, len(got))
	for i, c := range got {
		names[i] = strings.SplitN(c, "\t", 2)[0]
	}
	for _, want := range []string{"alpha", "beta"} {
		if !slicesContains(names, want) {
			t.Errorf("missing candidate %q in %v", want, names)
		}
	}
	if slicesContains(names, "my-city") {
		t.Errorf("synthetic HQ candidate should not be offered for rig arguments: %v", names)
	}

	// Prefix filter.
	got = rigNameCandidates("al")
	if len(got) != 1 || !strings.HasPrefix(got[0], "alpha\t") {
		t.Errorf("expected only alpha candidate for prefix 'al', got %v", got)
	}
}

func TestCompleteRigFlagNames_IgnoresPositionalArgs(t *testing.T) {
	cityPath := t.TempDir()
	writeCompletionCity(t, cityPath, "[workspace]\nname = \"my-city\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"/tmp/alpha\"\n\n[[rigs]]\nname = \"beta\"\npath = \"/tmp/beta\"\n")
	isolateCompletionContext(t, cityPath)

	for _, cmd := range []*cobra.Command{
		newOrderShowCmd(os.Stdout, os.Stderr),
		newOrderRunCmd(os.Stdout, os.Stderr),
	} {
		complete, ok := cmd.GetFlagCompletionFunc("rig")
		if !ok {
			t.Fatalf("%s missing --rig completion function", cmd.Name())
		}
		got, dir := complete(cmd, []string{"existing-order"}, "a")
		if dir != cobra.ShellCompDirectiveNoFileComp {
			t.Errorf("%s --rig directive = %v, want NoFileComp", cmd.Name(), dir)
		}
		if len(got) != 1 || !strings.HasPrefix(got[0], "alpha\t") {
			t.Errorf("%s --rig completion with positional args = %v, want alpha", cmd.Name(), got)
		}
	}
}

func TestCompleteOrderNames_LoadsOrders(t *testing.T) {
	cityPath := t.TempDir()
	writeCompletionCity(t, cityPath, "[workspace]\nname = \"orders-city\"\n")
	isolateCompletionContext(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "orders", "digest.toml"), []byte(`
[order]
formula = "mol-digest"
trigger = "cron"
schedule = "*/5 * * * *"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, dir := completeOrderNames(nil, nil, "di")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	if len(got) != 1 || got[0] != "digest\tformula, */5 * * * *" {
		t.Fatalf("order candidates = %v, want digest with cron description", got)
	}
}

func TestCompleteOrderNames_SuppressesConfigPackWarnings(t *testing.T) {
	cityPath := t.TempDir()
	writeCompletionCity(t, cityPath, `[workspace]
name = "orders-city"
includes = ["packs/missing"]
`)
	isolateCompletionContext(t, cityPath)

	origWriter := log.Default().Writer()
	t.Cleanup(func() { log.SetOutput(origWriter) })
	var logs bytes.Buffer
	log.SetOutput(&logs)

	_, dir := completeOrderNames(nil, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	if logs.Len() != 0 {
		t.Fatalf("completion wrote default logger output: %q", logs.String())
	}
}

func TestCompleteSessionIDs_LoadsBeadBackedSessions(t *testing.T) {
	cityPath := t.TempDir()
	writeCompletionCity(t, cityPath, `[workspace]
name = "sessions-city"

[session]
provider = "fake"

[beads]
provider = "file"
`)
	isolateCompletionContext(t, cityPath)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityPath, err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"session_name": "sessions-city--worker",
			"state":        "asleep",
			"template":     "codex",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session): %v", err)
	}

	got, dir := completeSessionIDs(nil, nil, "")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	names := completionCandidateNames(got)
	if !slicesContains(names, created.ID) {
		t.Errorf("session ID candidate %q missing from %v", created.ID, got)
	}
	if !slicesContains(names, "worker") {
		t.Errorf("session alias candidate missing from %v", got)
	}
	if !slicesContains(got, "worker\tworker (asleep)") {
		t.Errorf("session alias description missing from %v", got)
	}
}

func TestLoadSessionsForCompletion_SwallowsProviderConstructionError(t *testing.T) {
	cityPath := t.TempDir()
	writeCompletionCity(t, cityPath, `[workspace]
name = "sessions-city"

[session]
provider = "fake"

[beads]
provider = "file"

[providers.opencode]
command = "/bin/echo"
path_check = "true"
supports_acp = true
acp_command = "/bin/echo"

[[agent]]
name = "worker"
provider = "opencode"
session = "acp"
`)
	isolateCompletionContext(t, cityPath)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityPath, err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
	}); err != nil {
		t.Fatalf("store.Create(session): %v", err)
	}
	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		if name == "acp" {
			return nil, errors.New("provider unavailable")
		}
		return oldBuild(name, sc, cityName, cityPath)
	}

	got := loadSessionsForCompletion()
	if len(got) != 0 {
		t.Fatalf("sessions = %v, want none after provider construction failure", got)
	}
}

func TestCompleteOrderNames_DistinguishesSameNameRigOrders(t *testing.T) {
	cityPath := t.TempDir()
	sidecarPackDir := filepath.Join(cityPath, "packs", "sidecar")
	for _, dir := range []string{
		filepath.Join(cityPath, ".gc"),
		filepath.Join(cityPath, "rigs", "frontend"),
		filepath.Join(cityPath, "rigs", "backend"),
		filepath.Join(sidecarPackDir, "formulas"),
		filepath.Join(sidecarPackDir, "orders"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(cityPath, "pack.toml"), `
[pack]
name = "orders-city"
schema = 2
`)
	writeFile(t, filepath.Join(cityPath, "city.toml"), `
[workspace]
name = "orders-city"

[[rigs]]
name = "frontend"
path = "rigs/frontend"

[rigs.imports.sidecar]
source = "./packs/sidecar"

[[rigs]]
name = "backend"
path = "rigs/backend"

[rigs.imports.sidecar]
source = "./packs/sidecar"
`)
	writeFile(t, filepath.Join(sidecarPackDir, "pack.toml"), `
[pack]
name = "sidecar"
schema = 2
`)
	writeFile(t, filepath.Join(sidecarPackDir, "orders", "digest.toml"), `
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "5m"
`)
	isolateCompletionContext(t, cityPath)

	got, dir := completeOrderNames(nil, nil, "dig")
	if dir != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", dir)
	}
	for _, want := range []string{
		"digest\tformula, 5m (rig: backend)",
		"digest\tformula, 5m (rig: frontend)",
	} {
		if !slicesContains(got, want) {
			t.Errorf("missing candidate %q in %v", want, got)
		}
	}
}

func isolateCompletionContext(t *testing.T, cityPath string) {
	t.Helper()
	origCity, origRig := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag, rigFlag = origCity, origRig
	})
	for _, key := range []string{
		"GC_BEADS",
		"GC_BEADS_SCOPE_ROOT",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_DIR",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_SESSION",
	} {
		t.Setenv(key, "")
	}
	if cityPath != "" {
		t.Setenv("GC_CITY", cityPath)
		t.Setenv("GC_CITY_PATH", cityPath)
	}
}

func writeCompletionCity(t *testing.T, cityPath, cityToml string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func completionCandidateNames(candidates []string) []string {
	names := make([]string, len(candidates))
	for i, c := range candidates {
		names[i] = strings.SplitN(c, "\t", 2)[0]
	}
	return names
}

func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
