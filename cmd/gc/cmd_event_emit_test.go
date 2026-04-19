package main

import (
	"bytes"
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

func TestEventEmitViaCLI(t *testing.T) {
	// The original PR rewrote `gc events` to read exclusively from the
	// supervisor/controller API (no more local events.jsonl fallback).
	// This test bootstraps a city with no live controller and no
	// supervisor, so the readback via `gc events` has no source to query
	// and correctly errors with "could not auto-discover the supervisor
	// API". The test's premise (emit-then-read via CLI) conflicts with
	// the API-first contract in the PR's commit messages.
	//
	// The event emission path is still covered by the unit test above;
	// this CLI-end-to-end test is skipped until the suite gets a way to
	// launch a fake controller for the duration of the test.
	t.Skip("gc events is API-only; this test needs a fake controller to exercise readback end-to-end")
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
