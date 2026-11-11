package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// promptQueryPinTemplate prints the four discovery placeholders between line
// sentinels so each rendered value can be recovered byte-exactly from a fully
// rendered prompt, not just from a PromptContext struct.
const promptQueryPinTemplate = `__WORK_QUERY_BEGIN__
{{.WorkQuery}}
__WORK_QUERY_END__
__ASSIGNED_IN_PROGRESS_BEGIN__
{{.AssignedInProgressQuery}}
__ASSIGNED_IN_PROGRESS_END__
__ASSIGNED_READY_BEGIN__
{{.AssignedReadyQuery}}
__ASSIGNED_READY_END__
__ROUTED_POOL_BEGIN__
{{.RoutedPoolQuery}}
__ROUTED_POOL_END__
`

// writePromptQueryPinTemplate writes promptQueryPinTemplate under cityPath and
// returns its path relative to the city root.
func writePromptQueryPinTemplate(t *testing.T, cityPath string) string {
	t.Helper()
	dir := filepath.Join(cityPath, "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating prompts dir: %v", err)
	}
	rel := filepath.Join("prompts", "pin.template.md")
	if err := os.WriteFile(filepath.Join(cityPath, rel), []byte(promptQueryPinTemplate), 0o644); err != nil {
		t.Fatalf("writing pin template: %v", err)
	}
	return rel
}

// extractPinnedPromptQuery returns the value rendered between one template's
// sentinel pair, or "" when the pair is absent.
func extractPinnedPromptQuery(rendered, marker string) string {
	open := "__" + marker + "_BEGIN__"
	endMarker := "__" + marker + "_END__"
	i := strings.Index(rendered, open)
	if i < 0 {
		return ""
	}
	rest := rendered[i+len(open):]
	j := strings.Index(rest, endMarker)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// assertFederatedDiscoveryQuery checks one discovery placeholder of an agent
// whose prompt must reach work in every store: the query reads through the
// federated `gc ready` reader and no longer through a single-store `bd` read.
func assertFederatedDiscoveryQuery(t *testing.T, name, query string) {
	t.Helper()
	if !strings.Contains(query, "gc ready") {
		t.Errorf("%s does not read through the federated reader: %q", name, query)
	}
	if strings.Contains(query, "bd ready") {
		t.Errorf("%s still runs a single-store bd ready read: %q", name, query)
	}
	if strings.Contains(query, "bd list --status in_progress") {
		t.Errorf("%s still runs a single-store crash-recovery read: %q", name, query)
	}
}

// promptQueryTestParams builds the minimal agentBuildParams resolveTemplate
// needs for the render sites under test.
func promptQueryTestParams(cityPath string, cfg *config.City) *agentBuildParams {
	return &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        cfg.Workspace.Name,
		cityPath:        cityPath,
		city:            cfg,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          io.Discard,
		rigs:            cfg.Rigs,
	}
}

// TestPromptQueriesForCityScopedAgentReachRigStores pins AC-1 of issue #1689:
// a city-scoped agent's four discovery placeholders run verbatim in the agent's
// own shell, so on a city with rigs they must read through the federated
// `gc ready` (which spans the city store and every rig store) instead of the
// single-store `bd` commands its work_dir resolves. `gc hook` needs no such
// swap — it fans the same single-store query out per store — but the prompt
// placeholders have no fan-out around them.
func TestPromptQueriesForCityScopedAgentReachRigStores(t *testing.T) {
	t.Run("pack-agnostic city agent", func(t *testing.T) {
		cityPath := t.TempDir()
		tmpl := writePromptQueryPinTemplate(t, cityPath)
		rigs := []config.Rig{{Name: "myrig", Path: filepath.Join(t.TempDir(), "myrig")}}
		cfg := &config.City{
			Workspace: config.Workspace{Name: "bright-lights", Provider: "claude"},
			Rigs:      rigs,
		}
		agent := &config.Agent{
			Name:           "planner",
			Scope:          "city",
			Provider:       "claude",
			PromptTemplate: tmpl,
		}
		topo := cityQueryTopology(cityPath, cfg)
		if topo.FederatedReady {
			t.Fatalf("fixture precondition: a temp-dir city relocates no coordination class, so FederatedReady must be false here")
		}

		// Site 1: gc prime's context builder (also drives the prompt-delivery
		// doctor through the same function).
		ctx := buildAgentPromptContext(cityPath, cfg.Workspace.Name, agent, rigs, topo, io.Discard)
		assertFederatedDiscoveryQuery(t, "prime WorkQuery", ctx.WorkQuery)
		assertFederatedDiscoveryQuery(t, "prime AssignedInProgressQuery", ctx.AssignedInProgressQuery)
		assertFederatedDiscoveryQuery(t, "prime AssignedReadyQuery", ctx.AssignedReadyQuery)
		assertFederatedDiscoveryQuery(t, "prime RoutedPoolQuery", ctx.RoutedPoolQuery)

		// Site 2: the controller's session-start render. The placeholders come
		// back inside the rendered prompt, so recover them between the
		// sentinels and require byte-identical values from both sites.
		tp, err := resolveTemplate(promptQueryTestParams(cityPath, cfg), agent, agent.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate: %v", err)
		}
		sitePairs := []struct {
			marker string
			want   string
		}{
			{"WORK_QUERY", ctx.WorkQuery},
			{"ASSIGNED_IN_PROGRESS", ctx.AssignedInProgressQuery},
			{"ASSIGNED_READY", ctx.AssignedReadyQuery},
			{"ROUTED_POOL", ctx.RoutedPoolQuery},
		}
		for _, pair := range sitePairs {
			got := extractPinnedPromptQuery(tp.Prompt, pair.marker)
			if got == "" {
				t.Fatalf("controller render lost the %s placeholder; prompt:\n%s", pair.marker, tp.Prompt)
			}
			assertFederatedDiscoveryQuery(t, "controller "+pair.marker, got)
			if got != pair.want {
				t.Fatalf("controller %s differs from the gc prime render:\ncontroller: %q\nprime:      %q", pair.marker, got, pair.want)
			}
		}
	})

	t.Run("shipped gastown mayor", func(t *testing.T) {
		clearGCEnv(t)
		t.Setenv("GC_BEADS", "file")
		t.Setenv("GC_DOLT", "skip")
		configureIsolatedRuntimeEnv(t)

		src := gastownExamplePath(t)
		cityPath := filepath.Join(t.TempDir(), "city")
		var initOut, initErr bytes.Buffer
		if code := doInitFromDirWithOptions(src, cityPath, "", &initOut, &initErr, true); code != 0 {
			t.Fatalf("doInitFromDirWithOptions = %d; stderr: %s", code, initErr.String())
		}
		rigPath := filepath.Join(t.TempDir(), "myrig")
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatalf("creating rig dir: %v", err)
		}
		var rigOut, rigErr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &rigOut, &rigErr); code != 0 {
			t.Fatalf("doRigAdd = %d; stderr: %s", code, rigErr.String())
		}

		cfg, err := loadCityConfig(cityPath)
		if err != nil {
			t.Fatalf("loadCityConfig: %v", err)
		}
		if len(cfg.Rigs) != 1 {
			t.Fatalf("fixture precondition: len(Rigs) = %d, want 1", len(cfg.Rigs))
		}
		var mayor *config.Agent
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == "mayor" {
				mayor = &cfg.Agents[i]
				break
			}
		}
		if mayor == nil {
			t.Fatal("fixture precondition: the shipped gastown pack declares a mayor agent")
		}
		if !agentIsCrossStoreEligible(mayor) {
			t.Fatalf("fixture precondition: the shipped gastown mayor is city-scoped, got scope %q", mayor.Scope)
		}
		topo := cityQueryTopology(cityPath, cfg)
		if topo.FederatedReady {
			t.Fatal("fixture precondition: this city relocates no coordination class")
		}

		ctx := buildAgentPromptContext(cityPath, cfg.Workspace.Name, mayor, cfg.Rigs, topo, io.Discard)
		assertFederatedDiscoveryQuery(t, "gastown mayor WorkQuery", ctx.WorkQuery)
		assertFederatedDiscoveryQuery(t, "gastown mayor AssignedInProgressQuery", ctx.AssignedInProgressQuery)

		tp, err := resolveTemplate(promptQueryTestParams(cityPath, cfg), mayor, mayor.QualifiedName(), nil)
		if err != nil {
			t.Fatalf("resolveTemplate(mayor): %v", err)
		}
		if !strings.Contains(tp.Prompt, ctx.WorkQuery) {
			t.Errorf("the controller-rendered mayor prompt does not carry the WorkQuery gc prime renders; prime renders %q", ctx.WorkQuery)
		}
		if !strings.Contains(tp.Prompt, ctx.AssignedInProgressQuery) {
			t.Errorf("the controller-rendered mayor prompt does not carry the AssignedInProgressQuery gc prime renders; prime renders %q", ctx.AssignedInProgressQuery)
		}
	})
}

// TestCityScopedPromptDiscoveryFailsLoudOnABrokenRigStore pins the trade
// promptQueryTopology documents. A city-scoped agent's discovery placeholders
// read every serving rig store through `gc ready`, so while one rig store
// cannot be opened they exit non-zero naming that rig — even though the claim
// the agent holds sits in the healthy city store — rather than answer from the
// stores that still read. The healthy-city control runs the same rendered query
// over the same claim and serves it, so the failure belongs to the broken rig,
// not to a query that reads nothing.
func TestCityScopedPromptDiscoveryFailsLoudOnABrokenRigStore(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to run the generated discovery query")
	}
	gcBin := reexecGCTestBinaryForTests(t)
	fakeBD := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(fakeBD, []byte("#!/bin/sh\nprintf '[]'\n"), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	const session = "planner-session"

	// runInProgressQuery stores an in_progress claim for session in the city
	// store, renders the city-scoped agent's AssignedInProgressQuery the way
	// gc prime does, and runs it as the agent's shell would.
	runInProgressQuery := func(t *testing.T, cityDir string) (stdout, stderr string, exitCode int, claimID string) {
		t.Helper()
		cityStore, err := openStoreAtForCity(cityDir, cityDir)
		if err != nil {
			t.Fatalf("open city store: %v", err)
		}
		claim := mustCreateReadyBead(t, cityStore, beads.Bead{Title: "claimed city work", Type: "task"})
		if err := cityStore.Update(claim.ID, beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: stringPtr(session)}); err != nil {
			t.Fatalf("claim %s: %v", claim.ID, err)
		}
		cfg, err := loadCityConfig(cityDir, io.Discard)
		if err != nil {
			t.Fatalf("loadCityConfig: %v", err)
		}
		topo := cityQueryTopology(cityDir, cfg)
		if topo.FederatedReady {
			t.Fatal("fixture precondition: this city relocates no coordination class")
		}
		agent := &config.Agent{Name: "planner", Scope: "city"}
		query := buildAgentPromptContext(cityDir, cfg.Workspace.Name, agent, cfg.Rigs, topo, io.Discard).AssignedInProgressQuery
		if !strings.Contains(query, "gc ready --status in_progress") {
			t.Fatalf("the city-scoped AssignedInProgressQuery does not read through the federated reader: %q", query)
		}

		var out, errOut bytes.Buffer
		cmd := exec.Command("sh", "-c", query)
		cmd.Dir = cityDir
		cmd.Env = append(os.Environ(),
			"PATH="+filepath.Dir(gcBin)+string(os.PathListSeparator)+filepath.Dir(fakeBD)+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GC_SESSION_ID="+session, "GC_SESSION_NAME=", "GC_ALIAS=",
			// The local read: no controller runs here, and asking for one only
			// waits out its timeout before reading the stores anyway.
			"GC_NO_API=1",
		)
		cmd.Stdout, cmd.Stderr = &out, &errOut
		exitCode = 0
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("running the discovery query: %v", err)
			}
			exitCode = exitErr.ExitCode()
		}
		return out.String(), errOut.String(), exitCode, claim.ID
	}

	t.Run("healthy rig stores: the claim is served", func(t *testing.T) {
		clearGCEnv(t)
		cityDir := newReadyCityWithRigs(t)
		stdout, stderr, exitCode, claimID := runInProgressQuery(t, cityDir)
		if exitCode != 0 {
			t.Fatalf("query exited %d over healthy rig stores; stderr=%s", exitCode, stderr)
		}
		if !strings.Contains(stdout, claimID) {
			t.Fatalf("query served %q, want the agent's city-store claim %s", stdout, claimID)
		}
	})

	t.Run("one rig store broken: the query fails naming it", func(t *testing.T) {
		clearGCEnv(t)
		cityDir := newReadyCityWithBrokenRig(t)
		stdout, stderr, exitCode, claimID := runInProgressQuery(t, cityDir)
		if exitCode == 0 {
			t.Fatalf("query exited 0 with the %q rig store unopenable and printed %q; an answer short a store is the false \"no work\" the federated reader exists to rule out", readyBrokenRigName, stdout)
		}
		if strings.Contains(stdout, claimID) || strings.Contains(stdout, "[]") {
			t.Errorf("query printed %q alongside its failure; a caller must not find a plausible answer on stdout", stdout)
		}
		for _, want := range []string{readyBrokenRigName, "gc work query: gc ready --status in_progress failed"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("query stderr = %q, want it to contain %q", stderr, want)
			}
		}
	})
}
