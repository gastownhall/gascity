package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestBeadsState_JSONThroughRunMatchesSchema is the regression guard for the
// defect that "--json" shipped broken: every JSON test in the original change
// called cmdBeadsState directly, which skips run()'s JSON contract gate. That
// gate looks the command's result schema up in the embedded schema FS before
// the command ever executes, so with no schemas/beads/state/result.schema.json
// the real CLI answered {"ok":false,"error":{"code":"json_unsupported"}} and
// exit 1 while the unit tests stayed green. This test drives the same entry
// point the CLI does and validates the payload against the committed schema.
func TestBeadsState_JSONThroughRunMatchesSchema(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	t.Setenv("GC_CITY", dir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"beads", "state", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(beads state --json) = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}

	payload := stdout.Bytes()
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("payload is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if !envelope.OK {
		t.Fatalf("ok = false, want true: %s", stdout.String())
	}
	if envelope.SchemaVersion != "1" {
		t.Errorf("schema_version = %q, want \"1\"", envelope.SchemaVersion)
	}

	rawSchema, err := readBuiltinSchema([]string{"beads", "state"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read beads state result schema: %v", err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("beads-state.schema.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile("beads-state.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("payload does not match the committed beads state schema: %v\n%s", err, string(payload))
	}
}

// TestBeadsState_JSONSchemaManifestIsServable proves the schema is reachable
// through the same --json-schema surface every other JSON command exposes.
func TestBeadsState_JSONSchemaManifestIsServable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"beads", "state", "--json-schema"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(beads state --json-schema) = %d, want 0\nstderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "schema_version") {
		t.Errorf("manifest does not mention schema_version:\n%s", stdout.String())
	}
}

// beadsStateSessionBead builds a session bead carrying a lifecycle state.
func beadsStateSessionBead(lifecycleState string) beads.Bead {
	meta := map[string]string{"session_name": "sess", "template": "rig/agent"}
	if lifecycleState != "" {
		meta["state"] = lifecycleState
	}
	return beads.Bead{ID: "s-1", Status: "open", Metadata: meta}
}

// TestBeadsStateSessionIsLiveUsesCapacityProjection pins the liveness predicate
// to the canonical lifecycle projection. The original implementation used a
// four-value denylist (suspended/archived/quarantined/drained) that counted
// failed-create, orphaned, closing and stopped sessions as LIVE, so a bead held
// by a failed-create session reported in-progress instead of orphaned — the
// anomaly this command exists to surface.
func TestBeadsStateSessionIsLiveUsesCapacityProjection(t *testing.T) {
	live := []string{"active", "creating", "start-pending", "draining", "quarantined"}
	dead := []string{"failed-create", "orphaned", "closing", "stopped", "archived", "asleep", "drained", "suspended"}

	for _, st := range live {
		if !beadsStateSessionIsLive(beadsStateSessionBead(st)) {
			t.Errorf("state %q: got not-live, want live (it still occupies a slot)", st)
		}
	}
	for _, st := range dead {
		if beadsStateSessionIsLive(beadsStateSessionBead(st)) {
			t.Errorf("state %q: got live, want not-live", st)
		}
	}
	closed := beadsStateSessionBead("active")
	closed.Status = "closed"
	if beadsStateSessionIsLive(closed) {
		t.Error("closed session bead reported live")
	}
	// An unstamped bead is treated as live on purpose: a fabricated orphan is
	// worse than a missed one.
	if !beadsStateSessionIsLive(beadsStateSessionBead("")) {
		t.Error("session bead with no lifecycle state reported not-live; a missing stamp must not fabricate an orphan")
	}
}

// beadsStateDarkStore is a store whose session-bead listing has gone dark.
type beadsStateDarkStore struct{ beads.Store }

func (beadsStateDarkStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errBeadsStateDark{}
}

type errBeadsStateDark struct{}

func (errBeadsStateDark) Error() string { return "session leg is dark" }

// TestBeadsStateLiveSetsPropagateStoreErrors is the regression guard for the
// silent-degradation defect: buildBeadsStateLiveSets used to answer (nil, nil)
// on any error, and Classify reads nil live sets as "detection disabled". Every
// session-bound bead then read in-progress and every routed bead
// routed-waiting, with exit 0 and an empty stderr — an anomaly detector
// reporting calm precisely when it had gone blind.
func TestBeadsStateLiveSetsPropagateStoreErrors(t *testing.T) {
	live, liveRigs, err := buildBeadsStateLiveSets(beadsStateDarkStore{})
	if err == nil {
		t.Fatal("buildBeadsStateLiveSets returned nil error for a dark store; the failure must not be swallowed")
	}
	if live != nil || liveRigs != nil {
		t.Errorf("live=%v liveRigs=%v, want both nil alongside the error", live, liveRigs)
	}
}

// TestBeadsStateRejectsUnknownStateFilter proves a misspelled --state is
// reported rather than silently matching nothing: for a triage tool, "no
// anomalies" and "you typed the filter wrong" must not look identical.
func TestBeadsStateRejectsUnknownStateFilter(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	code := cmdBeadsState("", "routed_waiting", false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdBeadsState --state routed_waiting = 0, want non-zero\nstdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown --state") {
		t.Errorf("stderr does not name the bad flag: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "routed-waiting") {
		t.Errorf("stderr does not list the valid states: %s", stderr.String())
	}
}

// TestBeadsStateRejectsUnknownRigFilter is the same guarantee for --rig, but
// keyed on the city's configured rigs rather than on "did any bead match": a
// rig with genuinely no routed beads is a real answer and must still exit 0,
// so only a name the city does not know is an error.
func TestBeadsStateRejectsUnknownRigFilter(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	code := cmdBeadsState("no-such-rig", "", false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdBeadsState --rig no-such-rig = 0, want non-zero\nstdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown --rig") || !strings.Contains(stderr.String(), "no-such-rig") {
		t.Errorf("stderr does not name the bad rig: %s", stderr.String())
	}
}

// TestBeadsStateAcceptsConfiguredButIdleRig proves the flip side: a rig the
// city knows but which currently holds no routed beads reports an empty result
// with exit 0, rather than being mistaken for a typo.
func TestBeadsStateAcceptsConfiguredButIdleRig(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"worker\"\n\n[[rigs]]\nname = \"idle-rig\"\npath = \"rigs/idle-rig\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	if code := cmdBeadsState("idle-rig", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdBeadsState --rig idle-rig = %d, want 0 (a configured rig with no routed beads is a real answer)\nstderr=%s", code, stderr.String())
	}
}
