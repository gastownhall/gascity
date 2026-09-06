package storebinding

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

const planTestGeneration Generation = 9

// planMixedHandles opens exactly the participants the mixed plan names, with
// close counters so a test can observe that the builder closes nothing.
type planMixedFixtures struct {
	plan     *StoragePlan
	byName   map[BindingName]*planOpenedBinding
	events   []string
	messages *planRecordingBinder
}

func planBuildFixtures(t *testing.T, plan *StoragePlan) *planMixedFixtures {
	t.Helper()
	fixtures := &planMixedFixtures{plan: plan, byName: map[BindingName]*planOpenedBinding{}}
	for _, step := range plan.OpenProgram() {
		provider := ProviderID("reserved-beads")
		if !step.Reserved {
			provider = step.Spec.Provider
		}
		options := planOpenOptions{provider: provider, classes: step.AssignedClasses}
		if step.AssignedClasses.HasWork() {
			pins := planWorkPins()
			options.pins = &pins
		}
		fixtures.byName[step.Binding] = planOpen(t, step.Binding, options)
	}
	return fixtures
}

func (f *planMixedFixtures) inputs(t *testing.T) BuildInputs {
	t.Helper()
	var inputs BuildInputs
	for _, step := range f.plan.OpenProgram() {
		fixture := f.byName[step.Binding]
		handle := planHandle(t, fixture, planTestGeneration)
		if step.AssignedClasses.Has(coordclass.ClassMessaging) || step.AssignedClasses.Has(coordclass.ClassOrders) {
			recording := &planRecordingBinding{OpenedBinding: fixture.opened, events: &f.events}
			handle.Opened = recording
			if binder, ok := recording.Messaging(); ok {
				f.messages, _ = binder.(*planRecordingBinder)
			}
		}
		inputs.Handles = append(inputs.Handles, handle)
	}
	return inputs
}

func (f *planMixedFixtures) closes() int {
	total := 0
	for _, fixture := range f.byName {
		total += fixture.closes
	}
	return total
}

func TestStoreSetBuilderBuildsOneCandidateFromExactActiveHandles(t *testing.T) {
	plan := planMixedPlan(t)
	fixtures := planBuildFixtures(t, plan)
	builder, err := NewStoreSetBuilder(plan)
	if err != nil {
		t.Fatalf("NewStoreSetBuilder: %v", err)
	}

	candidate, err := builder.Build(context.Background(), fixtures.inputs(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	set, err := candidate.Publish(planPublicationAuthority(t, candidate))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if isNilInterface(set.Graph()) || isNilInterface(set.Sessions()) || isNilInterface(set.Orders()) {
		t.Fatal("published StoreSet has an empty typed front")
	}
	if !messagingFrontDoorsUsable(set.Messaging()) || !nudgeFrontDoorsUsable(set.Nudges()) {
		t.Fatal("published StoreSet has an incomplete aggregate front")
	}
	if err := plan.WorkPlan().checkTopology(set.Work(), false); err != nil {
		t.Fatalf("published Work topology does not match the pins: %v", err)
	}
	if fixtures.closes() != 0 {
		t.Fatalf("builder closed %d handles; it owns none of them", fixtures.closes())
	}

	if _, err := builder.Build(context.Background(), fixtures.inputs(t)); !errors.Is(err, ErrStoreSetAlreadyBuilt) {
		t.Fatalf("second Build = %v, want %v", err, ErrStoreSetAlreadyBuilt)
	}
}

func TestStoreSetBuilderRequiresTheExactPlannedHandleSet(t *testing.T) {
	plan := planMixedPlan(t)
	cases := []struct {
		name   string
		mutate func(*BuildInputs, *planMixedFixtures)
	}{
		{
			name:   "missing handle",
			mutate: func(in *BuildInputs, _ *planMixedFixtures) { in.Handles = in.Handles[1:] },
		},
		{
			name: "extra handle",
			mutate: func(in *BuildInputs, _ *planMixedFixtures) {
				in.Handles = append(in.Handles, in.Handles[0])
			},
		},
		{
			name: "duplicate handle",
			mutate: func(in *BuildInputs, _ *planMixedFixtures) {
				in.Handles[len(in.Handles)-1] = in.Handles[0]
			},
		},
		{
			name: "unplanned binding",
			mutate: func(in *BuildInputs, _ *planMixedFixtures) {
				in.Handles[0].Binding = "unplanned"
			},
		},
		{
			name: "missing opened handle",
			mutate: func(in *BuildInputs, _ *planMixedFixtures) {
				in.Handles[0].Opened = nil
			},
		},
		{
			name: "missing durable active authority",
			mutate: func(in *BuildInputs, _ *planMixedFixtures) {
				in.Handles[0].Authority = DurableActiveOpenAuthority{}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixtures := planBuildFixtures(t, plan)
			builder, err := NewStoreSetBuilder(plan)
			if err != nil {
				t.Fatalf("NewStoreSetBuilder: %v", err)
			}
			inputs := fixtures.inputs(t)
			testCase.mutate(&inputs, fixtures)
			if _, err := builder.Build(context.Background(), inputs); !errors.Is(err, ErrInvalidStoreSetBuild) {
				t.Fatalf("Build error = %v, want %v", err, ErrInvalidStoreSetBuild)
			}
			if fixtures.closes() != 0 {
				t.Fatal("a rejected build closed a handle it does not own")
			}
			if fixtures.messages != nil && fixtures.messages.binds != 0 {
				t.Fatal("a rejected build consumed the one-shot Messaging binder")
			}
		})
	}
}

func TestStoreSetBuilderRejectsDriftedDescriptorsCapabilitiesAndFronts(t *testing.T) {
	plan := planMixedPlan(t)

	t.Run("descriptor identity differs from its authority", func(t *testing.T) {
		fixtures := planBuildFixtures(t, plan)
		inputs := fixtures.inputs(t)
		foreign := planOpen(t, "foreign", planOpenOptions{classes: planClassSet(t, coordclass.ClassGraph)})
		authority, err := NewDurableActiveOpenAuthority(planTestGeneration, foreign.descriptor)
		if err != nil {
			t.Fatalf("NewDurableActiveOpenAuthority: %v", err)
		}
		inputs.Handles[0].Authority = authority
		planExpectBuildRejection(t, plan, fixtures, inputs, ErrInvalidOpenAuthority)
	})

	t.Run("generation differs between handles", func(t *testing.T) {
		fixtures := planBuildFixtures(t, plan)
		inputs := fixtures.inputs(t)
		last := len(inputs.Handles) - 1
		authority, err := NewDurableActiveOpenAuthority(planTestGeneration+1, fixtures.byName[inputs.Handles[last].Binding].descriptor)
		if err != nil {
			t.Fatalf("NewDurableActiveOpenAuthority: %v", err)
		}
		inputs.Handles[last].Authority = authority
		planExpectBuildRejection(t, plan, fixtures, inputs, ErrInvalidStoreSetBuild)
	})

	t.Run("degraded class capability", func(t *testing.T) {
		fixtures := planBuildFixtures(t, plan)
		graphBinding, _ := plan.BindingFor(coordclass.ClassGraph)
		classes := planOpenClasses(t, plan, graphBinding)
		degraded := planCapabilities(classes)
		degraded = planWithCapability(degraded, coordclass.ClassGraph, ClassCapability{Available: true, Transactions: true})
		fixtures.byName[graphBinding] = planOpen(t, graphBinding, planOpenOptions{
			provider:     "infra-provider",
			classes:      classes,
			capabilities: &degraded,
		})
		planExpectBuildRejection(t, plan, fixtures, fixtures.inputs(t), ErrMissingCapability)
	})

	t.Run("absent class front", func(t *testing.T) {
		fixtures := planBuildFixtures(t, plan)
		inputs := fixtures.inputs(t)
		graphBinding, _ := plan.BindingFor(coordclass.ClassGraph)
		for index, handle := range inputs.Handles {
			if handle.Binding != graphBinding {
				continue
			}
			inputs.Handles[index].Opened = &planFrontDroppingBinding{OpenedBinding: handle.Opened, drop: coordclass.ClassGraph}
		}
		planExpectBuildRejection(t, plan, fixtures, inputs, ErrInvalidStoreSetBuild)
	})

	t.Run("pinned work drift", func(t *testing.T) {
		fixtures := planBuildFixtures(t, plan)
		workBinding, _ := plan.BindingFor(coordclass.ClassWork)
		drifted := planWorkPins()
		drifted.Rigs = append([]WorkScopePin(nil), planWorkPins().Rigs...)
		drifted.Rigs[0].Prefix = "renamed"
		fixtures.byName[workBinding] = planOpen(t, workBinding, planOpenOptions{
			provider: "task-beads-provider",
			classes:  planOpenClasses(t, plan, workBinding),
			pins:     &drifted,
		})
		planExpectBuildRejection(t, plan, fixtures, fixtures.inputs(t), ErrWorkTopologyDrift)
	})
}

// TestStoreSetBuilderRetriesAfterFailureWithoutConsumingTheMessagingBinder
// proves the bind-last discipline: a build that fails validation leaves every
// handle open, unconsumed, and re-buildable by the same builder.
func TestStoreSetBuilderRetriesAfterFailureWithoutConsumingTheMessagingBinder(t *testing.T) {
	plan := planMixedPlan(t)
	fixtures := planBuildFixtures(t, plan)
	builder, err := NewStoreSetBuilder(plan)
	if err != nil {
		t.Fatalf("NewStoreSetBuilder: %v", err)
	}

	broken := fixtures.inputs(t)
	broken.Handles = broken.Handles[1:]
	if _, err := builder.Build(context.Background(), broken); err == nil {
		t.Fatal("Build accepted an incomplete handle set")
	}
	if fixtures.messages != nil && fixtures.messages.binds != 0 {
		t.Fatal("a failed build consumed the Messaging binder")
	}

	if _, err := builder.Build(context.Background(), fixtures.inputs(t)); err != nil {
		t.Fatalf("retry after a failed build: %v", err)
	}
	if fixtures.closes() != 0 {
		t.Fatal("the builder closed a handle it does not own")
	}
}

func TestStoreSetBuilderBindsOrdersThenMessagingLast(t *testing.T) {
	plan := planMixedPlan(t)
	fixtures := planBuildFixtures(t, plan)
	builder, err := NewStoreSetBuilder(plan)
	if err != nil {
		t.Fatalf("NewStoreSetBuilder: %v", err)
	}
	if _, err := builder.Build(context.Background(), fixtures.inputs(t)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{"orders-graph", "messaging-sessions"}
	if len(fixtures.events) != len(want) {
		t.Fatalf("deferred bind order = %v, want %v", fixtures.events, want)
	}
	for index := range want {
		if fixtures.events[index] != want[index] {
			t.Fatalf("deferred bind order = %v, want %v", fixtures.events, want)
		}
	}
}

func TestStoreSetBuilderSkipsTheOrdersGraphBindWhenOrdersAndGraphShareABinding(t *testing.T) {
	plan := planMovedWorkPlan(t)
	fixtures := planBuildFixtures(t, plan)
	builder, err := NewStoreSetBuilder(plan)
	if err != nil {
		t.Fatalf("NewStoreSetBuilder: %v", err)
	}
	if _, err := builder.Build(context.Background(), fixtures.inputs(t)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, event := range fixtures.events {
		if event == "orders-graph" {
			t.Fatal("a shared Orders/Graph binding must not run a second bind step")
		}
	}
	if len(fixtures.events) != 1 || fixtures.events[0] != "messaging-sessions" {
		t.Fatalf("deferred binds = %v, want only the Messaging bind", fixtures.events)
	}
}

// TestStoreSetBuilderChecksReservedWorkIdentity pins the moved-Work rule: while
// Work is still served by the reserved binding, the bootstrap identity triples
// are compared, not merely the semantic facts.
func TestStoreSetBuilderChecksReservedWorkIdentity(t *testing.T) {
	registry, _ := planRegistry(t)
	plan, err := ResolveStoragePlan(registry, planStorageConfig(planAllClassesOn(string(ReservedWorkBinding)), nil), planWorkPins(), "")
	if err != nil {
		t.Fatalf("ResolveStoragePlan: %v", err)
	}
	fixtures := planBuildFixtures(t, plan)
	relocated := planWorkPins()
	relocated.HQ.PhysicalID = "relocated-physical"
	fixtures.byName[ReservedWorkBinding] = planOpen(t, ReservedWorkBinding, planOpenOptions{
		provider: "reserved-beads",
		classes:  planOpenClasses(t, plan, ReservedWorkBinding),
		pins:     &relocated,
	})
	planExpectBuildRejection(t, plan, fixtures, fixtures.inputs(t), ErrWorkTopologyDrift)
}

func TestNewStoreSetBuilderRejectsAnAbsentPlan(t *testing.T) {
	if _, err := NewStoreSetBuilder(nil); !errors.Is(err, ErrInvalidStoreSetBuild) {
		t.Fatalf("NewStoreSetBuilder(nil) = %v, want %v", err, ErrInvalidStoreSetBuild)
	}
}

func planExpectBuildRejection(t *testing.T, plan *StoragePlan, fixtures *planMixedFixtures, inputs BuildInputs, want error) {
	t.Helper()
	builder, err := NewStoreSetBuilder(plan)
	if err != nil {
		t.Fatalf("NewStoreSetBuilder: %v", err)
	}
	candidate, err := builder.Build(context.Background(), inputs)
	if err == nil {
		t.Fatal("Build accepted drifted inputs")
	}
	if !errors.Is(err, want) {
		t.Fatalf("Build error = %v, want %v", err, want)
	}
	if _, publishErr := candidate.Publish(StoreSetPublicationAuthority{}); !errors.Is(publishErr, ErrUnpublishedStoreSetInvalid) {
		t.Fatalf("rejected build returned a publishable candidate: %v", publishErr)
	}
	if fixtures.closes() != 0 {
		t.Fatal("a rejected build closed a handle it does not own")
	}
	if fixtures.messages != nil && fixtures.messages.binds != 0 {
		t.Fatal("a rejected build consumed the one-shot Messaging binder")
	}
}

func planOpenClasses(t *testing.T, plan *StoragePlan, binding BindingName) ClassSet {
	t.Helper()
	step, ok := plan.openStep(binding)
	if !ok {
		t.Fatalf("plan has no open step for %q", binding)
	}
	return step.AssignedClasses
}

// planRecordingBinding observes the order of the deferred composition steps
// without changing what they do.
type planRecordingBinding struct {
	OpenedBinding
	events *[]string
	binder *planRecordingBinder
}

func (b *planRecordingBinding) Messaging() (MessagingFrontDoorBinder, bool) {
	inner, ok := b.OpenedBinding.Messaging()
	if !ok {
		return nil, false
	}
	if b.binder == nil {
		b.binder = &planRecordingBinder{inner: inner, events: b.events}
	}
	return b.binder, true
}

func (b *planRecordingBinding) Orders() (OrdersStore, bool) {
	inner, ok := b.OpenedBinding.Orders()
	if !ok {
		return nil, false
	}
	binder, isBinder := inner.(OrdersGraphBinder)
	if !isBinder {
		return inner, true
	}
	return &planRecordingOrders{OrdersStore: inner, inner: binder, events: b.events}, true
}

type planRecordingBinder struct {
	inner  MessagingFrontDoorBinder
	events *[]string
	binds  int
}

func (b *planRecordingBinder) BindSessions(directory SessionsAddressDirectory) (MessagingFrontDoors, error) {
	b.binds++
	*b.events = append(*b.events, "messaging-sessions")
	return b.inner.BindSessions(directory)
}

type planRecordingOrders struct {
	OrdersStore
	inner  OrdersGraphBinder
	events *[]string
}

func (o *planRecordingOrders) BindGraph(graph GraphStore) OrdersStore {
	*o.events = append(*o.events, "orders-graph")
	return o.inner.BindGraph(graph)
}

// planFrontDroppingBinding hides one typed front an assigned class requires.
type planFrontDroppingBinding struct {
	OpenedBinding
	drop coordclass.Class
}

func (b *planFrontDroppingBinding) Graph() (GraphStore, bool) {
	if b.drop == coordclass.ClassGraph {
		return nil, false
	}
	return b.OpenedBinding.Graph()
}

func (b *planFrontDroppingBinding) Sessions() (SessionsStore, bool) {
	if b.drop == coordclass.ClassSessions {
		return nil, false
	}
	return b.OpenedBinding.Sessions()
}

func (b *planFrontDroppingBinding) Messaging() (MessagingFrontDoorBinder, bool) {
	if b.drop == coordclass.ClassMessaging {
		return nil, false
	}
	return b.OpenedBinding.Messaging()
}

func (b *planFrontDroppingBinding) Orders() (OrdersStore, bool) {
	if b.drop == coordclass.ClassOrders {
		return nil, false
	}
	return b.OpenedBinding.Orders()
}

func (b *planFrontDroppingBinding) Nudges() (NudgeFrontDoors, bool) {
	if b.drop == coordclass.ClassNudges {
		return NudgeFrontDoors{}, false
	}
	return b.OpenedBinding.Nudges()
}

func (b *planFrontDroppingBinding) Work() (WorkTopology, bool) {
	if b.drop == coordclass.ClassWork {
		return WorkTopology{}, false
	}
	return b.OpenedBinding.Work()
}
