package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestCityStatusSchemaAcceptsPopulatedConditionalWrites(t *testing.T) {
	payload := populatedConditionalWritesSchemaInstance(t)
	block, ok := payload["conditional_writes"].(map[string]any)
	if !ok {
		t.Fatal("populated conditional_writes omitted from serialized status")
	}
	stores := block["stores"].([]any)
	if len(stores) != 2 || stores[1].(map[string]any)["reason"] == nil {
		t.Fatal("serialized status lost store verdicts or their reason")
	}
	notices := block["notices"].([]any)
	if len(notices) != 1 || notices[0].(map[string]any)["env_value"] != "require" {
		t.Fatal("serialized status lost the rollout notice")
	}
	if err := validateStatusSchemaInstance(t, payload); err != nil {
		t.Fatalf("populated conditional_writes must validate: %v", err)
	}
}

func TestCityStatusSchemaAcceptsConditionalWritesWithoutOptionalCollections(t *testing.T) {
	payload := statusSchemaInstance(t, cityStatusSnapshot{
		ConditionalWrites: &api.StatusConditionalWrites{Mode: "off", Origin: "builtin", Effective: "off"},
	})
	block := payload["conditional_writes"].(map[string]any)
	for _, field := range []string{"stores", "notices"} {
		if _, present := block[field]; present {
			t.Fatalf("empty optional %s must be omitted", field)
		}
	}
	if err := validateStatusSchemaInstance(t, payload); err != nil {
		t.Fatalf("conditional_writes without optional collections must validate: %v", err)
	}
}

func TestCityStatusSchemaAcceptsFallbackWithoutConditionalWrites(t *testing.T) {
	payload := statusSchemaInstance(t, cityStatusSnapshot{})
	if _, present := payload["conditional_writes"]; present {
		t.Fatal("fallback status must omit conditional_writes")
	}
	if err := validateStatusSchemaInstance(t, payload); err != nil {
		t.Fatalf("fallback without conditional_writes must validate: %v", err)
	}
}

func TestCityStatusSchemaRejectsInvalidConditionalWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "mode", mutate: func(b map[string]any) { b["mode"] = "invalid" }},
		{name: "origin", mutate: func(b map[string]any) { b["origin"] = "invalid" }},
		{name: "effective", mutate: func(b map[string]any) { b["effective"] = "invalid" }},
		{name: "probe", mutate: func(b map[string]any) { conditionalSchemaStore(b)["probe"] = "invalid" }},
		{name: "latch", mutate: func(b map[string]any) { conditionalSchemaStore(b)["latch"] = "invalid" }},
		{name: "missing capable", mutate: func(b map[string]any) { delete(conditionalSchemaStore(b), "capable") }},
		{name: "unknown verdict field", mutate: func(b map[string]any) { conditionalSchemaStore(b)["unexpected"] = true }},
		{name: "unknown notice field", mutate: func(b map[string]any) {
			b["notices"].([]any)[0].(map[string]any)["unexpected"] = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := populatedConditionalWritesSchemaInstance(t)
			tc.mutate(payload["conditional_writes"].(map[string]any))
			if err := validateStatusSchemaInstance(t, payload); err == nil {
				t.Fatal("invalid conditional_writes accepted by published schema")
			}
		})
	}
}

func populatedConditionalWritesSchemaInstance(t *testing.T) map[string]any {
	t.Helper()
	return statusSchemaInstance(t, cityStatusSnapshot{
		ConditionalWrites: &api.StatusConditionalWrites{
			Mode: "require", Origin: "env", Effective: "fail_closed",
			Stores: []api.StatusConditionalWriteStoreVerdict{
				{StoreID: "city", Kind: "bd", Probe: "capable", Latch: "unlatched", Capable: true},
				{
					StoreID: "rig/project", Kind: "bd", Probe: "capable", Latch: "incapable", Capable: false,
					Reason: "conditional writes unsupported at runtime",
				},
			},
			Notices: []api.StatusRolloutNotice{{
				Kind: "env_overrides_config", FlagKey: "beads.conditional_writes",
				EnvVar: "GC_BEADS_CONDITIONAL_WRITES", ConfigValue: "auto", EnvValue: "require",
				Message: "environment overrides configured conditional writes",
			}},
		},
	})
}

func conditionalSchemaStore(block map[string]any) map[string]any {
	return block["stores"].([]any)[0].(map[string]any)
}
