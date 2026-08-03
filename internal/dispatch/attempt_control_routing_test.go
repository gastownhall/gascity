package dispatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

func testProcessOptionsWithControlDispatcher(rigContext string) ProcessOptions {
	routeCfg := &routeConfigCache{}
	routeCfg.once.Do(func() {
		routeCfg.cfg = &config.City{Agents: []config.Agent{{
			Name: config.ControlDispatcherAgentName,
			Dir:  rigContext,
		}}}
	})
	return ProcessOptions{routeCfg: routeCfg}
}

// TestIsAttemptControlKindMatchesControlKinds pins isAttemptControlKind to
// exactly beadmeta.ControlKinds. The predicate used to be a frozen 2026-04-14
// snapshot that excluded drain (added to every other routing predicate by
// PR #2784 but never here); any kind the dispatcher's ProcessControl switch
// can execute must also be routed to it on the Attach/fragment paths, or
// those beads land on worker queues no prompt knows how to process
// (ga-fux85s residual).
func TestIsAttemptControlKindMatchesControlKinds(t *testing.T) {
	for _, kind := range beadmeta.ControlKinds {
		if !isAttemptControlKind(kind) {
			t.Errorf("isAttemptControlKind(%q) = false, want true (must cover all of beadmeta.ControlKinds)", kind)
		}
	}
	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindWorkflow, beadmeta.KindScope, beadmeta.KindSpec, beadmeta.KindRun, beadmeta.KindRetryRun, beadmeta.KindCleanup, beadmeta.KindWisp, beadmeta.KindClosed} {
		if isAttemptControlKind(kind) {
			t.Errorf("isAttemptControlKind(%q) = true, want false", kind)
		}
	}
}

// TestRouteFanoutFragmentStepsRoutesDrainToControlDispatcher proves the
// fragment-path drain residual of ga-fux85s: when a fanout fragment carries a
// gc.kind=drain step ([template.drain] is mintable on the
// CompileExpansionFragment path), routeFanoutFragmentSteps must route it to
// the control dispatcher (gc.routed_to = <rig>/control-dispatcher, execution
// lane preserved in gc.execution_routed_to) instead of stamping the worker
// execution route on it like plain work.
func TestRouteFanoutFragmentStepsRoutesDrainToControlDispatcher(t *testing.T) {
	fragment := &formula.FragmentRecipe{
		Name: "frag",
		Steps: []formula.RecipeStep{
			{ID: "frag.item.work", Metadata: map[string]string{}},
			{ID: "frag.item.drain", Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindDrain,
				beadmeta.RootStoreRefMetadataKey: "rig:stale",
			}},
		},
	}
	control := beads.Bead{Metadata: map[string]string{
		beadmeta.ExecutionRoutedToMetadataKey: "worker",
		beadmeta.RootStoreRefMetadataKey:      "rig:gascity",
	}}
	opts := testProcessOptionsWithControlDispatcher("gascity")
	opts.routeCfg.cfg.Agents = append(opts.routeCfg.cfg.Agents, config.Agent{Name: "worker"})

	if err := routeFanoutFragmentSteps(fragment, control, opts, beads.NewMemStore()); err != nil {
		t.Fatalf("routeFanoutFragmentSteps: %v", err)
	}

	wantControlRoute := "gascity/" + config.ControlDispatcherAgentName
	step := fragmentStepByID(t, fragment, "frag.item.drain")
	if got := step.Metadata[beadmeta.RoutedToMetadataKey]; got != wantControlRoute {
		t.Errorf("drain gc.routed_to = %q, want %q (control beads must reach the dispatcher, not a worker queue)", got, wantControlRoute)
	}
	if got := step.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "worker" {
		t.Errorf("drain gc.execution_routed_to = %q, want worker (city execution lane preserved)", got)
	}
	if got := step.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "rig:gascity" {
		t.Errorf("drain gc.root_store_ref = %q, want authoritative parent store rig:gascity", got)
	}

	work := fragmentStepByID(t, fragment, "frag.item.work")
	if got := work.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("work step gc.routed_to = %q, want worker", got)
	}
}

func TestRouteFanoutFragmentStepsUsesCityStoreScopeOverRigExecution(t *testing.T) {
	fragment := &formula.FragmentRecipe{
		Name: "frag",
		Steps: []formula.RecipeStep{{
			ID: "frag.item.drain",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindDrain,
				beadmeta.RootStoreRefMetadataKey: "rig:stale",
			},
		}},
	}
	control := beads.Bead{Metadata: map[string]string{
		beadmeta.ExecutionRoutedToMetadataKey: "fixture/worker",
		beadmeta.RootStoreRefMetadataKey:      "city:maintainer-city",
	}}
	opts := testProcessOptionsWithControlDispatcher("")
	opts.routeCfg.cfg.Agents = append(opts.routeCfg.cfg.Agents, config.Agent{Name: "worker", Dir: "fixture"})

	if err := routeFanoutFragmentSteps(fragment, control, opts, beads.NewMemStore()); err != nil {
		t.Fatalf("routeFanoutFragmentSteps: %v", err)
	}

	step := fragmentStepByID(t, fragment, "frag.item.drain")
	if got := step.Metadata[beadmeta.RoutedToMetadataKey]; got != config.ControlDispatcherAgentName {
		t.Fatalf("drain gc.routed_to = %q, want owning city-store dispatcher", got)
	}
	if got := step.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "fixture/worker" {
		t.Fatalf("drain gc.execution_routed_to = %q, want fixture/worker", got)
	}
	if got := step.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "city:maintainer-city" {
		t.Fatalf("drain gc.root_store_ref = %q, want authoritative parent city store", got)
	}
}

func fragmentStepByID(t *testing.T, fragment *formula.FragmentRecipe, id string) *formula.RecipeStep {
	t.Helper()
	for i := range fragment.Steps {
		if fragment.Steps[i].ID == id {
			return &fragment.Steps[i]
		}
	}
	t.Fatalf("fragment step %q not found", id)
	return nil
}

// TestLatestAttemptCandidateSkipsAllControlKinds pins the latest-attempt
// work-bead selection's control-infrastructure skip list to the authoritative
// vocabulary: every control kind plus the workflow topology root is skipped;
// scope beads are kind-dependent (ralph iterations ARE scopes) and plain work
// is selected.
func TestLatestAttemptCandidateSkipsAllControlKinds(t *testing.T) {
	for _, kind := range beadmeta.ControlKinds {
		if !latestAttemptCandidateIsControlInfrastructure(kind) {
			t.Errorf("latestAttemptCandidateIsControlInfrastructure(%q) = false, want true", kind)
		}
	}
	if !latestAttemptCandidateIsControlInfrastructure(beadmeta.KindWorkflow) {
		t.Error("workflow root must be skipped")
	}
	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindScope} {
		if latestAttemptCandidateIsControlInfrastructure(kind) {
			t.Errorf("latestAttemptCandidateIsControlInfrastructure(%q) = true, want false", kind)
		}
	}
}

// TestRouteFanoutFragmentStepsRouteConfigLoadFailureIsTransient is the post-merge
// remediation of PR #4175: a route-config load/parse failure on the fanout path
// must be classified as a transient controller-boundary error so the dispatcher
// retries the control bead as pending, instead of a hard failure that
// quarantines an in-flight molecule. A momentary city.toml read/parse blip is
// environmental, not a permanent defect in the workflow.
func TestRouteFanoutFragmentStepsRouteConfigLoadFailureIsTransient(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("this = = not valid toml ["), 0o644); err != nil {
		t.Fatalf("write malformed city.toml: %v", err)
	}
	fragment := &formula.FragmentRecipe{
		Name: "frag",
		Steps: []formula.RecipeStep{{
			ID: "frag.item.drain",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindDrain,
				beadmeta.RootStoreRefMetadataKey: "rig:gascity",
			},
		}},
	}
	control := beads.Bead{Metadata: map[string]string{
		beadmeta.ExecutionRoutedToMetadataKey: "gascity/worker",
		beadmeta.RootStoreRefMetadataKey:      "rig:gascity",
	}}
	// routeCfg unset so routeConfig() performs a fresh load from CityPath and
	// surfaces the malformed-config error the dispatcher must tolerate.
	opts := ProcessOptions{CityPath: cityPath}

	err := routeFanoutFragmentSteps(fragment, control, opts, beads.NewMemStore())
	if err == nil {
		t.Fatal("routeFanoutFragmentSteps: want error on malformed route config, got nil")
	}
	if !IsTransientControllerError(err) {
		t.Fatalf("route-config load failure classified hard (%v); want transient so the molecule retries as pending", err)
	}
}

// TestRouteFanoutFragmentStepsMissingDispatcherStaysTerminal pins the reserved
// fail-closed semantics of the same fix: when the route config loads
// successfully but lacks the required store-scoped control dispatcher, the error
// must stay terminal (not transient), so a genuine misconfiguration fails closed
// rather than spinning forever on retry.
func TestRouteFanoutFragmentStepsMissingDispatcherStaysTerminal(t *testing.T) {
	fragment := &formula.FragmentRecipe{
		Name: "frag",
		Steps: []formula.RecipeStep{{
			ID: "frag.item.drain",
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:         beadmeta.KindDrain,
				beadmeta.RootStoreRefMetadataKey: "rig:gascity",
			},
		}},
	}
	control := beads.Bead{Metadata: map[string]string{
		beadmeta.ExecutionRoutedToMetadataKey: "gascity/worker",
		beadmeta.RootStoreRefMetadataKey:      "rig:gascity",
	}}
	// Config loads fine but has no control-dispatcher agent scoped to rig gascity.
	routeCfg := &routeConfigCache{}
	routeCfg.once.Do(func() {
		routeCfg.cfg = &config.City{Agents: []config.Agent{{Name: "worker", Dir: "gascity"}}}
	})
	opts := ProcessOptions{routeCfg: routeCfg}

	err := routeFanoutFragmentSteps(fragment, control, opts, beads.NewMemStore())
	if err == nil {
		t.Fatal("routeFanoutFragmentSteps: want terminal error when store-scoped dispatcher is absent, got nil")
	}
	if IsTransientControllerError(err) {
		t.Fatalf("missing store-scoped dispatcher classified transient (%v); want terminal fail-closed", err)
	}
}
