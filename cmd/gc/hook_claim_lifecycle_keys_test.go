package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// A directly routed bead (gc sling, no formula) has neither a workflow root nor
// a continuation group. The worker role contract still records both from the
// claim line — an empty continuation group is the "hard session boundary"
// signal — so the line must carry the keys explicitly. With omitempty the keys
// vanished and every worker was left to infer "absent means empty" (citadel
// papercuts pc_b7a878218aac, pc_3be95454d9fe, pc_573e5bf01dfe, pc_33b927b6c054).
func TestHookClaimWorkResultCarriesLifecycleKeysExplicitly(t *testing.T) {
	result := hookClaimJSONResult{
		SchemaVersion: "1",
		OK:            true,
		Command:       hookClaimCommandName,
		Action:        "work",
		Reason:        "claimed",
		BeadID:        "gp-1",
		Assignee:      "ci-abc123",
		Route:         "rig/worker",
	}
	var out bytes.Buffer
	if err := writeHookClaimResultLine(result, true, &out); err != nil {
		t.Fatalf("writeHookClaimResultLine: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out.String())
	}
	for _, key := range []string{"root_bead_id", "continuation_group"} {
		value, ok := decoded[key]
		if !ok {
			t.Fatalf("work result omits %q; want an explicit empty string\n%s", key, out.String())
		}
		if value != "" {
			t.Fatalf("%s = %#v, want \"\"", key, value)
		}
	}

	raw, err := os.ReadFile(hookClaimResultSchemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", hookClaimResultSchemaPath, err)
	}
	schema := compileJSONSchema(t, "gc://schemas/hook/result.schema.json", raw)
	if err := schema.Validate(decoded); err != nil {
		t.Fatalf("work result does not match the published schema: %v\n%s", err, out.String())
	}

	// Removing either key from a work result must now be a schema violation, so
	// a consumer validating the line can rely on both being present.
	for _, key := range []string{"root_bead_id", "continuation_group"} {
		stripped := make(map[string]any, len(decoded))
		for k, v := range decoded {
			if k != key {
				stripped[k] = v
			}
		}
		if err := schema.Validate(stripped); err == nil {
			t.Fatalf("schema accepted a work result without %q", key)
		}
	}
}

// The drain line keeps validating with the keys present-but-empty: the schema
// types them as plain strings and never requires them for action=drain.
func TestHookClaimDrainResultStillMatchesSchemaWithLifecycleKeys(t *testing.T) {
	var out bytes.Buffer
	code := writeHookClaimDrain(hookClaimLabel, hookClaimReasonNoWork, true, false, nil, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("drain without drain-ack exit = %d, want 1", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("drain result is not JSON: %v\n%s", err, out.String())
	}
	raw, err := os.ReadFile(hookClaimResultSchemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", hookClaimResultSchemaPath, err)
	}
	if err := compileJSONSchema(t, "gc://schemas/hook/result.schema.json", raw).Validate(decoded); err != nil {
		t.Fatalf("drain result does not match the published schema: %v\n%s", err, out.String())
	}
}
