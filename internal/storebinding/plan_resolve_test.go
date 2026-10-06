package storebinding

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestResolveStoragePlanLooksUpEachProviderExactlyOnce(t *testing.T) {
	work := &planCountingFactory{id: "task-beads-provider"}
	infra := &planCountingFactory{id: "infra-provider"}
	_, lookup := planRegistry(t, work, infra)

	storage := planStorageConfig(map[coordclass.Class]string{
		coordclass.ClassWork:      "task-beads",
		coordclass.ClassGraph:     "infra",
		coordclass.ClassSessions:  "infra",
		coordclass.ClassMessaging: "infra",
		coordclass.ClassOrders:    "orders",
		coordclass.ClassNudges:    "infra",
	}, map[string]config.StorageBindingConfig{
		"task-beads": {Provider: "task-beads-provider", ConfigRef: "work-production"},
		"infra":      {Provider: "infra-provider", Path: ".gc/store"},
		"orders":     {Provider: "infra-provider", Path: ".gc/orders"},
	})

	plan, err := resolveStoragePlan(lookup, storage, planWorkPins(), "")
	if err != nil {
		t.Fatalf("resolveStoragePlan: %v", err)
	}

	if got := len(lookup.lookups); got != 2 {
		t.Fatalf("registry lookups = %v, want one per distinct provider ID", lookup.lookups)
	}
	for _, id := range []ProviderID{"task-beads-provider", "infra-provider"} {
		if got := lookup.count(id); got != 1 {
			t.Fatalf("lookups for %q = %d, want exactly 1", id, got)
		}
	}
	if len(work.specs) != 1 || work.specs[0].Name != "task-beads" {
		t.Fatalf("work factory constructions = %#v, want exactly the task-beads binding", work.specs)
	}
	if len(infra.specs) != 2 {
		t.Fatalf("infra factory constructions = %#v, want one per binding sharing the provider", infra.specs)
	}
	if work.calls()+infra.calls() != 0 {
		t.Fatal("planning called a provider operation; only ProviderFactory.New is permitted before intent derivation")
	}

	// Several classes share one binding, and that binding opens once.
	infraBinding := planBinding(t, plan, "infra")
	wantInfra := planClassSet(t, coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassMessaging, coordclass.ClassNudges)
	if !infraBinding.AssignedClasses.Equal(wantInfra) {
		t.Fatalf("infra assigned classes = %v, want %v", infraBinding.AssignedClasses.Classes(), wantInfra.Classes())
	}
	if count := planOpenCount(plan, "infra"); count != 1 {
		t.Fatalf("infra open participants = %d, want 1", count)
	}
	if infraBinding.ProviderID != "infra-provider" || isNilInterface(infraBinding.Provider) {
		t.Fatalf("infra binding does not name its exact resolved provider: %#v", infraBinding)
	}
	if planBinding(t, plan, "task-beads").ProviderID == infraBinding.ProviderID {
		t.Fatal("Work and infrastructure must be free to resolve distinct providers")
	}

	// Two bindings sharing one provider ID own different provider configuration,
	// so each must be constructed from its own specification.
	ordersProvider, ok := planBinding(t, plan, "orders").Provider.(*planInertProvider)
	if !ok {
		t.Fatalf("orders binding resolved to %T", planBinding(t, plan, "orders").Provider)
	}
	if ordersProvider.spec.Name != "orders" || ordersProvider.spec.Path != ".gc/orders" {
		t.Fatalf("orders provider constructed from %#v, want its own binding specification", ordersProvider.spec)
	}
	if infraBinding.Provider == planBinding(t, plan, "orders").Provider {
		t.Fatal("two bindings sharing a provider ID reused one provider facade")
	}
}

func TestResolveStoragePlanStopsAtFirstProviderFailure(t *testing.T) {
	known := &planCountingFactory{id: "aaa-provider"}
	_, lookup := planRegistry(t, known)

	storage := planStorageConfig(map[coordclass.Class]string{
		coordclass.ClassWork:      "known",
		coordclass.ClassGraph:     "unknown",
		coordclass.ClassSessions:  "unknown",
		coordclass.ClassMessaging: "unknown",
		coordclass.ClassOrders:    "unknown",
		coordclass.ClassNudges:    "unknown",
	}, map[string]config.StorageBindingConfig{
		"known":   {Provider: "aaa-provider", ConfigRef: "known"},
		"unknown": {Provider: "zzz-missing-provider", ConfigRef: "missing"},
	})

	plan, err := resolveStoragePlan(lookup, storage, planWorkPins(), "")
	if plan != nil || !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("resolveStoragePlan = %v, %v; want no plan and an unknown-provider error", plan, err)
	}
	if len(lookup.lookups) != 2 || lookup.lookups[1] != "zzz-missing-provider" {
		t.Fatalf("lookups = %v, want resolution to stop at the first failure", lookup.lookups)
	}
	if len(known.specs) != 0 {
		t.Fatal("planning constructed a provider before every lookup succeeded")
	}
}

func TestResolveStoragePlanRejectsInexactConfigurationBeforeAnyProviderMutation(t *testing.T) {
	valid := map[string]config.StorageBindingConfig{"infra": {Provider: "infra-provider", Path: ".gc/store"}}
	cases := []struct {
		name    string
		classes map[coordclass.Class]string
		binding map[string]config.StorageBindingConfig
		pins    *WorkPinInputs
		want    error
	}{
		{
			name:    "missing class assignment",
			classes: map[coordclass.Class]string{coordclass.ClassWork: "work"},
			binding: valid,
			want:    ErrMissingClassAssignment,
		},
		{
			name:    "undefined binding reference",
			classes: planAllClassesOn("ghost"),
			binding: valid,
			want:    ErrUndefinedBinding,
		},
		{
			name:    "reserved binding defined",
			classes: planAllClassesOn("work"),
			binding: map[string]config.StorageBindingConfig{"work": {Provider: "infra-provider"}},
			want:    ErrReservedBindingDefined,
		},
		{
			name:    "unreferenced binding definition",
			classes: planAllClassesOn("work"),
			binding: valid,
			want:    ErrUnreferencedBinding,
		},
		{
			name:    "invalid binding specification",
			classes: planAllClassesOn("infra"),
			binding: map[string]config.StorageBindingConfig{"infra": {Provider: "infra provider"}},
			want:    ErrInvalidBindingSpec,
		},
		{
			name:    "aliased work pins",
			classes: planAllClassesOn("work"),
			binding: nil,
			pins:    planAliasedPins(),
			want:    ErrInvalidWorkPin,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			factory := &planCountingFactory{id: "infra-provider"}
			_, lookup := planRegistry(t, factory)
			pins := planWorkPins()
			if testCase.pins != nil {
				pins = *testCase.pins
			}
			plan, err := resolveStoragePlan(lookup, planStorageConfig(testCase.classes, testCase.binding), pins, "")
			if plan != nil {
				t.Fatalf("resolveStoragePlan returned a partial plan: %#v", plan)
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("resolveStoragePlan error = %v, want %v", err, testCase.want)
			}
			if factory.calls() != 0 {
				t.Fatal("a rejected plan touched a provider operation")
			}
		})
	}
}

func TestResolveStoragePlanFreezesDefaultAllClassesOnReservedWork(t *testing.T) {
	registry, _ := planRegistry(t)
	city := &config.City{}

	plan, err := ResolveStoragePlan(registry, city.EffectiveStorage(), planWorkPins(), "")
	if err != nil {
		t.Fatalf("ResolveStoragePlan: %v", err)
	}

	for _, class := range planClassOrder() {
		binding, assigned := plan.BindingFor(class)
		if !assigned || binding != ReservedWorkBinding {
			t.Fatalf("class %s assigned to %q, want the reserved work binding", class, binding)
		}
	}
	if len(plan.Bindings()) != 0 {
		t.Fatalf("default plan defines explicit bindings: %#v", plan.Bindings())
	}
	open := plan.OpenProgram()
	if len(open) != 1 || !open[0].Reserved || !open[0].PinnedWork {
		t.Fatalf("default open program = %#v, want the pinned reserved binding alone", open)
	}
	if !isNilInterface(open[0].Provider) {
		t.Fatal("the reserved binding must carry no provider facade")
	}
	closes := plan.CloseProgram()
	if len(closes) != 1 || closes[0].Binding != ReservedWorkBinding || len(closes[0].PinnedWork) != 3 {
		t.Fatalf("default close program = %#v, want the reserved binding annotated with its pinned physical workspaces", closes)
	}
	if plan.NudgeQueueAuthority() != NudgeQueueRetainedLegacy {
		t.Fatalf("nudge queue authority = %v, want the retained legacy queue", plan.NudgeQueueAuthority())
	}
	deferred := plan.DeferredBinds()
	if len(deferred) != 1 || deferred[0].Kind != DeferredBindMessagingSessions {
		t.Fatalf("deferred binds = %#v, want only the Messaging bind when Orders and Graph share a binding", deferred)
	}
}

func TestResolveStoragePlanOrdersOpenCloseAndInspectionPrograms(t *testing.T) {
	plan := planMixedPlan(t)

	open := plan.OpenProgram()
	gotOpen := make([]BindingName, 0, len(open))
	for index, step := range open {
		if step.Rank != index {
			t.Fatalf("open step %d has rank %d", index, step.Rank)
		}
		gotOpen = append(gotOpen, step.Binding)
	}
	wantOpen := []BindingName{ReservedWorkBinding, "task-beads", "infra", "orders"}
	if !planSameNames(gotOpen, wantOpen) {
		t.Fatalf("open order = %v, want %v", gotOpen, wantOpen)
	}

	closes := plan.CloseProgram()
	gotClose := make([]BindingName, 0, len(closes))
	for _, step := range closes {
		gotClose = append(gotClose, step.Binding)
	}
	wantClose := []BindingName{"orders", "infra", "task-beads", ReservedWorkBinding}
	if !planSameNames(gotClose, wantClose) {
		t.Fatalf("close order = %v, want the exact reverse of the open order %v", gotClose, wantOpen)
	}

	inspections := plan.InspectionProgram()
	gotInspect := make([]BindingName, 0, len(inspections))
	for _, step := range inspections {
		if isNilInterface(step.Provider) {
			t.Fatalf("inspection step %q carries no resolved provider", step.Binding)
		}
		gotInspect = append(gotInspect, step.Binding)
	}
	wantInspect := []BindingName{"infra", "orders", "task-beads"}
	if !planSameNames(gotInspect, wantInspect) {
		t.Fatalf("inspection order = %v, want infrastructure classes before Work %v", gotInspect, wantInspect)
	}
}

func TestResolveStoragePlanKeepsReservedWorkAddressableAfterWorkMoves(t *testing.T) {
	plan := planMovedWorkPlan(t)

	if binding, _ := plan.BindingFor(coordclass.ClassWork); binding != "task-beads" {
		t.Fatalf("work class binding = %q, want the moved binding", binding)
	}
	step, planned := plan.openStep(ReservedWorkBinding)
	if !planned {
		t.Fatal("the reserved binding left the open program while an infrastructure class still selects it")
	}
	if !step.Reserved || step.PinnedWork {
		t.Fatalf("reserved open step = %#v, want a reserved participant without a live Work front", step)
	}
	if !step.AssignedClasses.Equal(planClassSet(t, coordclass.ClassGraph, coordclass.ClassOrders)) {
		t.Fatalf("reserved assigned classes = %v, want the retained infrastructure classes", step.AssignedClasses.Classes())
	}
	if !plan.WorkPlan().Present() || plan.WorkPlan().HQ().PhysicalID == "" {
		t.Fatal("the pinned bootstrap Work topology must survive a Work move")
	}
	participants, err := plan.WorkParticipants()
	if err != nil {
		t.Fatalf("WorkParticipants: %v", err)
	}
	if len(participants) != 3 {
		t.Fatalf("work participants = %d, want one per pinned physical workspace", len(participants))
	}
	if digest := plan.BindingConfigDigests()[ReservedWorkBinding]; digest != plan.WorkPlan().ConfigContext() {
		t.Fatalf("reserved binding config digest = %q, want the pinned config context", digest)
	}
}

func TestResolveStoragePlanPlansOrdersGraphBindOnlyForSplitAssignments(t *testing.T) {
	split := planMixedPlan(t)
	if !split.hasDeferredBind(DeferredBindOrdersGraph) {
		t.Fatal("split Orders and Graph assignments require a planned Orders-from-Graph bind")
	}
	for _, deferred := range split.DeferredBinds() {
		if deferred.Kind != DeferredBindOrdersGraph {
			continue
		}
		if deferred.Consumer != "orders" || deferred.Supplier != "infra" {
			t.Fatalf("orders-graph bind = %#v, want orders consuming the graph binding", deferred)
		}
	}

	shared := planMovedWorkPlan(t)
	if shared.hasDeferredBind(DeferredBindOrdersGraph) {
		t.Fatal("Orders and Graph sharing a binding must not plan a second bind step")
	}
}

func TestResolveStoragePlanRequiresFrozenRegistry(t *testing.T) {
	registry := NewProviderRegistry()
	if _, err := ResolveStoragePlan(registry, (&config.City{}).EffectiveStorage(), planWorkPins(), ""); !errors.Is(err, ErrProviderRegistryNotFrozen) {
		t.Fatalf("ResolveStoragePlan on an unfrozen registry = %v, want %v", err, ErrProviderRegistryNotFrozen)
	}
	if _, err := ResolveStoragePlan(nil, (&config.City{}).EffectiveStorage(), planWorkPins(), ""); !errors.Is(err, ErrInvalidStoragePlan) {
		t.Fatalf("ResolveStoragePlan without a registry = %v, want an invalid-plan error", err)
	}
}

func TestResolveStoragePlanAccessorsReturnDetachedValues(t *testing.T) {
	plan := planMixedPlan(t)

	assignments := plan.Assignments()
	assignments[coordclass.ClassGraph] = "tampered"
	if binding, _ := plan.BindingFor(coordclass.ClassGraph); binding == "tampered" {
		t.Fatal("Assignments returned the plan's own map")
	}

	open := plan.OpenProgram()
	open[0].Binding = "tampered"
	open[0].Requirements[0].RequireClaims = !open[0].Requirements[0].RequireClaims
	if plan.OpenProgram()[0].Binding == "tampered" || plan.OpenProgram()[0].Requirements[0] == open[0].Requirements[0] {
		t.Fatal("OpenProgram returned the plan's own steps")
	}

	closes := plan.CloseProgram()
	if len(closes[len(closes)-1].PinnedWork) == 0 {
		t.Fatal("the reserved close step lost its pinned physical annotation")
	}
	closes[len(closes)-1].PinnedWork[0].Scopes[0] = RigScope("tampered")
	if plan.CloseProgram()[len(closes)-1].PinnedWork[0].Scopes[0] == RigScope("tampered") {
		t.Fatal("CloseProgram returned the plan's own pinned groups")
	}
}

func TestPlannedOpenTemplateCompletesTheOpenRequestFromDurableFacts(t *testing.T) {
	plan := planMixedPlan(t)
	step, ok := plan.openStep("infra")
	if !ok {
		t.Fatal("infra binding is not in the open program")
	}
	descriptor := planDescriptor(t, "infra", "infra-provider", step.AssignedClasses, planCapabilities(step.AssignedClasses))
	authority, err := NewDurableActiveOpenAuthority(7, descriptor)
	if err != nil {
		t.Fatalf("NewDurableActiveOpenAuthority: %v", err)
	}

	request := step.OpenRequestTemplate(descriptor, 7, authority, nil)
	if err := request.Validate(); err != nil {
		t.Fatalf("planned open request is not valid: %v", err)
	}
	if request.Mode != OpenModeActive {
		t.Fatalf("planned open mode = %v, want the active generation", request.Mode)
	}
	if len(request.ExpectedComponents) != len(descriptor.Components) {
		t.Fatalf("expected components = %#v, want the complete pinned set", request.ExpectedComponents)
	}
}

func planAliasedPins() *WorkPinInputs {
	pins := planWorkPins()
	pins.Rigs[1].Prefix = pins.HQ.Prefix
	return &pins
}

// planMixedPlan resolves Work onto its own provider, four infrastructure
// classes onto a shared binding, and Orders onto a third binding.
func planMixedPlan(t *testing.T) *StoragePlan {
	t.Helper()
	registry, _ := planRegistry(t,
		&planCountingFactory{id: "task-beads-provider"},
		&planCountingFactory{id: "infra-provider"},
	)
	storage := planStorageConfig(map[coordclass.Class]string{
		coordclass.ClassWork:      "task-beads",
		coordclass.ClassGraph:     "infra",
		coordclass.ClassSessions:  "infra",
		coordclass.ClassMessaging: "infra",
		coordclass.ClassOrders:    "orders",
		coordclass.ClassNudges:    "infra",
	}, map[string]config.StorageBindingConfig{
		"task-beads": {Provider: "task-beads-provider", ConfigRef: "work-production"},
		"infra":      {Provider: "infra-provider", Path: ".gc/store"},
		"orders":     {Provider: "infra-provider", Path: ".gc/orders"},
	})
	// The reserved binding still serves Sessions so the mixed plan exercises a
	// four-participant program.
	storage.Classes.Sessions = string(ReservedWorkBinding)
	plan, err := ResolveStoragePlan(registry, storage, planWorkPins(), "")
	if err != nil {
		t.Fatalf("ResolveStoragePlan: %v", err)
	}
	return plan
}

// planMovedWorkPlan moves Work to an explicit binding while Graph stays on the
// reserved binding.
func planMovedWorkPlan(t *testing.T) *StoragePlan {
	t.Helper()
	registry, _ := planRegistry(t,
		&planCountingFactory{id: "task-beads-provider"},
		&planCountingFactory{id: "infra-provider"},
	)
	storage := planStorageConfig(map[coordclass.Class]string{
		coordclass.ClassWork:      "task-beads",
		coordclass.ClassGraph:     string(ReservedWorkBinding),
		coordclass.ClassSessions:  "infra",
		coordclass.ClassMessaging: "infra",
		coordclass.ClassOrders:    string(ReservedWorkBinding),
		coordclass.ClassNudges:    "infra",
	}, map[string]config.StorageBindingConfig{
		"task-beads": {Provider: "task-beads-provider", ConfigRef: "work-production"},
		"infra":      {Provider: "infra-provider", Path: ".gc/store"},
	})
	plan, err := ResolveStoragePlan(registry, storage, planWorkPins(), "")
	if err != nil {
		t.Fatalf("ResolveStoragePlan: %v", err)
	}
	return plan
}

func planBinding(t *testing.T, plan *StoragePlan, name BindingName) PlannedBinding {
	t.Helper()
	for _, binding := range plan.Bindings() {
		if binding.Name == name {
			return binding
		}
	}
	t.Fatalf("plan has no binding %q", name)
	return PlannedBinding{}
}

func planOpenCount(plan *StoragePlan, name BindingName) int {
	count := 0
	for _, step := range plan.OpenProgram() {
		if step.Binding == name {
			count++
		}
	}
	return count
}

func planSameNames(got, want []BindingName) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
