package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

const testCustomEventType = "custom.test"

func TestDoEventEmitSuccess(t *testing.T) {
	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-1", "Build Tower of Hanoi", "mayor", "", &stderr)
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	// Verify the event was written.
	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(evts))
	}
	e := evts[0]
	if e.Type != testCustomEventType {
		t.Errorf("Type = %q, want %q", e.Type, testCustomEventType)
	}
	if e.Subject != "gc-1" {
		t.Errorf("Subject = %q, want %q", e.Subject, "gc-1")
	}
	if e.Message != "Build Tower of Hanoi" {
		t.Errorf("Message = %q, want %q", e.Message, "Build Tower of Hanoi")
	}
	if e.Actor != "mayor" {
		t.Errorf("Actor = %q, want %q", e.Actor, "mayor")
	}
	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
}

func TestDoEventEmitDefaultActor(t *testing.T) {
	clearGCEnv(t)
	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-1", "", "", "", &stderr)

	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(evts))
	}
	// Default actor when GC_AGENT is not set.
	if evts[0].Actor != "human" {
		t.Errorf("Actor = %q, want %q", evts[0].Actor, "human")
	}
}

func TestDoEventEmitGCAgentEnv(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_AGENT", "worker")

	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-1", "task", "", "", &stderr)

	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if evts[0].Actor != "worker" {
		t.Errorf("Actor = %q, want %q (from GC_AGENT)", evts[0].Actor, "worker")
	}
}

func TestDoEventEmitPrefersAlias(t *testing.T) {
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_AGENT", "worker")

	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-1", "task", "", "", &stderr)

	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if evts[0].Actor != "mayor" {
		t.Errorf("Actor = %q, want %q (from GC_ALIAS)", evts[0].Actor, "mayor")
	}
}

func TestDoEventEmitPayload(t *testing.T) {
	ep := events.NewFake()

	payload := `{"type":"merge-request","title":"Fix login bug","assignee":"refinery"}`
	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-42", "Fix login bug", "polecat", payload, &stderr)

	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(evts))
	}
	if evts[0].Payload == nil {
		t.Fatal("Payload is nil, want JSON")
	}
	if string(evts[0].Payload) != payload {
		t.Errorf("Payload = %s, want %s", evts[0].Payload, payload)
	}
}

func TestDoEventEmitPayloadEmpty(t *testing.T) {
	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-1", "task", "", "", &stderr)

	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if evts[0].Payload != nil {
		t.Errorf("Payload = %s, want nil (omitted)", evts[0].Payload)
	}
}

func TestDoEventEmitPayloadInvalidJSON(t *testing.T) {
	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, testCustomEventType, "gc-1", "task", "", "not-json{", &stderr)
	if !strings.Contains(stderr.String(), "not valid JSON") {
		t.Errorf("stderr = %q, want 'not valid JSON' warning", stderr.String())
	}

	// No event should be written.
	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evts) != 0 {
		t.Errorf("len(events) = %d, want 0 (invalid payload skipped)", len(evts))
	}
}

func TestDoEventEmitRejectsBeadLifecycleEventWithoutSnapshot(t *testing.T) {
	lifecycleTypes := []string{
		events.BeadCreated,
		events.BeadUpdated,
		events.BeadClosed,
		events.BeadDeleted,
	}
	invalidPayloads := []struct {
		name        string
		payload     string
		diagnostics []string
	}{
		{
			name:        "missing payload",
			diagnostics: []string{"payload"},
		},
		{
			name:        "valid JSON without bead ID",
			payload:     `{"title":"snapshot without an id","status":"open"}`,
			diagnostics: []string{"payload", "id"},
		},
		{
			name:        "whitespace bead ID",
			payload:     `{"id":" "}`,
			diagnostics: []string{"payload", "id"},
		},
	}

	for _, eventType := range lifecycleTypes {
		for _, tc := range invalidPayloads {
			t.Run(eventType+"/"+tc.name, func(t *testing.T) {
				ep := events.NewFake()
				var stderr bytes.Buffer

				if submitted := doEventEmit(ep, eventType, "gc-1", "task", "mayor", tc.payload, &stderr); submitted {
					t.Fatal("doEventEmit returned true, want false")
				}

				evts, err := ep.List(events.Filter{})
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if len(evts) != 0 {
					t.Fatalf("len(events) = %d, want 0", len(evts))
				}

				diagnostic := strings.ToLower(stderr.String())
				for _, want := range append([]string{eventType}, tc.diagnostics...) {
					if !strings.Contains(diagnostic, strings.ToLower(want)) {
						t.Errorf("stderr = %q, want diagnostic containing %q", stderr.String(), want)
					}
				}
			})
		}
	}
}

func TestDoEventEmitAcceptsBeadLifecycleSnapshots(t *testing.T) {
	lifecycleTypes := []string{
		events.BeadCreated,
		events.BeadUpdated,
		events.BeadClosed,
		events.BeadDeleted,
	}
	snapshots := []struct {
		name    string
		beadID  string
		payload string
	}{
		{
			name:    "canonical raw snapshot",
			beadID:  "gc-raw",
			payload: `{"id":"gc-raw","title":"raw snapshot","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`,
		},
		{
			name:    "wrapped snapshot",
			beadID:  "gc-wrapped",
			payload: `{"bead":{"id":"gc-wrapped","title":"wrapped snapshot","status":"closed","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}}`,
		},
		{
			name:    "exec type alias",
			beadID:  "gc-compat",
			payload: `{"id":"gc-compat","title":"compat snapshot","status":"open","type":"task","created_at":"2026-07-15T00:00:00Z"}`,
		},
	}

	for _, eventType := range lifecycleTypes {
		for _, tc := range snapshots {
			t.Run(eventType+"/"+tc.name, func(t *testing.T) {
				ep := events.NewFake()
				var stderr bytes.Buffer
				payload := tc.payload
				if eventType == events.BeadClosed {
					payload = strings.Replace(payload, `"status":"open"`, `"status":"closed"`, 1)
				}

				if submitted := doEventEmit(ep, eventType, tc.beadID, "task", "mayor", payload, &stderr); !submitted {
					t.Fatalf("doEventEmit returned false; stderr = %q", stderr.String())
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q, want empty", stderr.String())
				}

				evts, err := ep.List(events.Filter{})
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if len(evts) != 1 {
					t.Fatalf("len(events) = %d, want 1", len(evts))
				}
				if evts[0].Type != eventType {
					t.Errorf("Type = %q, want %q", evts[0].Type, eventType)
				}
				if string(evts[0].Payload) != payload {
					t.Errorf("Payload = %s, want %s", evts[0].Payload, payload)
				}
			})
		}
	}
}

func TestDoEventEmitRejectsOpenBeadClosedSnapshot(t *testing.T) {
	ep := events.NewFake()
	var stderr bytes.Buffer
	payload := `{"id":"gc-1","title":"still open","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`

	if submitted := doEventEmit(ep, events.BeadClosed, "gc-1", "", "mayor", payload, &stderr); submitted {
		t.Fatal("doEventEmit returned true, want false")
	}
	if len(ep.Events) != 0 {
		t.Fatalf("len(events) = %d, want 0", len(ep.Events))
	}
	if diagnostic := strings.ToLower(stderr.String()); !strings.Contains(diagnostic, events.BeadClosed) || !strings.Contains(diagnostic, "closed") {
		t.Errorf("stderr = %q, want bead.closed status diagnostic", stderr.String())
	}
}

func TestDoEventEmitRejectsLifecycleSubjectPayloadIDMismatch(t *testing.T) {
	ep := events.NewFake()
	var stderr bytes.Buffer
	payload := `{"id":"gc-payload","title":"different bead","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`

	if submitted := doEventEmit(ep, events.BeadUpdated, "gc-subject", "", "mayor", payload, &stderr); submitted {
		t.Fatal("doEventEmit returned true, want false")
	}
	if len(ep.Events) != 0 {
		t.Fatalf("len(events) = %d, want 0", len(ep.Events))
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "gc-subject") || !strings.Contains(diagnostic, "gc-payload") {
		t.Errorf("stderr = %q, want both conflicting bead IDs", diagnostic)
	}
}

func TestDoEventEmitRejectsSparseLifecycleSnapshotBeforeProjection(t *testing.T) {
	ep := events.NewFake()
	var stderr bytes.Buffer
	createdPayload := `{"id":"gc-1","title":"Build Hanoi","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`

	if submitted := doEventEmit(ep, events.BeadCreated, "gc-1", "created", "mayor", createdPayload, &stderr); !submitted {
		t.Fatalf("created event returned false; stderr = %q", stderr.String())
	}
	invalid := []struct {
		name    string
		payload string
		fields  []string
	}{
		{
			name:    "missing fields",
			payload: `{"id":"gc-1"}`,
			fields:  []string{"title", "status", "created_at", "issue_type"},
		},
		{
			name:    "empty and null fields",
			payload: `{"id":"gc-1","title":"","status":null,"issue_type":"","created_at":null}`,
			fields:  []string{"title", "status", "created_at", "issue_type"},
		},
		{
			name:    "whitespace and zero timestamp",
			payload: `{"id":"gc-1","title":" ","status":"\t","issue_type":" ","created_at":"0001-01-01T00:00:00Z"}`,
			fields:  []string{"title", "status", "created_at", "issue_type"},
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			stderr.Reset()
			if submitted := doEventEmit(ep, events.BeadUpdated, "gc-1", "invalid update", "mayor", tc.payload, &stderr); submitted {
				t.Fatal("invalid updated event returned true, want false")
			}
			for _, field := range tc.fields {
				if !strings.Contains(stderr.String(), field) {
					t.Errorf("stderr = %q, want invalid-field diagnostic containing %q", stderr.String(), field)
				}
			}
		})
	}

	evts, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	projector := runproj.NewProjector()
	projector.Apply(evts)
	projected := projector.Beads()
	if len(projected) != 1 {
		t.Fatalf("projected beads = %d, want 1", len(projected))
	}
	if projected[0].Title != "Build Hanoi" || projected[0].Status != "open" || projected[0].Type != "task" {
		t.Fatalf("projected bead = %+v, want complete created snapshot preserved", projected[0])
	}
}

func TestLoadEventBeadPayloadFromStoreNullsNeedsAlongsideDependencies(t *testing.T) {
	const beadID = "gc-step-1"
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected command %q %v", name, args)
		}
		switch args[0] {
		case "show", "query":
			return []byte(`[{"id":"gc-step-1","title":"molecule step","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z","needs":["gc-step-0"]}]`), nil
		case "dep":
			return []byte(`[{"id":"gc-blocker-1","dependency_type":"blocks"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected bd arguments %q", args)
		}
	}
	store := wrapStoreWithBeadPolicies(beads.NewBdStore(t.TempDir(), runner), nil)

	payload, err := loadEventBeadPayloadFromStore(store, beadID, true)
	if err != nil {
		t.Fatalf("loadEventBeadPayloadFromStore: %v", err)
	}
	bead, ok := beads.DecodeBeadEventPayload(payload)
	if !ok {
		t.Fatalf("payload is not a bead snapshot: %s", payload)
	}
	// The canonical CachingStore snapshot (readBeadWithDeps) carries dependency
	// edges in Dependencies and nulls Needs; the CLI snapshot must match so both
	// producers publish structurally identical rows.
	if bead.Needs != nil {
		t.Fatalf("hydrated snapshot Needs = %#v, want nil to match the canonical snapshot", bead.Needs)
	}
	if len(bead.Dependencies) != 1 || bead.Dependencies[0].DependsOnID != "gc-blocker-1" || bead.Dependencies[0].Type != "blocks" {
		t.Fatalf("hydrated dependencies = %#v, want the authoritative blocker edge", bead.Dependencies)
	}
}

func TestGCEventEmitBeadDeletedPayloadOnlyEmitsIDOnlySnapshot(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc init = %d; stderr: %s", code, stderr.String())
	}
	store, err := openStoreAtForCity(dir, dir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "doomed task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	t.Chdir(dir)

	stdout.Reset()
	stderr.Reset()
	// The documented post-delete invocation: only --bead-payload, no --payload.
	code := run([]string{"--city", dir, "event", "emit", events.BeadDeleted, "--bead-payload", created.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit bead.deleted = %d; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty for a synthesized ID-only bead.deleted", stderr.String())
	}

	eventsPath := filepath.Join(dir, ".gc", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("ReadFile(events.jsonl): %v", err)
	}
	var deleted *events.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e events.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Type == events.BeadDeleted {
			captured := e
			deleted = &captured
		}
	}
	if deleted == nil {
		t.Fatalf("no bead.deleted event recorded in %s:\n%s", eventsPath, data)
	}
	bead, ok := beads.DecodeBeadEventPayload(deleted.Payload)
	if !ok || bead.ID != created.ID {
		t.Fatalf("bead.deleted payload = %s, want ID-only snapshot for %s", deleted.Payload, created.ID)
	}
	if deleted.Subject != created.ID {
		t.Fatalf("bead.deleted subject = %q, want derived %q", deleted.Subject, created.ID)
	}
}

func TestDoEventEmitAcceptsIDOnlyBeadDeletedPayload(t *testing.T) {
	ep := events.NewFake()
	var stderr bytes.Buffer

	if submitted := doEventEmit(ep, events.BeadDeleted, "gc-1", "deleted", "mayor", `{"id":"gc-1"}`, &stderr); !submitted {
		t.Fatalf("deleted event returned false; stderr = %q", stderr.String())
	}
	if len(ep.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(ep.Events))
	}
}

func TestDoEventEmitDerivesLifecycleSubjectFromWrappedSnapshot(t *testing.T) {
	ep := events.NewFake()
	var stderr bytes.Buffer
	payload := `{"bead":{"id":"gc-wrapped","title":"wrapped snapshot","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}}`

	if submitted := doEventEmit(ep, events.BeadUpdated, "", "updated", "mayor", payload, &stderr); !submitted {
		t.Fatalf("doEventEmit returned false; stderr = %q", stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	routed, err := ep.List(events.Filter{Subject: "gc-wrapped"})
	if err != nil {
		t.Fatalf("List by derived subject: %v", err)
	}
	if len(routed) != 1 {
		t.Fatalf("len(events routed to gc-wrapped) = %d, want 1", len(routed))
	}
	if routed[0].Subject != "gc-wrapped" {
		t.Errorf("Subject = %q, want gc-wrapped", routed[0].Subject)
	}
}

func TestEventEmitJSONReportsDerivedLifecycleSubject(t *testing.T) {
	cityDir := fakeEventEmitCity(t)
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "authoritative snapshot", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Chdir(cityDir)
	payload := fmt.Sprintf(`{"bead":{"id":%q,"title":"caller snapshot","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}}`, created.ID)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "event", "emit", events.BeadUpdated, "--payload", payload, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit = %d, want 0; stderr = %q", code, stderr.String())
	}

	var result eventEmitJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(stdout): %v; stdout = %q", err, stdout.String())
	}
	if !result.Submitted {
		t.Errorf("submitted = false, want true; result = %+v; stderr = %q", result, stderr.String())
	}
	if result.Subject != created.ID {
		t.Errorf("subject = %q, want %q; result = %+v", result.Subject, created.ID, result)
	}
}

func TestEventEmitRejectsLifecycleWhenAuthoritativeSnapshotCannotBeLoaded(t *testing.T) {
	cityDir := fakeEventEmitCity(t)
	t.Chdir(cityDir)
	payload := `{"id":"gc-missing","title":"caller snapshot","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "event", "emit", events.BeadUpdated, "--payload", payload, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit = %d, want 0 for best-effort rejection; stderr = %q", code, stderr.String())
	}

	var result eventEmitJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(stdout): %v; stdout = %q", err, stdout.String())
	}
	if result.Submitted {
		t.Errorf("submitted = true, want false; result = %+v", result)
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "authoritative") || !strings.Contains(diagnostic, "gc-missing") {
		t.Errorf("stderr = %q, want authoritative snapshot load diagnostic", diagnostic)
	}
}

func TestEventEmitRejectsExplicitEmptyLifecycleSubject(t *testing.T) {
	cityDir := fakeEventEmitCity(t)
	payload := `{"bead":{"id":"gc-wrapped","title":"wrapped snapshot","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}}`

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "event", "emit", events.BeadUpdated, "--payload", payload, "--subject=", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit = %d, want 0 for best-effort rejection; stderr = %q", code, stderr.String())
	}

	var result eventEmitJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(stdout): %v; stdout = %q", err, stdout.String())
	}
	if result.Submitted {
		t.Errorf("submitted = true, want false; result = %+v", result)
	}
	if result.Subject != "" {
		t.Errorf("subject = %q, want the explicitly supplied empty value", result.Subject)
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, `subject ""`) || !strings.Contains(diagnostic, "gc-wrapped") {
		t.Errorf("stderr = %q, want explicit-empty/payload-ID mismatch diagnostic", diagnostic)
	}
}

func TestDoEventEmitCustomEventPayloadsRemainUnrestricted(t *testing.T) {
	tests := []struct {
		name        string
		subject     string
		payload     string
		wantSubject string
	}{
		{name: "absent payload", subject: "build-1", wantSubject: "build-1"},
		{name: "arbitrary valid JSON", subject: "build-1", payload: `[{"anything":true},42,"custom"]`, wantSubject: "build-1"},
		{
			name:    "bead-shaped payload does not imply a subject",
			payload: `{"bead":{"id":"gc-custom","status":"open"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := events.NewFake()
			var stderr bytes.Buffer

			if submitted := doEventEmit(ep, "acme.build.completed", tc.subject, "done", "builder", tc.payload, &stderr); !submitted {
				t.Fatalf("doEventEmit returned false; stderr = %q", stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}

			evts, err := ep.List(events.Filter{})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(evts) != 1 {
				t.Fatalf("len(events) = %d, want 1", len(evts))
			}
			if string(evts[0].Payload) != tc.payload {
				t.Errorf("Payload = %s, want %s", evts[0].Payload, tc.payload)
			}
			if evts[0].Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", evts[0].Subject, tc.wantSubject)
			}
		})
	}
}

func TestEventEmitLifecycleRejectionRemainsBestEffort(t *testing.T) {
	cityDir := fakeEventEmitCity(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "event", "emit", events.BeadCreated, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit = %d, want 0; stderr = %q", code, stderr.String())
	}

	var result eventEmitJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(stdout): %v; stdout = %q", err, stdout.String())
	}
	if result.Submitted {
		t.Errorf("submitted = true, want false; result = %+v", result)
	}
	if !result.OK {
		t.Errorf("ok = false, want true for best-effort command; result = %+v", result)
	}
	if !strings.Contains(stderr.String(), events.BeadCreated) || !strings.Contains(strings.ToLower(stderr.String()), "payload") {
		t.Errorf("stderr = %q, want useful lifecycle-payload diagnostic", stderr.String())
	}
}

func fakeEventEmitCity(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	dir := writeOddballMinimalCity(t, "event-emit")
	writeFile(t, filepath.Join(dir, "city.toml"), `[workspace]
name = "event-emit"

[beads]
provider = "file"

[events]
provider = "fake"
`)
	if err := ensureScopedFileStoreLayout(dir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	return dir
}

func TestEventPayloadForEmitFallsBackToStoreBead(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := ensureScopedFileStoreLayout(dir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	store, err := openStoreAtForCity(dir, dir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "hook-created task", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Chdir(dir)
	var stderr bytes.Buffer
	payload := eventPayloadForEmit(`{"bead":}`, created.ID, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want none", stderr.String())
	}
	if !json.Valid([]byte(payload)) {
		t.Fatalf("payload is not valid JSON: %q", payload)
	}
	var decoded struct {
		Bead beads.Bead `json:"bead"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if decoded.Bead.ID != created.ID {
		t.Fatalf("payload bead ID = %q, want %q", decoded.Bead.ID, created.ID)
	}
	if decoded.Bead.Title != "hook-created task" {
		t.Fatalf("payload bead title = %q, want hook-created task", decoded.Bead.Title)
	}
}

func TestLoadEventBeadPayloadUsesCanonicalWispSnapshot(t *testing.T) {
	const beadID = "duplicate-id"
	var showCalls, queryCalls, depCalls int
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("bd called without arguments")
		}
		switch args[0] {
		case "show":
			showCalls++
			return []byte(`[{"id":"duplicate-id","title":"stale issue","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z","metadata":{"workflow_id":"stale-run","gc.session_id":"stale-session","gc.step_id":"stale-step"}}]`), nil
		case "query":
			queryCalls++
			return []byte(`[{"id":"duplicate-id","title":"canonical wisp","status":"closed","issue_type":"task","created_at":"2026-07-15T00:00:00Z","no_history":true,"metadata":{"workflow_id":"canonical-run","gc.session_id":"canonical-session","gc.step_id":"canonical-step"}}]`), nil
		case "dep":
			depCalls++
			return []byte(`[]`), nil
		default:
			return nil, fmt.Errorf("unexpected bd arguments %q", args)
		}
	}
	store := wrapStoreWithBeadPolicies(beads.NewBdStore(t.TempDir(), runner), nil)

	payload, err := loadEventBeadPayloadFromStore(store, beadID, true)
	if err != nil {
		t.Fatalf("loadEventBeadPayloadFromStore: %v", err)
	}
	bead, ok := beads.DecodeBeadEventPayload(payload)
	if !ok {
		t.Fatalf("payload is not a bead snapshot: %s", payload)
	}
	if bead.Title != "canonical wisp" || bead.Status != "closed" || !bead.NoHistory {
		t.Fatalf("payload bead = %+v, want canonical closed wisp snapshot", bead)
	}
	if showCalls != 0 || queryCalls != 1 || depCalls != 1 {
		t.Fatalf("bd calls: show=%d query=%d dep=%d, want 0/1/1", showCalls, queryCalls, depCalls)
	}

	ep := events.NewFake()
	var stderr bytes.Buffer
	if submitted := doEventEmit(ep, events.BeadClosed, beadID, "closed", "mayor", string(payload), &stderr); !submitted {
		t.Fatalf("doEventEmit returned false; stderr = %q", stderr.String())
	}
	if len(ep.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(ep.Events))
	}
	got := ep.Events[0]
	if got.RunID != "canonical-run" || got.SessionID != "canonical-session" || got.StepID != "canonical-step" {
		t.Fatalf("correlation = run:%q session:%q step:%q, want canonical wisp metadata", got.RunID, got.SessionID, got.StepID)
	}
}

func TestEventPayloadForEmitUsesInheritedBeadsDirOutsideRig(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout(city): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(rig): %v", err)
	}
	created, err := rigStore.Create(beads.Bead{Title: "rig hook-created task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(rig): %v", err)
	}

	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("BEADS_DIR", filepath.Join(rigDir, ".beads"))
	t.Chdir(t.TempDir())

	var stderr bytes.Buffer
	payload := eventPayloadForEmit(`{"bead":}`, created.ID, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want none", stderr.String())
	}
	var decoded struct {
		Bead beads.Bead `json:"bead"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if decoded.Bead.ID != created.ID {
		t.Fatalf("payload bead ID = %q, want %q", decoded.Bead.ID, created.ID)
	}
	if decoded.Bead.Title != "rig hook-created task" {
		t.Fatalf("payload bead title = %q, want rig hook-created task", decoded.Bead.Title)
	}
}

func TestEventEmitLifecyclePayloadUsesAuthoritativeOwningStoreSnapshot(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc init = %d; stderr: %s", code, stderr.String())
	}
	store, err := openStoreAtForCity(dir, dir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	parent, err := store.Create(beads.Bead{Title: "authoritative parent", Type: "epic"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	blocker, err := store.Create(beads.Bead{Title: "authoritative blocker", Type: "task"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:    "authoritative run step",
		Type:     "task",
		ParentID: parent.ID,
		Assignee: "polecat/alpha",
		Labels:   []string{"run-step", "accounted"},
	})
	if err != nil {
		t.Fatalf("Create(run step): %v", err)
	}
	if err := store.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	wantMetadata := map[string]string{
		"gc.run_id":     "run-authoritative",
		"workflow_id":   "run-authoritative",
		"gc.session_id": "session-authoritative",
		"gc.step_id":    "build",
	}
	if err := store.SetMetadataBatch(created.ID, wantMetadata); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	t.Chdir(dir)

	emit := func(eventType, status string) {
		t.Helper()
		// This is superficially complete enough to pass lifecycle validation,
		// but intentionally omits every relationship and accounting field.
		sparse := fmt.Sprintf(
			`{"id":%q,"title":"caller title","status":%q,"issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`,
			created.ID, status,
		)
		stdout.Reset()
		stderr.Reset()
		code := run([]string{
			"--city", dir, "event", "emit", eventType,
			"--subject", created.ID,
			"--bead-payload", created.ID,
			"--payload", sparse,
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("gc event emit %s = %d; stderr: %s", eventType, code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("gc event emit %s stderr = %q, want empty", eventType, stderr.String())
		}
	}

	emit(events.BeadCreated, "open")
	emit(events.BeadUpdated, "open")
	if err := store.Close(created.ID); err != nil {
		t.Fatalf("Close(run step): %v", err)
	}
	emit(events.BeadClosed, "closed")

	eventsPath := filepath.Join(dir, ".gc", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("ReadFile(events.jsonl): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("events.jsonl lines = %d, want 3; content:\n%s", len(lines), data)
	}
	for i, line := range lines {
		var event events.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("Unmarshal(event %d): %v", i, err)
		}
		got, ok := beads.DecodeBeadEventPayload(event.Payload)
		if !ok {
			t.Fatalf("event %d payload is not a bead snapshot: %s", i, event.Payload)
		}
		wantStatus := "open"
		if event.Type == events.BeadClosed {
			wantStatus = "closed"
		}
		if got.Title != "authoritative run step" || got.Status != wantStatus || got.ParentID != parent.ID || got.Assignee != "polecat/alpha" {
			t.Errorf("event %d bead = %+v, want authoritative scalar snapshot with status %q", i, got, wantStatus)
		}
		if !hasString(got.Labels, "run-step") || !hasString(got.Labels, "accounted") {
			t.Errorf("event %d labels = %#v, want authoritative labels", i, got.Labels)
		}
		if len(got.Dependencies) != 1 || got.Dependencies[0].IssueID != created.ID || got.Dependencies[0].DependsOnID != blocker.ID || got.Dependencies[0].Type != "blocks" {
			t.Errorf("event %d dependencies = %#v, want authoritative blocker edge", i, got.Dependencies)
		}
		for key, want := range wantMetadata {
			if value := got.Metadata[key]; value != want {
				t.Errorf("event %d metadata[%q] = %q, want %q", i, key, value, want)
			}
		}
		if event.RunID != "run-authoritative" || event.SessionID != "session-authoritative" || event.StepID != "build" {
			t.Errorf("event %d correlation = run:%q session:%q step:%q, want authoritative run/session/step", i, event.RunID, event.SessionID, event.StepID)
		}
	}
}

// TestEventEmitViaCLI exercises the full `gc event emit` CLI path: flag
// parsing, city discovery, event-provider open, and local events.jsonl
// write. The matching read path (`gc events`) now goes through the
// supervisor/controller API and is covered by TestDoEvents* against a
// mock API server, so this test focuses on the emit CLI's end-to-end
// behavior without needing a live controller.
//
// Pre-migration this test did an emit-then-read roundtrip via `gc events`,
// but that readback is incompatible with the API-first contract — `gc
// events` no longer reads local files. Splitting emit and read into
// their own tests keeps each side focused without needing a fake
// controller harness in the cmd/gc test tree.
func TestEventEmitViaCLI(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc init = %d; stderr: %s", code, stderr.String())
	}
	store, err := openStoreAtForCity(dir, dir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	bead, err := store.Create(beads.Bead{Title: "Build Hanoi", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Chdir(dir)

	// Emit two events via the CLI. `gc event emit` is best-effort and
	// always returns 0, but it should still write the events locally.
	stdout.Reset()
	stderr.Reset()
	createdPayload := fmt.Sprintf(`{"id":%q,"title":"caller title","status":"open","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}`, bead.ID)
	code = run([]string{"--city", dir, "event", "emit", "bead.created", "--subject", bead.ID, "--message", "Build Hanoi", "--payload", createdPayload}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit bead.created = %d; stderr: %s", code, stderr.String())
	}

	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closedPayload := fmt.Sprintf(`{"bead":{"id":%q,"title":"caller title","status":"closed","issue_type":"task","created_at":"2026-07-15T00:00:00Z"}}`, bead.ID)
	code = run([]string{"--city", dir, "event", "emit", "bead.closed", "--subject", bead.ID, "--message", "Done", "--payload", closedPayload}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit bead.closed = %d; stderr: %s", code, stderr.String())
	}

	// Verify events landed in the local JSONL file. Parse line-by-line
	// because the file is append-only JSONL, not a JSON array.
	eventsPath := filepath.Join(dir, ".gc", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("events.jsonl has %d lines, want 2; content:\n%s", len(lines), string(data))
	}

	var created, closed events.Event
	if err := json.Unmarshal([]byte(lines[0]), &created); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &closed); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}

	if created.Type != "bead.created" {
		t.Errorf("line 0 type = %q, want bead.created", created.Type)
	}
	if created.Subject != bead.ID {
		t.Errorf("line 0 subject = %q, want %s", created.Subject, bead.ID)
	}
	if created.Message != "Build Hanoi" {
		t.Errorf("line 0 message = %q, want Build Hanoi", created.Message)
	}
	if created.Seq != 1 {
		t.Errorf("line 0 seq = %d, want 1", created.Seq)
	}

	if closed.Type != "bead.closed" {
		t.Errorf("line 1 type = %q, want bead.closed", closed.Type)
	}
	if closed.Seq != 2 {
		t.Errorf("line 1 seq = %d, want 2", closed.Seq)
	}
}

func TestEventMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"event"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("gc event = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("stderr = %q, want 'missing subcommand'", stderr.String())
	}
}

func TestEventEmitMissingType(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"event", "emit"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("gc event emit = %d, want 1 (missing type arg)", code)
	}
}
