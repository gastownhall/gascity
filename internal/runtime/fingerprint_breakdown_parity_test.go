package runtime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// breakdownKeyForCoreField maps EVERY Config field hashed by hashCoreFields to
// the CoreFingerprintBreakdown.Fields key that carries its per-field hash. The
// domain must equal coreFieldHalf's key set (fingerprint_partition_test.go), so
// a new core field cannot be classified into a partition half without also
// naming the breakdown key that lets the config-drift diagnostic attribute it.
//
// Several Config fields fold into one breakdown key: ProviderName,
// ProviderOverlayName and InstallAgentHooks all flow through
// OverlayProviderNames into "OverlayProviders".
var breakdownKeyForCoreField = map[string]string{
	"Command":              "Command",
	"Lifecycle":            "Lifecycle",
	"Upstream":             "Upstream",
	"OperatorEnv":          "OperatorEnv",
	"MCPServers":           "MCPServers",
	"AcceptStartupDialogs": "AcceptStartupDialogs",
	"MouseOn":              "MouseOn",
	"SessionSetup":         "SessionSetup",
	"SessionSetupScript":   "SessionSetupScript",
	"Env":                  "Env",
	"FingerprintExtra":     "FPExtra",
	"PreStart":             "PreStart",
	"OverlayDir":           "OverlayDir",
	"CopyFiles":            "CopyFiles",
	"ProviderName":         "OverlayProviders",
	"ProviderOverlayName":  "OverlayProviders",
	"InstallAgentHooks":    "OverlayProviders",
}

// TestCoreFingerprintBreakdownCoversEveryCoreField is the structural parity
// guard between hashCoreFields and CoreFingerprintBreakdown. The reconciler
// diffs the stored breakdown against the current one to tell the operator
// WHICH field drained a session; an input that hashCoreFields consumes but the
// breakdown omits drains with "no per-field diff (possible sentinel/ordering
// issue)" — a message that blames the hasher for a legitimate restart.
//
// Three checks close the gap:
//
//  1. breakdownKeyForCoreField and coreFieldHalf have the same domain, so the
//     partition completeness guard (which already forces every Config field to
//     be classified) transitively forces a breakdown key.
//  2. Every breakdown key produced by CoreFingerprintBreakdown is named by the
//     map and vice versa — no orphan keys on either side.
//  3. Behaviorally, mutating ONLY a core field (via partitionHalfCases) moves
//     exactly its mapped breakdown key and nothing else.
func TestCoreFingerprintBreakdownCoversEveryCoreField(t *testing.T) {
	for field := range coreFieldHalf {
		if _, ok := breakdownKeyForCoreField[field]; !ok {
			t.Errorf("Config.%s feeds CoreFingerprint (coreFieldHalf) but breakdownKeyForCoreField does not name its breakdown key. Add it to CoreFingerprintBreakdown.Fields so config-drift diagnostics can attribute a change to it.", field)
		}
	}
	for field := range breakdownKeyForCoreField {
		if _, ok := coreFieldHalf[field]; !ok {
			t.Errorf("breakdownKeyForCoreField names Config.%s, which coreFieldHalf does not classify as a core field", field)
		}
	}

	base := goldenFixtures()["comprehensive"]
	baseBd := CoreFingerprintBreakdown(base)

	wantKeys := map[string]bool{}
	for _, key := range breakdownKeyForCoreField {
		wantKeys[key] = true
	}
	for key := range wantKeys {
		if _, ok := baseBd.Fields[key]; !ok {
			t.Errorf("CoreFingerprintBreakdown.Fields lacks %q, which breakdownKeyForCoreField expects", key)
		}
	}
	for key := range baseBd.Fields {
		if !wantKeys[key] {
			t.Errorf("CoreFingerprintBreakdown.Fields has orphan key %q that no core field maps to", key)
		}
	}

	witnessed := map[string]bool{}
	for _, tc := range partitionHalfCases {
		witnessed[tc.field] = true
		t.Run(tc.field, func(t *testing.T) {
			wantKey, ok := breakdownKeyForCoreField[tc.field]
			if !ok {
				t.Fatalf("partitionHalfCases mutates %q but breakdownKeyForCoreField omits it", tc.field)
			}
			mutated := base
			tc.mutate(&mutated)
			if CoreFingerprint(base) == CoreFingerprint(mutated) {
				t.Fatalf("%s: mutation did not change CoreFingerprint", tc.field)
			}
			diffs := diffBreakdownFields(baseBd.Fields, CoreFingerprintBreakdown(mutated).Fields)
			if len(diffs) != 1 || diffs[0] != wantKey {
				t.Fatalf("%s: mutation changed CoreFingerprint but breakdown diff = %v, want exactly [%s]", tc.field, diffs, wantKey)
			}
		})
	}

	// Every core field is either behaviorally witnessed or folds into a key
	// that a witnessed field also drives.
	witnessedKeys := map[string]bool{}
	for field := range witnessed {
		witnessedKeys[breakdownKeyForCoreField[field]] = true
	}
	for field, key := range breakdownKeyForCoreField {
		if !witnessedKeys[key] {
			t.Errorf("breakdown key %q (from Config.%s) has no behavioral witness in partitionHalfCases", key, field)
		}
	}
}

// TestCoreFingerprintBreakdownOptionalFieldsFollowCoreFraming pins that the
// new optional fields hash exactly like hashCoreFields: nil and empty
// OperatorEnv / unset Upstream contribute nothing, so their breakdown hashes
// are identical for every config that does not set them.
func TestCoreFingerprintBreakdownOptionalFieldsFollowCoreFraming(t *testing.T) {
	nilCfg := Config{Command: "claude"}
	emptyCfg := Config{Command: "claude", OperatorEnv: map[string]string{}}
	setCfg := Config{Command: "claude", OperatorEnv: map[string]string{"K": "v"}}

	nilBd := CoreFingerprintBreakdown(nilCfg)
	emptyBd := CoreFingerprintBreakdown(emptyCfg)
	setBd := CoreFingerprintBreakdown(setCfg)

	if nilBd.Fields["OperatorEnv"] != emptyBd.Fields["OperatorEnv"] {
		t.Errorf("nil vs empty OperatorEnv breakdown hashes differ: %q vs %q", nilBd.Fields["OperatorEnv"], emptyBd.Fields["OperatorEnv"])
	}
	if nilBd.Fields["OperatorEnv"] == setBd.Fields["OperatorEnv"] {
		t.Error("set OperatorEnv breakdown hash equals the empty one")
	}
	if diffs := diffBreakdownFields(nilBd.Fields, emptyBd.Fields); len(diffs) != 0 {
		t.Errorf("nil vs empty OperatorEnv produced breakdown diffs %v, want none", diffs)
	}

	// Order independence: same pairs inserted differently hash identically.
	a := Config{Command: "claude", OperatorEnv: map[string]string{"A": "1", "B": "2"}}
	b := Config{Command: "claude", OperatorEnv: map[string]string{"B": "2", "A": "1"}}
	if CoreFingerprintBreakdown(a).Fields["OperatorEnv"] != CoreFingerprintBreakdown(b).Fields["OperatorEnv"] {
		t.Error("OperatorEnv breakdown hash depends on map insertion order")
	}

	unset := Config{Command: "claude"}
	upstream := Config{Command: "claude", Upstream: "bedrock"}
	if CoreFingerprintBreakdown(unset).Fields["Upstream"] == CoreFingerprintBreakdown(upstream).Fields["Upstream"] {
		t.Error("set Upstream breakdown hash equals the unset one")
	}
}

// TestLogDriftAttributesOperatorEnvOnlyChange reproduces the operator-facing
// symptom: the only difference between the stored and current config is a
// config-authored env value. hashCoreFields moves (the session legitimately
// drains), so the diagnostic must name OperatorEnv instead of reporting
// "no per-field diff".
func TestLogDriftAttributesOperatorEnvOnlyChange(t *testing.T) {
	stored := goldenFixtures()["comprehensive"]
	current := stored
	current.OperatorEnv = envWith(stored.OperatorEnv, "OPERATOR_AUTHORED", "v2")
	if CoreFingerprint(stored) == CoreFingerprint(current) {
		t.Fatal("precondition: OperatorEnv change must move CoreFingerprint")
	}

	storedJSON, err := json.Marshal(CoreFingerprintBreakdown(stored))
	if err != nil {
		t.Fatal(err)
	}

	if got := CoreFingerprintDriftFieldsFromJSON(string(storedJSON), current); !reflect.DeepEqual(got, []string{"OperatorEnv"}) {
		t.Fatalf("CoreFingerprintDriftFieldsFromJSON = %v, want [OperatorEnv]", got)
	}

	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "agent-a", string(storedJSON), current)
	out := buf.String()
	if strings.Contains(out, "no per-field diff") {
		t.Fatalf("diagnostic still reports no per-field diff:\n%s", out)
	}
	if !strings.Contains(out, "config-drift-diag agent-a: drifted fields: OperatorEnv\n") {
		t.Errorf("missing drifted-fields line naming OperatorEnv:\n%s", out)
	}
	if !strings.Contains(out, "    OperatorEnv: stored-hash=") {
		t.Errorf("missing OperatorEnv stored-hash/current-hash line:\n%s", out)
	}
	// The detail line names the keys so the operator knows where to look,
	// but never echoes values: config-authored env can carry credentials.
	if !strings.Contains(out, "    OperatorEnv keys: [ANOTHER_KEY OPERATOR_AUTHORED]\n") {
		t.Errorf("missing OperatorEnv keys detail line:\n%s", out)
	}
	if strings.Contains(out, "v2") {
		t.Errorf("diagnostic leaked an OperatorEnv value:\n%s", out)
	}
}

// TestLogDriftAttributesUpstreamOnlyChange covers the other hashCoreFields
// input the breakdown omitted: switching the model-serving upstream.
func TestLogDriftAttributesUpstreamOnlyChange(t *testing.T) {
	stored := goldenFixtures()["comprehensive"]
	current := stored
	current.Upstream = "bedrock"

	storedJSON, err := json.Marshal(CoreFingerprintBreakdown(stored))
	if err != nil {
		t.Fatal(err)
	}
	if got := CoreFingerprintDriftFieldsFromJSON(string(storedJSON), current); !reflect.DeepEqual(got, []string{"Upstream"}) {
		t.Fatalf("CoreFingerprintDriftFieldsFromJSON = %v, want [Upstream]", got)
	}

	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "agent-a", string(storedJSON), current)
	out := buf.String()
	if !strings.Contains(out, "drifted fields: Upstream\n") {
		t.Errorf("missing drifted-fields line naming Upstream:\n%s", out)
	}
	if !strings.Contains(out, "    Upstream: \"bedrock\"\n") {
		t.Errorf("missing Upstream detail line:\n%s", out)
	}
}

// TestLogDriftReportsFieldsAbsentFromStoredBreakdown pins the upgrade-window
// behavior: a session started by a binary whose BreakdownV1 predates the
// OperatorEnv/Upstream keys carries the current FingerprintVersion but no
// entry for either. On that session's first real drift (here: Command),
// diffBreakdownFields sees the current side's empty-input digest for both new
// keys against nothing stored, so BOTH are listed with stored-hash=(absent)
// alongside the field that actually changed — never "no per-field diff", and
// never an empty stored-hash. The restart re-stamps the breakdown and the
// absent entries disappear.
func TestLogDriftReportsFieldsAbsentFromStoredBreakdown(t *testing.T) {
	stored := goldenFixtures()["comprehensive"]
	legacy := CoreFingerprintBreakdown(stored)
	delete(legacy.Fields, "OperatorEnv")
	delete(legacy.Fields, "Upstream")
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	// The only genuine change is Command; OperatorEnv and Upstream are
	// unchanged, and Upstream is unset on both sides.
	current := stored
	current.Command += " --changed"

	got := CoreFingerprintDriftFieldsFromJSON(string(legacyJSON), current)
	if !reflect.DeepEqual(got, []string{"Command", "OperatorEnv", "Upstream"}) {
		t.Fatalf("CoreFingerprintDriftFieldsFromJSON = %v, want [Command OperatorEnv Upstream] (keys absent from the stored breakdown are reported alongside the real change, not skipped)", got)
	}

	var buf bytes.Buffer
	LogCoreFingerprintDrift(&buf, "agent-a", string(legacyJSON), current)
	out := buf.String()
	if strings.Contains(out, "no per-field diff") {
		t.Fatalf("legacy stored breakdown produced no per-field diff:\n%s", out)
	}
	if !strings.Contains(out, "drifted fields: Command, OperatorEnv, Upstream\n") {
		t.Errorf("missing drifted-fields line:\n%s", out)
	}
	// Both never-stored keys render as absent on the stored side with a
	// real digest on the current side.
	for _, key := range []string{"OperatorEnv", "Upstream"} {
		if !strings.Contains(out, "    "+key+": stored-hash=(absent) current-hash="+legacyCurrentHash(t, current, key)+"\n") {
			t.Errorf("%s must render stored-hash=(absent) with its current digest:\n%s", key, out)
		}
	}
	// The genuinely changed key keeps the plain rendering with a real
	// stored hash.
	wantCommand := "    Command: stored-hash=" + legacy.Fields["Command"] + " current-hash=" + legacyCurrentHash(t, current, "Command") + "\n"
	if !strings.Contains(out, wantCommand) {
		t.Errorf("changed, present key must render both real hashes:\n%s", out)
	}
	if strings.Contains(out, "Command: stored-hash=(absent)") {
		t.Errorf("present key rendered as absent:\n%s", out)
	}
	if strings.Contains(out, "stored-hash= ") {
		t.Errorf("an absent key rendered as an empty stored-hash:\n%s", out)
	}
}

// legacyCurrentHash returns the current-side breakdown digest for key,
// failing the test if the key is missing from the breakdown.
func legacyCurrentHash(t *testing.T, current Config, key string) string {
	t.Helper()
	h, ok := CoreFingerprintBreakdown(current).Fields[key]
	if !ok {
		t.Fatalf("CoreFingerprintBreakdown lacks %q", key)
	}
	return h
}
