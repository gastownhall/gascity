package storebinding

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestPlanRequiresRestartComparesFrozenFactsWithoutTouchingAProvider(t *testing.T) {
	baseline := planReloadConfig()
	current, currentFactory := planResolveForReload(t, baseline, planWorkPins())
	same, sameFactory := planResolveForReload(t, baseline, planWorkPins())

	before := planSnapshot(t, current)
	if PlanRequiresRestart(current, same) {
		t.Fatal("identical configuration reported restart required")
	}
	if currentFactory.calls()+sameFactory.calls() != 0 {
		t.Fatal("reload comparison called a provider operation")
	}
	if after := planSnapshot(t, current); !reflect.DeepEqual(before, after) {
		t.Fatal("reload comparison mutated the frozen plan")
	}
}

func TestPlanRequiresRestartDetectsEveryFrozenFactChange(t *testing.T) {
	cases := []struct {
		name    string
		storage func() config.StorageConfig
		pins    func() WorkPinInputs
	}{
		{
			name: "class assignment",
			storage: func() config.StorageConfig {
				storage := planReloadConfig()
				storage.Classes.Nudges = "orders"
				return storage
			},
		},
		{
			name: "provider ID",
			storage: func() config.StorageConfig {
				storage := planReloadConfig()
				storage.Bindings["infra"] = config.StorageBindingConfig{Provider: "task-beads-provider", ConfigRef: "infra"}
				return storage
			},
		},
		{
			name: "provider configuration",
			storage: func() config.StorageConfig {
				storage := planReloadConfig()
				storage.Bindings["infra"] = config.StorageBindingConfig{Provider: "infra-provider", Path: ".gc/relocated"}
				return storage
			},
		},
		{
			name: "pinned physical identity",
			pins: func() WorkPinInputs {
				pins := planWorkPins()
				pins.HQ.PhysicalID = "relocated-physical"
				return pins
			},
		},
		{
			name: "pinned suspension",
			pins: func() WorkPinInputs {
				pins := planWorkPins()
				pins.Rigs = append([]WorkScopePin(nil), planWorkPins().Rigs...)
				pins.Rigs[0].Suspended = false
				return pins
			},
		},
		{
			name: "pinned config context",
			pins: func() WorkPinInputs {
				pins := planWorkPins()
				pins.ConfigContext = ConfigRefDigest(canonicalDigest([]byte("other-config-context")))
				return pins
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			storage, pins := planReloadConfig(), planWorkPins()
			current, _ := planResolveForReload(t, storage, pins)
			if testCase.storage != nil {
				storage = testCase.storage()
			}
			if testCase.pins != nil {
				pins = testCase.pins()
			}
			next, factory := planResolveForReload(t, storage, pins)
			if !PlanRequiresRestart(current, next) {
				t.Fatal("changed frozen facts did not report restart required")
			}
			if factory.calls() != 0 {
				t.Fatal("reload comparison called a provider operation")
			}
		})
	}
}

func TestPlanRequiresRestartReportsRestartForAnAbsentPlan(t *testing.T) {
	current, _ := planResolveForReload(t, planReloadConfig(), planWorkPins())
	if !PlanRequiresRestart(nil, current) || !PlanRequiresRestart(current, nil) || !PlanRequiresRestart(nil, nil) {
		t.Fatal("an absent frozen plan must report restart required")
	}
}

func planReloadConfig() config.StorageConfig {
	return planStorageConfig(map[coordclass.Class]string{
		coordclass.ClassWork:      string(ReservedWorkBinding),
		coordclass.ClassGraph:     "infra",
		coordclass.ClassSessions:  "infra",
		coordclass.ClassMessaging: "infra",
		coordclass.ClassOrders:    "orders",
		coordclass.ClassNudges:    "infra",
	}, map[string]config.StorageBindingConfig{
		"infra":  {Provider: "infra-provider", Path: ".gc/store"},
		"orders": {Provider: "infra-provider", Path: ".gc/orders"},
	})
}

func planResolveForReload(t *testing.T, storage config.StorageConfig, pins WorkPinInputs) (*StoragePlan, *planCountingFactory) {
	t.Helper()
	factory := &planCountingFactory{id: "infra-provider"}
	registry, _ := planRegistry(t, factory, &planCountingFactory{id: "task-beads-provider"})
	plan, err := ResolveStoragePlan(registry, storage, pins, "")
	if err != nil {
		t.Fatalf("ResolveStoragePlan: %v", err)
	}
	return plan, factory
}

// planSnapshot captures every frozen fact a reload comparison may read, so a
// mutation during comparison is observable rather than assumed absent.
func planSnapshot(t *testing.T, plan *StoragePlan) []any {
	t.Helper()
	participants, err := plan.WorkParticipants()
	if err != nil {
		t.Fatalf("WorkParticipants: %v", err)
	}
	return []any{
		plan.Assignments(),
		planBindingFacts(plan),
		plan.OpenProgram(),
		plan.CloseProgram(),
		plan.InspectionProgram(),
		plan.DeferredBinds(),
		plan.WorkPlan().All(),
		plan.WorkPlan().Physical(),
		participants,
		plan.ConfigDigest(),
		plan.BindingConfigDigests(),
		plan.NudgeQueueAuthority(),
	}
}

func planBindingFacts(plan *StoragePlan) []string {
	facts := make([]string, 0, len(plan.Bindings()))
	for _, binding := range plan.Bindings() {
		facts = append(facts, string(binding.Name)+"|"+string(binding.ProviderID)+"|"+string(binding.ConfigDigest)+"|"+binding.Spec.Path)
	}
	return facts
}
