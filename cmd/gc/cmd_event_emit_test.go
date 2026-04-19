package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

func TestDoEventEmitSuccess(t *testing.T) {
	ep := events.NewFake()

	var stderr bytes.Buffer
	doEventEmit(ep, events.BeadCreated, "gc-1", "Build Tower of Hanoi", "mayor", "", &stderr)
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
	if e.Type != events.BeadCreated {
		t.Errorf("Type = %q, want %q", e.Type, events.BeadCreated)
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
	doEventEmit(ep, events.BeadClosed, "gc-1", "", "", "", &stderr)

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
	doEventEmit(ep, events.BeadCreated, "gc-1", "task", "", "", &stderr)

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
	doEventEmit(ep, events.BeadCreated, "gc-1", "task", "", "", &stderr)

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
	doEventEmit(ep, events.BeadCreated, "gc-42", "Fix login bug", "polecat", payload, &stderr)

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
	doEventEmit(ep, events.BeadCreated, "gc-1", "task", "", "", &stderr)

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
	doEventEmit(ep, events.BeadCreated, "gc-1", "task", "", "not-json{", &stderr)
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
	code := run([]string{"init", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc init = %d; stderr: %s", code, stderr.String())
	}

	// Emit two events via the CLI. `gc event emit` is best-effort and
	// always returns 0, but it should still write the events locally.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--city", dir, "event", "emit", "bead.created", "--subject", "gc-1", "--message", "Build Hanoi"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc event emit bead.created = %d; stderr: %s", code, stderr.String())
	}

	code = run([]string{"--city", dir, "event", "emit", "bead.closed", "--subject", "gc-1", "--message", "Done"}, &stdout, &stderr)
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
	if created.Subject != "gc-1" {
		t.Errorf("line 0 subject = %q, want gc-1", created.Subject)
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
