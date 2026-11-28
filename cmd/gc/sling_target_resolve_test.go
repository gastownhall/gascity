package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// namepoolSlingCity writes a city with one rig pool, frontend/polecat
// (max_active_sessions = 2), whose members are named from a namepool file
// listing furiosa, rictus and nux. It returns the city and rig directories
// with file-backed stores scoped to each.
func namepoolSlingCity(t *testing.T) (cityDir, rigDir string) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	// Agent.NamepoolNames is loaded from this file at config load.
	if err := os.WriteFile(filepath.Join(cityDir, "polecat-names.txt"), []byte("furiosa\nrictus\nnux\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(namepool): %v", err)
	}
	cityToml := `[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "FE"

[[agent]]
name = "polecat"
dir = "frontend"
min_active_sessions = 0
max_active_sessions = 2
namepool = "polecat-names.txt"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Chdir(cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	return cityDir, rigDir
}

// listStoreBeads returns every bead in the file store scoped to dir.
func listStoreBeads(t *testing.T, dir, cityDir string) []beads.Bead {
	t.Helper()
	store, err := openStoreAtForCity(dir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(%s): %v", dir, err)
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(%s): %v", dir, err)
	}
	return all
}

// TestCmdSlingNamepoolMemberTargetRoutesToPool covers #5724: gc status names
// namepool pool members by their themed names (frontend/furiosa), so gc sling
// must accept that name and route the work to the base pool.
func TestCmdSlingNamepoolMemberTargetRoutesToPool(t *testing.T) {
	cityDir, rigDir := namepoolSlingCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdSling([]string{"frontend/furiosa", "ship feature"}, false, false, true, "", nil, "", true, false, false, "", false, false, false, "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSling(frontend/furiosa) = %d, want 0; stderr: %s", code, stderr.String())
	}

	rigBeads := listStoreBeads(t, rigDir, cityDir)
	if len(rigBeads) != 1 {
		t.Fatalf("rig store bead count = %d, want 1: %#v", len(rigBeads), rigBeads)
	}
	if got := rigBeads[0].Title; got != "ship feature" {
		t.Errorf("rig bead title = %q, want %q", got, "ship feature")
	}
	if got := rigBeads[0].Metadata["gc.routed_to"]; got != "frontend/polecat" {
		t.Errorf("rig bead gc.routed_to = %q, want frontend/polecat", got)
	}
	if cityBeads := listStoreBeads(t, cityDir, cityDir); len(cityBeads) != 0 {
		t.Errorf("city store bead count = %d, want 0: %#v", len(cityBeads), cityBeads)
	}
}

// TestBuiltInSlingNamepoolMemberTargetNormalizesRoutedTo is the namepool twin
// of TestBuiltInSlingSlotSuffixedTargetNormalizesRoutedTo: a binding-qualified
// namepool member target resolves to the member identity, and slinging it
// records the base pool in gc.routed_to.
func TestBuiltInSlingNamepoolMemberTargetNormalizesRoutedTo(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "gascity", Path: "/tmp/gascity", Prefix: "gc"}},
		Agents:    []config.Agent{namepoolAgentForNudgeTest()},
	}
	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	store := newSlingTestStore()
	deps = slingDepsWithStore(deps, store)
	deps.StoreRef = "rig:gascity"

	created, err := store.Create(beads.Bead{Title: "namepool-routed work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	target, ok := resolveSlingTarget(cfg, "gascity/gastown.rictus", "")
	if !ok {
		t.Fatal("resolveSlingTarget(gascity/gastown.rictus) failed")
	}
	if got := target.QualifiedName(); got != "gascity/gastown.rictus" {
		t.Fatalf("resolved target QualifiedName = %q, want gascity/gastown.rictus", got)
	}
	if got := agentutil.RoutedToIdentity(&target); got != "gascity/gastown.polecat" {
		t.Fatalf("RoutedToIdentity = %q, want gascity/gastown.polecat", got)
	}

	code := doSling(testOpts(target, created.ID), deps, &fakeQuerier{bead: created}, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	routed, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get routed bead: %v", err)
	}
	if got := routed.Metadata["gc.routed_to"]; got != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to = %q, want gascity/gastown.polecat (member name should normalize to the pool)", got)
	}
}

// TestCmdSlingNamepoolTargetOutsidePoolFailsResolve guards against
// over-acceptance: a namepool name past the pool's bound and a name not in
// the namepool are not pool members, so gc sling refuses them and creates
// nothing.
func TestCmdSlingNamepoolTargetOutsidePoolFailsResolve(t *testing.T) {
	for _, target := range []string{"frontend/nux", "frontend/ghost"} {
		t.Run(target, func(t *testing.T) {
			cityDir, rigDir := namepoolSlingCity(t)

			var stdout, stderr bytes.Buffer
			code := cmdSlingWithJSON([]string{target, "ship feature"}, false, false, true, "", nil, "", true, false, false, "", false, false, false, "", "", true, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("cmdSlingWithJSON(%s) = %d, want 1; stdout=%s stderr=%s", target, code, stdout.String(), stderr.String())
			}
			var got cliJSONErrorOutput
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not a JSON error: %v (%q)", err, stdout.String())
			}
			if got.Error.Code != "target_resolve_failed" {
				t.Errorf("error code = %q, want target_resolve_failed (message %q)", got.Error.Code, got.Error.Message)
			}
			if rigBeads := listStoreBeads(t, rigDir, cityDir); len(rigBeads) != 0 {
				t.Errorf("rig store bead count = %d, want 0: %#v", len(rigBeads), rigBeads)
			}
			if cityBeads := listStoreBeads(t, cityDir, cityDir); len(cityBeads) != 0 {
				t.Errorf("city store bead count = %d, want 0: %#v", len(cityBeads), cityBeads)
			}
		})
	}
}

// TestResolveSlingTargetNamepoolMembers covers namepool member resolution:
// qualified member names, the bare and binding-qualified bare spellings when
// exactly one pool lists the name, and ambiguity across pools.
func TestResolveSlingTargetNamepoolMembers(t *testing.T) {
	boundPool := func(dir string, names ...string) config.Agent {
		return config.Agent{
			Name: "polecat", Dir: dir, BindingName: "gastown",
			NamepoolNames:     names,
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
		}
	}
	cityPool := config.Agent{
		Name: "polecat", NamepoolNames: []string{"furiosa", "rictus"},
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
	}
	unlimited := config.Agent{
		Name: "polecat", Dir: "frontend",
		NamepoolNames: []string{"furiosa", "rictus", "nux", "slit"},
	}
	maxZero := config.Agent{
		Name: "polecat", Dir: "frontend", NamepoolNames: []string{"furiosa"},
		MaxActiveSessions: intPtr(0),
	}
	suspended := boundPool("gascity", "furiosa", "rictus")
	suspended.Suspended = true
	duplicate := boundPool("gascity", "furiosa", "furiosa")

	cases := []struct {
		name       string
		agents     []config.Agent
		target     string
		rigContext string
		wantMember string // "" means the target must not resolve
		wantPool   string
	}{
		{"bare name, one pool", []config.Agent{boundPool("gascity", "furiosa", "rictus")}, "furiosa", "", "gascity/gastown.furiosa", "gascity/gastown.polecat"},
		{"binding-qualified bare name, one pool", []config.Agent{boundPool("gascity", "furiosa", "rictus")}, "gastown.furiosa", "", "gascity/gastown.furiosa", "gascity/gastown.polecat"},
		{"bare name ignores rig context", []config.Agent{boundPool("gascity", "furiosa", "rictus")}, "rictus", "other", "gascity/gastown.rictus", "gascity/gastown.polecat"},
		{"bare name, two rigs", []config.Agent{boundPool("alpha", "furiosa"), boundPool("beta", "furiosa")}, "furiosa", "", "", ""},
		{"binding-qualified bare name, two rigs", []config.Agent{boundPool("alpha", "furiosa"), boundPool("beta", "furiosa")}, "gastown.furiosa", "", "", ""},
		{"qualified name, first of two rigs", []config.Agent{boundPool("alpha", "furiosa"), boundPool("beta", "furiosa")}, "alpha/gastown.furiosa", "", "alpha/gastown.furiosa", "alpha/gastown.polecat"},
		{"qualified name, second of two rigs", []config.Agent{boundPool("alpha", "furiosa"), boundPool("beta", "furiosa")}, "beta/gastown.furiosa", "", "beta/gastown.furiosa", "beta/gastown.polecat"},
		{"qualified name never matches bare", []config.Agent{boundPool("alpha", "furiosa")}, "beta/gastown.furiosa", "", "", ""},
		{"bare name prefers city-scoped pool member", []config.Agent{cityPool, boundPool("gascity", "furiosa")}, "furiosa", "", "furiosa", "polecat"},
		{"duplicate name in one namepool", []config.Agent{duplicate}, "furiosa", "", "gascity/gastown.furiosa", "gascity/gastown.polecat"},
		{"unlimited pool, last namepool name", []config.Agent{unlimited}, "frontend/slit", "", "frontend/slit", "frontend/polecat"},
		{"max-0 pool resolves no member", []config.Agent{maxZero}, "frontend/furiosa", "", "", ""},
		{"max-0 pool resolves no bare member", []config.Agent{maxZero}, "furiosa", "", "", ""},
		{"suspended pool member resolves", []config.Agent{suspended}, "gascity/gastown.furiosa", "", "gascity/gastown.furiosa", "gascity/gastown.polecat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: tc.agents}
			got, ok := resolveSlingTarget(cfg, tc.target, tc.rigContext)
			if tc.wantMember == "" {
				if ok {
					t.Fatalf("resolveSlingTarget(%q) = %q, want no match", tc.target, got.QualifiedName())
				}
				return
			}
			if !ok {
				t.Fatalf("resolveSlingTarget(%q) did not resolve, want %q", tc.target, tc.wantMember)
			}
			if q := got.QualifiedName(); q != tc.wantMember {
				t.Errorf("QualifiedName = %q, want %q", q, tc.wantMember)
			}
			if got.PoolName != tc.wantPool {
				t.Errorf("PoolName = %q, want %q", got.PoolName, tc.wantPool)
			}
			if r := agentutil.RoutedToIdentity(&got); r != tc.wantPool {
				t.Errorf("RoutedToIdentity = %q, want %q", r, tc.wantPool)
			}
		})
	}

	t.Run("suspended pool member keeps suspension", func(t *testing.T) {
		cfg := &config.City{Agents: []config.Agent{suspended}}
		got, ok := resolveSlingTarget(cfg, "gascity/gastown.rictus", "")
		if !ok || !got.Suspended {
			t.Fatalf("resolveSlingTarget = (%+v, %v), want a suspended member", got, ok)
		}
	})
}

// TestResolveSlingTargetKeepsResolveAgentIdentityResults pins precedence:
// any target resolveAgentIdentity already resolves comes back unchanged, so
// namepool member resolution only ever fills in previously failing targets.
func TestResolveSlingTargetKeepsResolveAgentIdentityResults(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: "/tmp/frontend"}},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "frontend", NamepoolNames: []string{"furiosa"}, MaxActiveSessions: intPtr(3)},
			{Name: "furiosa", Dir: "frontend", Description: "literal agent"},
			{Name: "clerk"},
		},
	}

	cases := []struct {
		name       string
		target     string
		rigContext string
		// wantName and wantPool are checked only when wantName is set.
		wantName string
		wantPool string
	}{
		{name: "numeric member", target: "frontend/polecat-2", wantName: "frontend/polecat-2", wantPool: "frontend/polecat"},
		{name: "literal agent shadows a member identity", target: "frontend/furiosa", wantName: "frontend/furiosa", wantPool: ""},
		{name: "city-scoped literal agent bare name", target: "clerk"},
		{name: "rig-qualified pool template", target: "frontend/polecat"},
		{name: "bare pool template name", target: "polecat"},
		{name: "bare name with rig context", target: "polecat", rigContext: "frontend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, ok := resolveAgentIdentity(cfg, tc.target, tc.rigContext)
			if !ok {
				t.Fatalf("resolveAgentIdentity(%q, %q) did not resolve; the case must exercise an existing form", tc.target, tc.rigContext)
			}
			got, ok := resolveSlingTarget(cfg, tc.target, tc.rigContext)
			if !ok {
				t.Fatalf("resolveSlingTarget(%q, %q) did not resolve", tc.target, tc.rigContext)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("resolveSlingTarget(%q, %q) diverged:\nwant %#v\ngot  %#v", tc.target, tc.rigContext, want, got)
			}
			if tc.wantName == "" {
				return
			}
			if q := got.QualifiedName(); q != tc.wantName {
				t.Errorf("QualifiedName = %q, want %q", q, tc.wantName)
			}
			if got.PoolName != tc.wantPool {
				t.Errorf("PoolName = %q, want %q", got.PoolName, tc.wantPool)
			}
		})
	}
}
