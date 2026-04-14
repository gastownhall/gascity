package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeFreshnessRunner returns a beads.CommandRunner closure that always returns the
// given output and error, ignoring its inputs. This pins production parsing
// behavior against a literal-JSON fixture.
func fakeFreshnessRunner(out []byte, err error) func(string, string, ...string) ([]byte, error) {
	return func(string, string, ...string) ([]byte, error) {
		return out, err
	}
}

// TestSweepWispFreshness_LiteralJSONFieldNames pins the JSON tag names that
// production code reads from `bd list --json`: `id`, `updated_at`, and
// `metadata.session_name`. If any of these tags are renamed without updating
// production parsing, this regression test fails.
func TestSweepWispFreshness_LiteralJSONFieldNames(t *testing.T) {
	const fixture = `[{"id":"bd-1","updated_at":"2026-01-01T12:00:00Z","metadata":{"session_name":"worker-1"}}]`
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner([]byte(fixture), nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wf, ok := got["worker-1"]
	if !ok {
		t.Fatalf("expected entry for session worker-1, got %v", got)
	}
	if wf.id != "bd-1" {
		t.Errorf("id: got %q want %q", wf.id, "bd-1")
	}
	want, _ := time.Parse(time.RFC3339, "2026-01-01T12:00:00Z")
	if !wf.updatedAt.Equal(want) {
		t.Errorf("updatedAt: got %v want %v", wf.updatedAt, want)
	}
}

// TestSweepWispFreshness_MostRecentWins verifies that when multiple wisps
// map to one session, the entry with the most-recent updated_at wins.
func TestSweepWispFreshness_MostRecentWins(t *testing.T) {
	const fixture = `[
		{"id":"bd-old","updated_at":"2026-01-01T10:00:00Z","metadata":{"session_name":"s"}},
		{"id":"bd-new","updated_at":"2026-01-01T12:00:00Z","metadata":{"session_name":"s"}},
		{"id":"bd-mid","updated_at":"2026-01-01T11:00:00Z","metadata":{"session_name":"s"}}
	]`
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner([]byte(fixture), nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["s"].id != "bd-new" {
		t.Fatalf("expected most-recent bd-new to win, got %q", got["s"].id)
	}
}

// TestSweepWispFreshness_EmptySessionSkipped verifies entries with empty
// session_name are dropped.
func TestSweepWispFreshness_EmptySessionSkipped(t *testing.T) {
	const fixture = `[{"id":"bd-1","updated_at":"2026-01-01T12:00:00Z","metadata":{"session_name":""}}]`
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner([]byte(fixture), nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// TestSweepWispFreshness_MalformedUpdatedAtSkipped verifies entries with an
// unparseable updated_at are dropped (not propagated as an error).
func TestSweepWispFreshness_MalformedUpdatedAtSkipped(t *testing.T) {
	const fixture = `[{"id":"bd-1","updated_at":"not-a-date","metadata":{"session_name":"s"}}]`
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner([]byte(fixture), nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map for malformed updated_at, got %v", got)
	}
}

// TestSweepWispFreshness_RunnerErrorReturnsError verifies that a runner
// failure surfaces as a wrapped error and a nil map.
func TestSweepWispFreshness_RunnerErrorReturnsError(t *testing.T) {
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner(nil, errors.New("bd exploded")))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil map on runner error, got %v", got)
	}
	if !strings.Contains(err.Error(), "listing in-progress wisps") {
		t.Fatalf("expected wrapped error context, got %v", err)
	}
}

// TestSweepWispFreshness_UnmarshalErrorReturnsError verifies that non-JSON
// output surfaces as a wrapped parse error.
func TestSweepWispFreshness_UnmarshalErrorReturnsError(t *testing.T) {
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner([]byte("not json"), nil))
	if err == nil {
		t.Fatal("expected unmarshal error, got nil")
	}
	if got != nil {
		t.Fatalf("expected nil map on unmarshal error, got %v", got)
	}
	if !strings.Contains(err.Error(), "parsing wisp list") {
		t.Fatalf("expected wrapped error context, got %v", err)
	}
}

// TestSweepWispFreshness_EmptyArrayReturnsEmptyMap verifies an empty JSON
// array yields an empty map and a nil error.
func TestSweepWispFreshness_EmptyArrayReturnsEmptyMap(t *testing.T) {
	got, err := sweepWispFreshness("/tmp/city", fakeFreshnessRunner([]byte("[]"), nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}
