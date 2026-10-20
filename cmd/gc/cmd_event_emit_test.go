package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
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
	t.Setenv("GC_CITY_PATH", dir)
	var stderr bytes.Buffer
	payload, missing := eventPayloadForEmit(`{"bead":}`, created.ID, &stderr)
	if missing != nil {
		t.Fatalf("bead %s missing: %v", created.ID, missing)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want none", stderr.String())
	}
	if !json.Valid([]byte(payload)) {
		t.Fatalf("payload is not valid JSON: %q", payload)
	}
	decoded, ok := beads.DecodeBeadEventPayload(json.RawMessage(payload))
	if !ok {
		t.Fatalf("payload is not a raw canonical bead snapshot: %s", payload)
	}
	if decoded.ID != created.ID {
		t.Fatalf("payload bead ID = %q, want %q", decoded.ID, created.ID)
	}
	if decoded.Title != "hook-created task" {
		t.Fatalf("payload bead title = %q, want hook-created task", decoded.Title)
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
	payload, missing := eventPayloadForEmit(`{"bead":}`, created.ID, &stderr)
	if missing != nil {
		t.Fatalf("bead %s missing: %v", created.ID, missing)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want none", stderr.String())
	}
	decoded, ok := beads.DecodeBeadEventPayload(json.RawMessage(payload))
	if !ok {
		t.Fatalf("payload is not a raw canonical bead snapshot: %s", payload)
	}
	if decoded.ID != created.ID {
		t.Fatalf("payload bead ID = %q, want %q", decoded.ID, created.ID)
	}
	if decoded.Title != "rig hook-created task" {
		t.Fatalf("payload bead title = %q, want rig hook-created task", decoded.Title)
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
	stubInitRemoteImports(t)

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr)
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

func TestEventBeadPayloadScopeRoot(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		city string // --city's path, when given
		want string
	}{
		{
			name: "BEADS_DIR absolute resolves to its parent",
			env:  map[string]string{"BEADS_DIR": "/city/root/.beads"},
			want: "/city/root",
		},
		{
			// filepath.Dir on a trailing-slash path does not strip the final
			// element the way a shell's dirname would, so an unclean BEADS_DIR
			// must not silently resolve to itself instead of its parent.
			name: "BEADS_DIR absolute with a trailing slash still resolves to its parent",
			env:  map[string]string{"BEADS_DIR": "/city/root/.beads/"},
			want: "/city/root",
		},
		{
			name: "GC_RIG_ROOT used when BEADS_DIR is unset",
			env:  map[string]string{"GC_RIG_ROOT": "/rig/root"},
			want: "/rig/root",
		},
		{
			// mc-zg8te: never the working directory when --city is given.
			name: "--city with no hook environment is the city",
			city: "/city/root",
			want: "/city/root",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"BEADS_DIR", "GC_RIG_ROOT"} {
				t.Setenv(k, "")
				_ = os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := eventBeadPayloadScopeRoot(resolvedContext{CityPath: tc.city}, tc.city != "", "")
			if err != nil {
				t.Fatalf("eventBeadPayloadScopeRoot() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("eventBeadPayloadScopeRoot() with env %v = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// eventEmitTestCity makes a file-store city, enters it, and returns its path
// with one created bead.
func eventEmitTestCity(t *testing.T) (string, beads.Bead) {
	t.Helper()
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
	created, err := store.Create(beads.Bead{Title: "step", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("GC_CITY_PATH", dir)
	return dir, created
}

func recordedEvents(t *testing.T, dir string) []events.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".gc", "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	var out []events.Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var evt events.Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, evt)
	}
	return out
}

// TestEventEmitRefusesCacheActorsAndBeadPayloads: the cache-* actor class is
// the controller's own, and a bead row-snapshot payload is a claim about a row
// the emitter could have read instead. Both refusals exit 1 and record
// nothing; in --json mode stdout carries the typed failure object.
func TestEventEmitRefusesCacheActorsAndBeadPayloads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alias string
		args  []string
		want  string
		code  string
	}{
		{name: "cache-local actor", args: []string{"bead.closed", "--subject", "gc-1", "--actor", cacheLocalActor}, want: "reserved for the controller's cache", code: eventEmitReservedActorCode},
		{name: "cache-reconcile actor", args: []string{"bead.updated", "--actor", cacheReconcileActor}, want: "reserved for the controller's cache", code: eventEmitReservedActorCode},
		{name: "other cache actor on a non-bead event", args: []string{"deploy.started", "--actor", "cache-anything"}, want: "reserved for the controller's cache", code: eventEmitReservedActorCode},
		{name: "implicit actor that is the cache's own", alias: cacheLocalActor, args: []string{"custom.x"}, want: "reserved for the controller's cache", code: eventEmitReservedActorCode},
		{name: "bead payload", args: []string{"bead.closed", "--subject", "gc-1", "--payload", `{"id":"gc-1","status":"closed"}`}, want: "--bead-payload", code: eventEmitBeadPayloadCode},
		{name: "bead payload with bead-payload too", args: []string{"bead.updated", "--subject", "gc-1", "--payload", `{"id":"gc-1"}`, "--bead-payload", "gc-1"}, want: "--bead-payload", code: eventEmitBeadPayloadCode},
		{name: "created payload", args: []string{"bead.created", "--payload", `{"id":"gc-1"}`}, want: "--bead-payload", code: eventEmitBeadPayloadCode},
		{name: "deleted payload", args: []string{"bead.deleted", "--payload", `{"id":"gc-1"}`}, want: "--bead-payload", code: eventEmitBeadPayloadCode},
		{name: "op.changed", args: []string{"op.changed", "--subject", "gc-op-aaaaaaaaaaaaaaaaaaaaaaaaaa", "--payload", `{"op":{"status":"closed"}}`}, want: "operation-journal row", code: eventEmitJournalDerivedCode},
		{name: "fact.recorded without a payload", args: []string{"fact.recorded", "--subject", "gc-fact-1"}, want: "operation-journal row", code: eventEmitJournalDerivedCode},
	} {
		for _, jsonOut := range []bool{false, true} {
			name := tc.name
			args := append([]string(nil), tc.args...)
			if jsonOut {
				name += " json"
				args = append(args, "--json")
			}
			t.Run(name, func(t *testing.T) {
				dir, _ := eventEmitTestCity(t)
				t.Setenv("GC_ALIAS", tc.alias)
				var stdout, stderr bytes.Buffer
				cmd := newEventEmitCmd(&stdout, &stderr)
				cmd.SetArgs(args)
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				if err := cmd.Execute(); !errors.Is(err, errExit) {
					t.Fatalf("Execute = %v, want errExit", err)
				}
				if jsonOut {
					var failure cliJSONErrorOutput
					if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
						t.Fatalf("stdout = %q, want a JSON failure object: %v", stdout.String(), err)
					}
					if failure.OK || failure.Error.Code != tc.code || failure.Error.ExitCode != 1 || !strings.Contains(failure.Error.Message, tc.want) {
						t.Fatalf("failure = %+v, want code %q, exit 1, message mentioning %q", failure, tc.code, tc.want)
					}
				} else {
					if !strings.Contains(stderr.String(), tc.want) {
						t.Fatalf("stderr = %q, want it to mention %q", stderr.String(), tc.want)
					}
					if stdout.Len() != 0 {
						t.Fatalf("stdout = %q, want nothing for a refused emit", stdout.String())
					}
				}
				if got := recordedEvents(t, dir); len(got) != 0 {
					t.Fatalf("recorded %+v, want nothing", got)
				}
			})
		}
	}
}

// TestEventEmitJSONRefusalThroughRunIsTyped: the global --json wrapper passes
// the refusal's typed failure object through instead of a generic
// command_failed.
func TestEventEmitJSONRefusalThroughRunIsTyped(t *testing.T) {
	dir, _ := eventEmitTestCity(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"event", "emit", "bead.closed", "--actor", cacheLocalActor, "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run = %d, want 1; stderr: %s", code, stderr.String())
	}
	var failure cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &failure); err != nil {
		t.Fatalf("stdout = %q, want one JSON failure object: %v", stdout.String(), err)
	}
	if failure.Error.Code != eventEmitReservedActorCode {
		t.Fatalf("failure code = %q, want %q", failure.Error.Code, eventEmitReservedActorCode)
	}
	if got := recordedEvents(t, dir); len(got) != 0 {
		t.Fatalf("recorded %+v, want nothing", got)
	}
}

// TestEventEmitAcceptsCachePrefixedAgentNamesAndNonSnapshotPayloads: only the
// cache's exact actors are reserved for the implicit actor, so an agent whose
// alias merely starts with cache- still emits; and only the row-snapshot
// lifecycle types refuse --payload.
func TestEventEmitAcceptsCachePrefixedAgentNamesAndNonSnapshotPayloads(t *testing.T) {
	dir, _ := eventEmitTestCity(t)
	t.Setenv("GC_ALIAS", "cache-warmer")
	for _, args := range [][]string{
		{"custom.x"},
		{events.BeadClaimRejected, "--subject", "gc-1", "--payload", `{"reason":"taken"}`},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newEventEmitCmd(&stdout, &stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v) = %v; stderr: %s", args, err, stderr.String())
		}
	}
	got := recordedEvents(t, dir)
	if len(got) != 2 || got[0].Actor != "cache-warmer" || got[1].Type != events.BeadClaimRejected || string(got[1].Payload) != `{"reason":"taken"}` {
		t.Fatalf("recorded %+v, want custom.x by cache-warmer and the claim_rejected payload", got)
	}
}

// TestEventEmitAllowsNonBeadPayloadsAndBeadPayloadFromTheStore: a bead.*
// event still carries a payload when gc reads it from the store
// (--bead-payload), and a non-bead event keeps --payload.
func TestEventEmitAllowsNonBeadPayloadsAndBeadPayloadFromTheStore(t *testing.T) {
	dir, created := eventEmitTestCity(t)
	for _, args := range [][]string{
		{"bead.closed", "--subject", created.ID, "--actor", "worker-1", "--bead-payload", created.ID},
		{"deploy.started", "--payload", `{"version":"2.1"}`, "--json"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newEventEmitCmd(&stdout, &stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v) = %v; stderr: %s", args, err, stderr.String())
		}
	}
	got := recordedEvents(t, dir)
	if len(got) != 2 {
		t.Fatalf("recorded %d events, want 2: %+v", len(got), got)
	}
	decoded, ok := beads.DecodeBeadEventPayload(got[0].Payload)
	if got[0].Type != events.BeadClosed || got[0].Actor != "worker-1" || !ok || decoded.ID != created.ID || decoded.Title != "step" {
		t.Fatalf("bead.closed event = %+v (payload %s), want the store's row for %s", got[0], got[0].Payload, created.ID)
	}
	if got[1].Type != "deploy.started" || string(got[1].Payload) != `{"version":"2.1"}` {
		t.Fatalf("deploy.started event = %+v, want its payload", got[1])
	}
}

// withCityFlag sets the --city persistent flag for the rest of one test.
func withCityFlag(t *testing.T, city string) {
	t.Helper()
	prev := cityFlag
	cityFlag = city
	t.Cleanup(func() { cityFlag = prev })
}

// emitBeadPayload runs gc event emit bead.updated --bead-payload id and
// returns the recorded event's decoded row.
func emitBeadPayload(t *testing.T, cityPath, id string) (beads.Bead, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newEventEmitCmd(&stdout, &stderr)
	cmd.SetArgs([]string{events.BeadUpdated, "--subject", id, "--actor", "worker-1", "--bead-payload", id})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute = %v; stderr: %s", err, stderr.String())
	}
	var updated []events.Event
	for _, e := range recordedEvents(t, cityPath) {
		if e.Type == events.BeadUpdated {
			updated = append(updated, e)
		}
	}
	if len(updated) != 1 {
		t.Fatalf("recorded %d bead.updated events, want 1: %+v; stderr: %s", len(updated), updated, stderr.String())
	}
	decoded, ok := beads.DecodeBeadEventPayload(updated[0].Payload)
	if !ok {
		return beads.Bead{}, stderr.String()
	}
	return decoded, stderr.String()
}

// TestEventEmitBeadPayloadHonorsTheCityFlag (mc-zg8te). Kills a
// --bead-payload read that finds its store from the working directory when
// --city names the city: run from another tree whose .beads is not this
// city's (staging read maintainer-city's .beads/config.yaml so), the payload
// is still the --city's row.
func TestEventEmitBeadPayloadHonorsTheCityFlag(t *testing.T) {
	dir, created := eventEmitTestCity(t)
	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, ".beads", "config.yaml"), []byte("not: [a valid config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(decoy)
	t.Setenv("GC_CITY_PATH", "")
	withCityFlag(t, dir)
	got, stderr := emitBeadPayload(t, dir, created.ID)
	if got.ID != created.ID || got.Title != "step" {
		t.Fatalf("payload row %+v (stderr %q), want the --city store's row %s", got, stderr, created.ID)
	}
}

// TestEventEmitBeadPayloadResolvesASessionRowInItsBinding (mc-zg8te). Kills a
// --bead-payload read of the work store alone: a session row lives in the
// sessions class's binding, and its payload is that row, not "bead not
// found".
func TestEventEmitBeadPayloadResolvesASessionRowInItsBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DOLT", "skip")
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatal(err)
	}
	row := mustCreateClassBead(t, classStore, beads.Bead{Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "worker-1", "state": "awake"}})
	t.Chdir(cityPath)
	withCityFlag(t, cityPath)
	got, stderr := emitBeadPayload(t, cityPath, row.ID)
	if got.ID != row.ID || got.Title != "worker-1" {
		t.Fatalf("payload row %+v (stderr %q), want the session row %s from its binding", got, stderr, row.ID)
	}
}

// TestEventEmitBeadPayloadNotFoundRecordsNothing (mc-zg8te, mc-op6ol). Kills
// an emit that reports ok, submitted, with no payload, for a --bead-payload
// bead no store of the city holds (J24's skew then went out payload-less and
// the controller never saw it): the command exits 1 with
// bead_payload_not_found, in --json mode as the failure object, and records
// no event.
func TestEventEmitBeadPayloadNotFoundRecordsNothing(t *testing.T) {
	dir, _ := eventEmitTestCity(t)
	for _, jsonOut := range []bool{false, true} {
		args := []string{events.BeadUpdated, "--subject", "gc-missing", "--bead-payload", "gc-missing"}
		if jsonOut {
			args = append(args, "--json")
		}
		var stdout, stderr bytes.Buffer
		cmd := newEventEmitCmd(&stdout, &stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("json %t: Execute = nil, want a failure; stdout %q", jsonOut, stdout.String())
		}
		out := stderr.String()
		if jsonOut {
			out = stdout.String()
		}
		if !strings.Contains(out, "gc-missing") || jsonOut && !strings.Contains(out, eventEmitBeadNotFoundCode) {
			t.Fatalf("json %t: output %q, want the missing bead named (and %s in --json)", jsonOut, out, eventEmitBeadNotFoundCode)
		}
	}
	if got := recordedEvents(t, dir); len(got) != 0 {
		t.Fatalf("recorded %+v, want nothing for a bead no store holds", got)
	}
}

// eventEmitRigCity is a file-store city with a bound rig "frontend" (prefix
// fe) holding one bead, entered from outside both, with no hook environment.
func eventEmitRigCity(t *testing.T) (cityDir, rigDir string, created beads.Bead) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)
	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"frontend\"\nprefix = \"fe\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{cityDir, rigDir} {
		if dir == cityDir {
			if err := ensureScopedFileStoreLayout(dir); err != nil {
				t.Fatal(err)
			}
		}
		if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
			t.Fatal(err)
		}
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if created, err = rigStore.Create(beads.Bead{Title: "rig task", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	for _, k := range []string{"GC_CITY_PATH", "BEADS_DIR", "GC_RIG_ROOT", "GC_RIG"} {
		t.Setenv(k, "")
	}
	return cityDir, rigDir, created
}

// withRigFlag sets the --rig persistent flag for the rest of one test.
func withRigFlag(t *testing.T, rig string) {
	t.Helper()
	prev := rigFlag
	rigFlag = rig
	t.Cleanup(func() { rigFlag = prev })
}

// TestEventEmitBeadPayloadFindsARigBeadWithTheCityFlag (the #7874 review).
// Kills a --city emit that reads only the city's work store for a rig's
// bead and reports it not found: with --rig, and without it by the bead's
// prefix, the payload is the rig's row.
func TestEventEmitBeadPayloadFindsARigBeadWithTheCityFlag(t *testing.T) {
	for _, withRig := range []bool{false, true} {
		t.Run(map[bool]string{false: "no --rig", true: "--rig frontend"}[withRig], func(t *testing.T) {
			cityDir, _, created := eventEmitRigCity(t)
			withCityFlag(t, cityDir)
			if withRig {
				withRigFlag(t, "frontend")
			}
			if got, stderr := emitBeadPayload(t, cityDir, created.ID); got.ID != created.ID || got.Title != "rig task" {
				t.Fatalf("payload row %+v (stderr %q), want the rig's row %s", got, stderr, created.ID)
			}
		})
	}
}

// TestEventEmitBeadPayloadEnvYieldsToTheCityFlag (the #7874 review). Kills an
// inherited BEADS_DIR or GC_RIG_ROOT, a rig agent's environment, that reads
// another city's store under --city: it is honored only when it names a root
// of the --city's own, its rig's here.
func TestEventEmitBeadPayloadEnvYieldsToTheCityFlag(t *testing.T) {
	for _, key := range []string{"BEADS_DIR", "GC_RIG_ROOT"} {
		t.Run(key+" naming another city", func(t *testing.T) {
			staging, stagingBead := eventEmitTestCity(t)
			other, _ := eventEmitTestCity(t) // its own bead shares the ID, titled the same
			if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"prod\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			otherStore, err := openStoreAtForCity(other, other)
			if err != nil {
				t.Fatal(err)
			}
			title := "production-row"
			if err := otherStore.Update(stagingBead.ID, beads.UpdateOpts{Title: &title}); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			t.Setenv("GC_CITY_PATH", "")
			withCityFlag(t, staging)
			if key == "BEADS_DIR" {
				t.Setenv(key, filepath.Join(other, ".beads"))
			} else {
				t.Setenv(key, other)
			}
			if got, stderr := emitBeadPayload(t, staging, stagingBead.ID); got.Title != "step" {
				t.Fatalf("payload row %+v (stderr %q), want the --city's row, not the inherited %s's", got, stderr, key)
			}
		})
		t.Run(key+" naming the city's rig", func(t *testing.T) {
			cityDir, rigDir, created := eventEmitRigCity(t)
			withCityFlag(t, cityDir)
			if key == "BEADS_DIR" {
				t.Setenv(key, filepath.Join(rigDir, ".beads"))
			} else {
				t.Setenv(key, rigDir)
			}
			if got, stderr := emitBeadPayload(t, cityDir, created.ID); got.ID != created.ID {
				t.Fatalf("payload row %+v (stderr %q), want the rig's row the inherited %s names", got, stderr, key)
			}
		})
	}
}

// TestEventEmitBeadPayloadOtherReadErrorRecordsTheEvent (the #7874 ruling).
// Kills a refusal on every read failure: a store that cannot be opened is
// reported and the event is still recorded, without the payload; only a
// bead its store does not hold is refused.
func TestEventEmitBeadPayloadOtherReadErrorRecordsTheEvent(t *testing.T) {
	city, created := eventEmitTestCity(t)
	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, ".beads", "config.yaml"), []byte("not: [a valid config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DIR", filepath.Join(decoy, ".beads")) // a hook's environment, no --city
	var stdout, stderr bytes.Buffer
	cmd := newEventEmitCmd(&stdout, &stderr)
	cmd.SetArgs([]string{events.BeadUpdated, "--subject", created.ID, "--bead-payload", created.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute = %v (stderr %q), want the event recorded", err, stderr.String())
	}
	got := recordedEvents(t, city)
	if len(got) != 1 || got[0].Type != events.BeadUpdated || len(got[0].Payload) != 0 || !strings.Contains(stderr.String(), "bead payload") {
		t.Fatalf("recorded %+v, stderr %q; want one bead.updated without payload and the read error reported", got, stderr.String())
	}
}

// TestEventEmitBeadDeletedRecordsItsSubject (the #7874 review). Kills a
// bead.deleted --bead-payload that fails because the bead is gone, which is
// the event's point: it is recorded with its subject alone, which still
// flags the controller's next pass.
func TestEventEmitBeadDeletedRecordsItsSubject(t *testing.T) {
	city, created := eventEmitTestCity(t)
	store, err := openStoreAtForCity(city, city)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := newEventEmitCmd(&stdout, &stderr)
	cmd.SetArgs([]string{events.BeadDeleted, "--subject", created.ID, "--bead-payload", created.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute = %v (stderr %q), want the deletion recorded", err, stderr.String())
	}
	if got := recordedEvents(t, city); len(got) != 1 || got[0].Type != events.BeadDeleted || got[0].Subject != created.ID {
		t.Fatalf("recorded %+v, want one bead.deleted for %s", got, created.ID)
	}
}

// TestBeadSnapshotEventTypesMatchBeadEventPayloadRegistrations: the --payload
// refusal set is exactly the event types registered with BeadEventPayload, so
// registering a new row-snapshot type cannot leave its --payload open.
func TestBeadSnapshotEventTypesMatchBeadEventPayloadRegistrations(t *testing.T) {
	snapshotType := reflect.TypeOf(api.BeadEventPayload{})
	want := map[string]bool{}
	for eventType, sample := range events.RegisteredPayloadTypes() {
		if reflect.TypeOf(sample) == snapshotType {
			want[eventType] = true
		}
	}
	if len(want) == 0 {
		t.Fatal("no event type is registered with api.BeadEventPayload")
	}
	if !maps.Equal(beadSnapshotEventTypes, want) {
		t.Fatalf("beadSnapshotEventTypes = %v, want the BeadEventPayload registrations %v",
			slices.Sorted(maps.Keys(beadSnapshotEventTypes)), slices.Sorted(maps.Keys(want)))
	}
}
