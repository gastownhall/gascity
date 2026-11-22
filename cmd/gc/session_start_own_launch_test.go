package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// uncommittedStartRowMeta is the metadata of a session row right after a
// requested restart's handoff (gastownhall/gascity#6417): the row kept its
// identity and conversation but lost its start witness, so nothing that
// watches the row can tell whether the runtime under its name ever received
// the startup prompt.
func uncommittedStartRowMeta(sessionName string) map[string]string {
	return map[string]string{
		"session_name":   sessionName,
		"agent_name":     sessionName,
		"template":       "worker",
		"generation":     "2",
		"instance_token": "tok-ours",
		"state":          "asleep",
	}
}

func createUncommittedStartRow(t *testing.T, store beads.Store, id string, meta map[string]string) beads.Bead {
	t.Helper()
	m := uncommittedStartRowMeta("worker")
	for k, v := range meta {
		m[k] = v
	}
	b, err := store.Create(beads.Bead{
		ID:       id,
		Title:    "worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: m,
	})
	if err != nil {
		t.Fatalf("creating session row %s: %v", id, err)
	}
	return b
}

func ownLaunchTestItem(t *testing.T, b beads.Bead, prompt string) preparedStart {
	t.Helper()
	return preparedStart{
		candidate: startCandidate{
			info: sessiontest.SeedBead(t, b),
			tp: TemplateParams{
				Command:      "claude",
				SessionName:  b.Metadata["session_name"],
				TemplateName: "worker",
				Prompt:       prompt,
			},
		},
		cfg: runtime.Config{Command: "claude"},
	}
}

// TestStartPreparedStartCandidate_UncommittedPromptRowRefusesLiveOccupant is
// AC-1 of gastownhall/gascity#6417, by warm reuse: a row whose restart handoff
// cleared started_config_hash and whose template renders a prompt must not
// adopt a live runtime it did not launch, because warm reuse delivers the
// startup prompt to no one and commits the hash as if the start had run. The
// refusal is the claim refusal's shape: an error wrapping
// runtime.ErrSessionExists, leaving the collision classification to decide.
func TestStartPreparedStartCandidate_UncommittedPromptRowRefusesLiveOccupant(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("planting occupant runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     "gc-uncommitted",
		"GC_INSTANCE_TOKEN": "tok-occupant",
		"GC_RUNTIME_EPOCH":  "1",
	} {
		if err := sp.SetMeta("worker", key, value); err != nil {
			t.Fatalf("stamping occupant %s: %v", key, err)
		}
	}
	item := ownLaunchTestItem(t, createUncommittedStartRow(t, beads.NewMemStore(), "gc-uncommitted", nil), "rendered startup prompt")
	item.ownLaunchRequired = true

	starts, stops := sp.CountCalls("Start", "worker"), sp.CountCalls("Stop", "worker")
	started, err := startPreparedStartCandidate(context.Background(), item, t.TempDir(), nil, sp, nil, nil, immediateSessionStaleKeyDetectionWaiter, nil)
	if err == nil || !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("warm reuse over an uncommitted prompt row: err = %v, want an error wrapping runtime.ErrSessionExists", err)
	}
	if started {
		t.Fatal("the refusal must report that no runtime was started")
	}
	if got := sp.CountCalls("Start", "worker"); got != starts {
		t.Fatalf("Start calls = %d, want unchanged at %d: nothing may be launched", got, starts)
	}
	if got := sp.CountCalls("Stop", "worker"); got != stops {
		t.Fatalf("Stop calls = %d, want unchanged at %d: a deferral stops nothing (#2081)", got, stops)
	}
}

// TestRunPreparedStartCandidate_SessionExistsUncommittedOtherIncarnationDefers
// is AC-1 of gastownhall/gascity#6417, through the collision arm: when the
// name is held by another incarnation of the same session, the start outcome
// is deferred with no launch, no stop and no commit, so the row gains none of
// the start witness keys and a later pass can still replace the runtime.
func TestRunPreparedStartCandidate_SessionExistsUncommittedOtherIncarnationDefers(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	row := createUncommittedStartRow(t, store, "gc-uncommitted", map[string]string{"state": "active", "session_key": "key-1"})
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), row.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("planting occupant runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     row.ID,
		"GC_INSTANCE_TOKEN": "tok-occupant",
		"GC_RUNTIME_EPOCH":  "1",
	} {
		if err := sp.SetMeta(row.Metadata["session_name"], key, value); err != nil {
			t.Fatalf("stamping occupant %s: %v", key, err)
		}
	}
	item := ownLaunchTestItem(t, row, "rendered startup prompt")
	item.ownLaunchRequired = true

	starts, stops := sp.CountCalls("Start", row.Metadata["session_name"]), sp.CountCalls("Stop", row.Metadata["session_name"])
	result := runPreparedStartCandidate(context.Background(), item, t.TempDir(), sp, store, nil, 10*time.Second, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)

	if result.outcome != TraceOutcomeDeferred {
		t.Fatalf("outcome = %q, want %q (a colliding runtime another incarnation of this session launched is not this start's runtime)", result.outcome, TraceOutcomeDeferred)
	}
	if result.err != nil {
		t.Fatalf("a deferral carries no terminal error, got %v", result.err)
	}
	if result.rollbackPending {
		t.Fatal("the row holds no pending-create claim, so the deferral must not roll one back")
	}
	if result.unattributedErr == nil || !strings.Contains(result.unattributedErr.Error(), "live_incarnation") {
		t.Fatalf("unattributedErr = %v, want the live-incarnation account of the collision", result.unattributedErr)
	}
	if got := sp.CountCalls("Start", row.Metadata["session_name"]); got != starts {
		t.Fatalf("Start calls = %d, want unchanged at %d", got, starts)
	}
	if got := sp.CountCalls("Stop", row.Metadata["session_name"]); got != stops {
		t.Fatalf("Stop calls = %d, want unchanged at %d", got, stops)
	}
	if commitStartResultTraced(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard, nil) == startCommitSucceeded {
		t.Fatal("a deferred start must not commit")
	}
	got, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("get row after deferred commit: %v", err)
	}
	for _, key := range []string{"started_config_hash", "primed_at", "prompt_hash"} {
		if v := got.Metadata[key]; v != "" {
			t.Fatalf("row gained %s = %q; a start that never launched commits no witness keys", key, v)
		}
	}
}

// TestPrepareStartCandidateForCity_OwnLaunchRequiredRecordsPreWakeRow pins
// how the start records ownLaunchRequired (F1 of #6417's design): from the
// pre-PreWake row — an empty started_config_hash, no pending-create claim and
// a template that renders a prompt — so a fresh-mode warm pool slot whose hash
// was already committed keeps warm reuse, and pending-create rows keep their
// #2081 arms.
func TestPrepareStartCandidateForCity_OwnLaunchRequiredRecordsPreWakeRow(t *testing.T) {
	cases := []struct {
		name   string
		meta   map[string]string
		prompt string
		want   bool
	}{
		{
			name:   "handoff-cleared prompt row",
			meta:   map[string]string{},
			prompt: "rendered startup prompt",
			want:   true,
		},
		{
			name:   "committed before PreWake keeps warm reuse",
			meta:   map[string]string{"started_config_hash": "hash-before"},
			prompt: "rendered startup prompt",
			want:   false,
		},
		{
			name:   "pending-create claim keeps its own arms",
			meta:   map[string]string{"pending_create_claim": "true", "state": "creating"},
			prompt: "rendered startup prompt",
			want:   false,
		},
		{
			name:   "template renders no prompt",
			meta:   map[string]string{},
			prompt: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
			row := createUncommittedStartRow(t, store, "gc-own", tc.meta)
			tp := ownLaunchTestItem(t, row, tc.prompt).candidate.tp

			prepared, err := prepareStartCandidateForCity(
				startCandidate{info: sessiontest.SeedBead(t, row), tp: tp},
				"", "", &config.City{}, nil, store, clk, io.Discard, nil, dispatchOptionSources{},
			)
			if err != nil {
				t.Fatalf("prepareStartCandidateForCity: %v", err)
			}
			if prepared.ownLaunchRequired != tc.want {
				t.Fatalf("ownLaunchRequired = %v, want %v", prepared.ownLaunchRequired, tc.want)
			}
		})
	}
}
