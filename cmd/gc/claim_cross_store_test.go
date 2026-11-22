package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCrossStoreClaimDirRedirectsCityAgentToBeadOwningRig(t *testing.T) {
	// vp-kvp stage iii (write half): a city-scoped singleton discovers rig-store
	// work through the federated gc-hook read, so its bd update --claim must land
	// in that rig store rather than its own (city) store.
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "voxist-web")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-web", Path: rigPath, Prefix: "vw"}},
	}
	cityAgent := &config.Agent{Name: "platform-architect", Scope: "city"}

	dir, ok := crossStoreClaimDir(cfg, cityAgent, "vw-123")
	if !ok {
		t.Fatal("city-scoped agent claiming a rig-store bead must redirect to the rig store")
	}
	if dir != rigPath {
		t.Fatalf("redirect dir = %q, want %q", dir, rigPath)
	}
}

func TestCrossStoreClaimDirLeavesRigAgentUnchanged(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "voxist-web")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-web", Path: rigPath, Prefix: "vw"}},
	}
	rigAgent := &config.Agent{Name: "reviewer", Dir: "voxist-web"}

	if _, ok := crossStoreClaimDir(cfg, rigAgent, "vw-123"); ok {
		t.Fatal("rig-scoped agent must be byte-for-byte unchanged (no claim redirect)")
	}
}

func TestCrossStoreClaimDirNoRedirectForCityOwnedBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "voxist-web")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "voxist-web", Path: rigPath, Prefix: "vw"}},
	}
	cityAgent := &config.Agent{Name: "platform-architect", Scope: "city"}

	// A bead whose prefix does not resolve to a configured rig (HQ/city-owned)
	// stays in the agent's own store: no redirect.
	if _, ok := crossStoreClaimDir(cfg, cityAgent, "hq-9"); ok {
		t.Fatal("city-owned bead must not redirect")
	}
}

func TestRunBDForBeadRedirectsCrossStoreWrite(t *testing.T) {
	var inStoreDir string
	var inStoreArgs []string
	var fellBack bool
	exec := agentScriptExecutor{
		runCommand: func(_ string, _ ...string) error {
			fellBack = true
			return nil
		},
		runCommandInStore: func(dir string, _ []string, name string, args ...string) error {
			inStoreDir = dir
			inStoreArgs = append([]string{name}, args...)
			return nil
		},
		resolveBeadStore: func(beadID string) (string, []string, bool, error) {
			if beadID == "vw-1" {
				return "/rigs/voxist-web", []string{"BEADS_DIR=/rigs/voxist-web/.beads"}, true, nil
			}
			return "", nil, false, nil
		},
	}

	if err := exec.runBDForBead("vw-1", "update", "vw-1", "--claim"); err != nil {
		t.Fatalf("runBDForBead cross-store: %v", err)
	}
	if fellBack {
		t.Fatal("cross-store bead must route through runCommandInStore, not the default runCommand")
	}
	if inStoreDir != "/rigs/voxist-web" {
		t.Fatalf("in-store dir = %q, want /rigs/voxist-web", inStoreDir)
	}
	if len(inStoreArgs) == 0 || inStoreArgs[0] != "bd" {
		t.Fatalf("in-store args = %v, want bd ...", inStoreArgs)
	}

	// A bead the resolver does not redirect falls back to the default runner.
	fellBack = false
	if err := exec.runBDForBead("hq-9", "update", "hq-9", "--claim"); err != nil {
		t.Fatalf("runBDForBead fallback: %v", err)
	}
	if !fellBack {
		t.Fatal("non-redirected bead must fall back to the default runCommand")
	}
}

func TestCrossStoreClaimDirNilCfgOrAgent(t *testing.T) {
	rigAgent := &config.Agent{Name: "reviewer", Dir: "voxist-web"}
	if _, ok := crossStoreClaimDir(nil, rigAgent, "vw-1"); ok {
		t.Fatal("nil cfg must not redirect")
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "voxist-web", Path: "/x", Prefix: "vw"}}}
	if _, ok := crossStoreClaimDir(cfg, nil, "vw-1"); ok {
		t.Fatal("nil agent must not redirect")
	}
}

// A store that cannot be decided refuses the write: no bd process runs, in
// the bead's store or in the default one.
func TestRunBDForBeadRefusesWhenStoreUnresolved(t *testing.T) {
	ran := false
	exec := agentScriptExecutor{
		runCommand: func(_ string, _ ...string) error {
			ran = true
			return nil
		},
		runCommandInStore: func(_ string, _ []string, _ string, _ ...string) error {
			ran = true
			return nil
		},
		resolveBeadStore: func(string) (string, []string, bool, error) {
			return "", nil, false, errors.New("loading city config to route bead vw-1: boom")
		},
	}
	err := exec.runBDForBead("vw-1", "update", "vw-1", "--claim")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("runBDForBead err = %v, want the resolver's error", err)
	}
	if ran {
		t.Fatal("a write whose store cannot be decided must not run bd")
	}
}

// bd_claim and bd_update fail when the city config cannot be loaded, instead
// of running the write in the agent's default store.
func TestAgentScriptCrossStoreBeadEnvFailsOnUnloadableCityConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_ALIAS", "platform-architect")
	_, _, ok, err := agentScriptCrossStoreBeadEnv("vw-1")
	if err == nil {
		t.Fatal("agentScriptCrossStoreBeadEnv must fail when city.toml cannot be loaded")
	}
	if ok {
		t.Fatal("a failed resolution must not report a store")
	}
	if !strings.Contains(err.Error(), "vw-1") {
		t.Fatalf("error %q should name the bead", err)
	}
}

// TestHookClaimLandsRigRoutedCandidateInTheRigStore is issue #9159: on a city
// whose federated work query reads every rig store from the primary (city)
// leg, a city-scoped agent's `gc hook --claim` is served a rig-store bead
// routed to it. The claim must run against the store that holds the bead (the
// rig store its prefix names), not the primary leg's city workspace, where it
// fails "bead not found" and the hook drains claims_errored forever. The stores
// and the work-leg route come from the production branch decision
// (hookClaimStores) for the split topology, and the claim runs through
// claimHookWorkProvisioning end to end against a bd stub dispatching by dir.
func TestHookClaimLandsRigRoutedCandidateInTheRigStore(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	for _, d := range []string{filepath.Join(cityDir, ".gc"), filepath.Join(rigDir, ".beads")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[rigs]]\nname = \"frontend\"\npath = " + strconv.Quote(rigDir) + "\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)

	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStore.IDPrefix = "fe"
	bead, err := rigStore.Create(beads.Bead{Title: "rig work", Type: "task", Metadata: map[string]string{"gc.routed_to": "overseer"}})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var claimDirs []string
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, env map[string]string) beads.CommandRunner {
		return func(dir, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				return nil, errors.New("unexpected command " + name)
			}
			store := cityStore
			if filepath.Clean(dir) == filepath.Clean(rigDir) {
				store = rigStore
			}
			show := func(id string) ([]byte, error) {
				b, err := store.Get(id)
				if err != nil {
					return []byte(`{"error": "no issues found matching the provided IDs", "schema_version": 1}`), errors.New("exit status 1: Issue " + id + " not found")
				}
				return json.Marshal([]beads.Bead{b})
			}
			switch {
			case len(args) >= 3 && args[0] == "update" && args[2] == "--claim":
				mu.Lock()
				claimDirs = append(claimDirs, dir)
				mu.Unlock()
				id := args[1]
				if _, err := store.Get(id); err != nil {
					return []byte("Error: claiming bead " + id + ": bead not found"), errors.New("exit status 1: bead not found")
				}
				status, assignee := "in_progress", env["BEADS_ACTOR"]
				if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
					return nil, err
				}
				return show(id)
			case len(args) >= 3 && args[0] == "update" && args[1] == "--json":
				id := args[2]
				opts := beads.UpdateOpts{Metadata: map[string]string{}}
				for i := 3; i+1 < len(args); i += 2 {
					if args[i] == "--set-metadata" {
						k, v, _ := strings.Cut(args[i+1], "=")
						opts.Metadata[k] = v
					}
				}
				if err := store.Update(id, opts); err != nil {
					return []byte("Error: bead not found"), errors.New("exit status 1: bead not found")
				}
				return show(id)
			case len(args) >= 3 && args[0] == "show" && args[1] == "--json":
				return show(args[len(args)-1])
			case len(args) >= 2 && args[0] == "dep" && args[1] == "list":
				return []byte("[]"), nil
			case len(args) >= 1 && args[0] == "query":
				return []byte("[]"), nil
			default:
				return nil, errors.New("unexpected bd invocation: " + strings.Join(args, " "))
			}
		}
	}

	out, err := json.Marshal([]beads.Bead{bead})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)
	a := config.Agent{Name: "overseer", Scope: "city"}
	overrides, err := hookQueryEnv(cityDir, cfg, &a)
	if err != nil {
		t.Fatalf("hookQueryEnv: %v", err)
	}
	workDir := agentCommandDir(cityDir, &a, cfg.Rigs)
	queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
	// The federated primary leg's answer: the rig bead, served from the city leg.
	workQuery := "printf '%s' '" + string(out) + "'"
	singleStore := singleStoreHookWorkQuery(cityDir, loadedCityName(cfg, cityDir), cfg, &a, federatedHookTopology(), io.Discard)
	stores, route := hookClaimStores(cityDir, cfg, &a, "overseer", workDir, queryEnv, overrides, workQuery, singleStore, func() map[string]bool { return nil })
	if route == nil || len(stores) != 1 {
		t.Fatalf("claim stores = %d, work-leg route installed = %v; want the primary-scoped branch: one store and a route", len(stores), route != nil)
	}
	opts := hookClaimOptions{
		Assignee:           "overseer",
		IdentityCandidates: []string{"overseer"},
		RouteTargets:       []string{"overseer"},
		Env:                queryEnv,
		JSON:               true,
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkProvisioning(newHookCity(cityDir, cfg), workQuery, workDir, queryEnv, stores, route, opts, nil,
		func(string, error) {}, &stdout, &stderr)

	held, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || strings.Contains(stdout.String(), `"drain"`) || held.Status != "in_progress" || held.Assignee != "overseer" {
		t.Fatalf("rig-routed %s: code=%d rig store holds status=%q assignee=%q; claims ran in %q; stdout=%s stderr=%s\nwant the claim to run in the rig store %s and own the bead",
			bead.ID, code, held.Status, held.Assignee, claimDirs, stdout.String(), stderr.String(), rigDir)
	}
}
