package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

const uncommittedStartPrompt = "rendered template prompt"

// createUncommittedRestartRow stores the row a restart handoff leaves: asleep,
// no started_config_hash, no priming markers, no pending-create claim, under
// generation 3 and token tok-row. extra overrides the defaults ("" deletes).
func createUncommittedRestartRow(t *testing.T, store beads.Store, clk clock.Clock, workDir string, extra map[string]string) beads.Bead {
	t.Helper()
	meta := map[string]string{
		"session_name":       "worker",
		"template":           "worker",
		"state":              "asleep",
		"provider":           "claude",
		"transport":          "tmux",
		"command":            "claude",
		"work_dir":           workDir,
		"generation":         "3",
		"continuation_epoch": "7",
		"instance_token":     "tok-row",
		"reset_committed_at": clk.Now().Format(time.RFC3339),
	}
	for k, v := range extra {
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	bead, err := store.Create(beads.Bead{
		ID:       "gc-worker",
		Title:    "worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return bead
}

// stampOccupant stamps the runtime under name as a launch of session id under
// token, at no runtime epoch: what a non-controller start leaves.
func stampOccupant(t *testing.T, sp *runtime.Fake, name, id, token string) {
	t.Helper()
	for key, value := range map[string]string{"GC_SESSION_ID": id, "GC_INSTANCE_TOKEN": token} {
		if err := sp.SetMeta(name, key, value); err != nil {
			t.Fatal(err)
		}
	}
}

// uncommittedStartItem is the prepared start of bead, leased, with
// ownLaunchRequired as the prepare would compute it.
func uncommittedStartItem(t *testing.T, city string, bead beads.Bead, ownLaunch bool) (preparedStart, func()) {
	t.Helper()
	return leasedStart(t, city, preparedStart{
		candidate: startCandidate{
			info: sessiontest.SeedBead(t, bead),
			tp: TemplateParams{
				Command:      "claude",
				Prompt:       uncommittedStartPrompt,
				SessionName:  "worker",
				TemplateName: "worker",
			},
		},
		cfg:               runtime.Config{Command: "claude", WorkDir: bead.Metadata["work_dir"]},
		coreHash:          "hash-new",
		promptHash:        "prompt-new",
		ownLaunchRequired: ownLaunch,
	})
}

// runUncommittedStart runs item's start wave and commits its result.
func runUncommittedStart(t *testing.T, city string, store beads.Store, sp runtime.Provider, clk clock.Clock, item preparedStart) (startResult, bool, string) {
	t.Helper()
	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, store, nil, 10*time.Second, 1)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	var stderr strings.Builder
	committed := commitStartResult(results[0], sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, &stderr)
	return results[0], committed, stderr.String()
}

// assertNoStartCommitted fails unless the row gained none of a committed
// start's keys.
func assertNoStartCommitted(t *testing.T, store beads.Store, id string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	for _, key := range []string{"started_config_hash", sessionpkg.PrimedAtMetadataKey, sessionpkg.PromptHashMetadataKey} {
		if v := got.Metadata[key]; v != "" {
			t.Errorf("row %s = %q, want no start committed", key, v)
		}
	}
}

// collidingOccupantProvider brings up a runtime stamped as another
// incarnation of the session when Start is called, and refuses that Start
// with ErrSessionExists: the name was taken between the start's liveness read
// and its launch.
type collidingOccupantProvider struct {
	*runtime.Fake
	id, token string
}

func (p *collidingOccupantProvider) Start(ctx context.Context, name string, _ runtime.Config) error {
	if !p.IsRunning(name) {
		if err := p.Fake.Start(ctx, name, runtime.Config{Command: "occupant"}); err != nil {
			return err
		}
		for key, value := range map[string]string{"GC_SESSION_ID": p.id, "GC_INSTANCE_TOKEN": p.token} {
			if err := p.SetMeta(name, key, value); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
}

// TestStartPreparedStartCandidate_UncommittedPromptRowRefusesLiveOccupant is
// AC-1 by warm reuse: after a restart handoff, the controller's start finds
// the name held by a live runtime another incarnation of the session launched.
// It defers instead of reusing it warm, launches and stops nothing, and the
// row gains no commit.
func TestStartPreparedStartCandidate_UncommittedPromptRowRefusesLiveOccupant(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	city, workDir := t.TempDir(), t.TempDir()
	bead := createUncommittedRestartRow(t, store, clk, workDir, nil)
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "occupant"}); err != nil {
		t.Fatal(err)
	}
	stampOccupant(t, sp, "worker", bead.ID, "tok-occupant")
	item, release := uncommittedStartItem(t, city, bead, true)
	defer release()

	result, committed, stderr := runUncommittedStart(t, city, store, sp, clk, item)

	if result.err != nil || result.outcome != TraceOutcomeDeferred {
		t.Fatalf("result err=%v outcome=%q, want a deferral", result.err, result.outcome)
	}
	if !errors.Is(result.unattributedErr, runtime.ErrSessionExists) || !strings.Contains(result.unattributedErr.Error(), "live_incarnation") {
		t.Errorf("unattributedErr = %v, want the collision annotated live_incarnation", result.unattributedErr)
	}
	if committed {
		t.Errorf("deferred start reported committed; stderr=%s", stderr)
	}
	if got := countCalls(sp, "Start"); got != 1 {
		t.Errorf("Start calls = %d, want only the occupant's", got)
	}
	if got := countCalls(sp, "Stop"); got != 0 {
		t.Errorf("Stop calls = %d, want the occupant left running", got)
	}
	assertNoStartCommitted(t, store, bead.ID)
}

// TestRunPreparedStartCandidate_SessionExistsUncommittedOtherIncarnationDefers
// is AC-1 through the collision arm: the name is taken between the start's
// liveness read and its launch, by another incarnation of the session, whose
// runtime reads live. The start defers and commits nothing.
func TestRunPreparedStartCandidate_SessionExistsUncommittedOtherIncarnationDefers(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	city, workDir := t.TempDir(), t.TempDir()
	bead := createUncommittedRestartRow(t, store, clk, workDir, nil)
	sp := &collidingOccupantProvider{Fake: runtime.NewFake(), id: bead.ID, token: "tok-occupant"}
	item, release := uncommittedStartItem(t, city, bead, true)
	defer release()

	result, committed, stderr := runUncommittedStart(t, city, store, sp, clk, item)

	if result.err != nil || result.outcome != TraceOutcomeDeferred {
		t.Fatalf("result err=%v outcome=%q, want a deferral", result.err, result.outcome)
	}
	if !strings.Contains(fmt.Sprint(result.unattributedErr), "live_incarnation") {
		t.Errorf("unattributedErr = %v, want live_incarnation", result.unattributedErr)
	}
	if committed {
		t.Errorf("deferred start reported committed; stderr=%s", stderr)
	}
	if got := countCalls(sp.Fake, "Stop"); got != 0 {
		t.Errorf("Stop calls = %d, want the occupant left running", got)
	}
	assertNoStartCommitted(t, store, bead.ID)
}

// TestRunPreparedStartCandidate_SessionExistsUncommittedForeignDefers covers
// the other holders AC-1's collision arm defers on: another session's runtime
// and one whose identity cannot be read.
func TestRunPreparedStartCandidate_SessionExistsUncommittedForeignDefers(t *testing.T) {
	for name, tc := range map[string]struct {
		id      string
		metaErr error
	}{
		"another session": {id: "gc-other"},
		"unreadable":      {id: "gc-worker", metaErr: errors.New("tmux transport: timeout")},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
			city, workDir := t.TempDir(), t.TempDir()
			bead := createUncommittedRestartRow(t, store, clk, workDir, nil)
			sp := &collidingOccupantProvider{Fake: runtime.NewFake(), id: tc.id, token: "tok-occupant"}
			if tc.metaErr != nil {
				sp.GetMetaErrors["worker"] = map[string]error{"GC_SESSION_ID": tc.metaErr, "GC_INSTANCE_TOKEN": tc.metaErr}
			}
			item, release := uncommittedStartItem(t, city, bead, true)
			defer release()

			result, committed, _ := runUncommittedStart(t, city, store, sp, clk, item)

			if result.err != nil || result.outcome != TraceOutcomeDeferred || committed {
				t.Fatalf("result err=%v outcome=%q committed=%v, want an uncommitted deferral", result.err, result.outcome, committed)
			}
			if !errors.Is(result.unattributedErr, runtime.ErrSessionExists) || strings.Contains(result.unattributedErr.Error(), "live_incarnation") {
				t.Errorf("unattributedErr = %v, want the collision unattributed", result.unattributedErr)
			}
			assertNoStartCommitted(t, store, bead.ID)
		})
	}
}

// TestPrepareStartCandidate_OwnLaunchRequired pins when the prepare requires
// the start to launch its own runtime: only a row with no started_config_hash
// before its PreWake, no pending-create claim, and a prompt to deliver.
func TestPrepareStartCandidate_OwnLaunchRequired(t *testing.T) {
	cases := map[string]struct {
		extra  map[string]string
		prompt string
		want   bool
	}{
		"restart handoff":    {prompt: uncommittedStartPrompt, want: true},
		"committed":          {extra: map[string]string{"started_config_hash": "hash-old"}, prompt: uncommittedStartPrompt},
		"fresh-mode slot":    {extra: map[string]string{"started_config_hash": "hash-old", "wake_mode": "fresh"}, prompt: uncommittedStartPrompt},
		"no prompt":          {prompt: "  "},
		"pending-create row": {extra: map[string]string{"pending_create_claim": "true", "state": "creating"}, prompt: uncommittedStartPrompt},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
			bead := createUncommittedRestartRow(t, store, clk, t.TempDir(), tc.extra)
			prepared, err := prepareStartCandidate(startCandidate{
				info: sessiontest.SeedBead(t, bead),
				tp:   TemplateParams{Command: "claude", Prompt: tc.prompt, SessionName: "worker", TemplateName: "worker"},
			}, &config.City{}, store, clk)
			if err != nil {
				t.Fatalf("prepareStartCandidate: %v", err)
			}
			if prepared.ownLaunchRequired != tc.want {
				t.Fatalf("ownLaunchRequired = %v, want %v", prepared.ownLaunchRequired, tc.want)
			}
		})
	}
}
