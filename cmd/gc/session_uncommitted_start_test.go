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

// phantomCollisionProvider refuses every Start with ErrSessionExists while
// nothing runs under the name: a collision whose holder is gone by the time
// the start observes it.
type phantomCollisionProvider struct {
	*runtime.Fake
}

func (p *phantomCollisionProvider) Start(_ context.Context, name string, _ runtime.Config) error {
	return fmt.Errorf("%w: session %q", runtime.ErrSessionExists, name)
}

// TestRunPreparedStartCandidate_SessionExistsNotLiveDefers is AC-3: on any
// row, a start collision whose holder reads not live defers the start instead
// of failing it with provider_error, so no wake failure is recorded.
func TestRunPreparedStartCandidate_SessionExistsNotLiveDefers(t *testing.T) {
	for name, tc := range map[string]struct {
		extra     map[string]string
		ownLaunch bool
	}{
		"committed row":   {extra: map[string]string{"started_config_hash": "hash-old"}},
		"restart handoff": {ownLaunch: true},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
			city, workDir := t.TempDir(), t.TempDir()
			bead := createUncommittedRestartRow(t, store, clk, workDir, tc.extra)
			sp := &phantomCollisionProvider{Fake: runtime.NewFake()}
			item, release := uncommittedStartItem(t, city, bead, tc.ownLaunch)
			defer release()

			result, committed, stderr := runUncommittedStart(t, city, store, sp, clk, item)

			if result.err != nil || result.outcome != TraceOutcomeDeferred || committed {
				t.Fatalf("result err=%v outcome=%q committed=%v, want an uncommitted deferral; stderr=%s", result.err, result.outcome, committed, stderr)
			}
			if !errors.Is(result.unattributedErr, runtime.ErrSessionExists) {
				t.Errorf("unattributedErr = %v, want the collision for the lifecycle log", result.unattributedErr)
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", bead.ID, err)
			}
			if v := got.Metadata["wake_attempts"]; v != "" {
				t.Errorf("wake_attempts = %q, want no wake failure recorded", v)
			}
			if got := countCalls(sp.Fake, "Stop"); got != 0 {
				t.Errorf("Stop calls = %d, want none", got)
			}
		})
	}
}

// TestRunPreparedStartCandidate_SessionExistsUncommittedOursConverges is
// AC-4: on an uncommitted, prompt-bearing row the colliding runtime carries
// this attempt's instance token, so it is this start's launch and the start
// converges and commits as any collision does.
func TestRunPreparedStartCandidate_SessionExistsUncommittedOursConverges(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	city, workDir := t.TempDir(), t.TempDir()
	bead := createUncommittedRestartRow(t, store, clk, workDir, nil)
	item, release := uncommittedStartItem(t, city, bead, true)
	defer release()
	sp := &collidingOccupantProvider{Fake: runtime.NewFake(), id: bead.ID, token: item.candidate.info.InstanceToken}

	result, committed, stderr := runUncommittedStart(t, city, store, sp, clk, item)

	if result.err != nil || result.outcome != TraceOutcomeSessionExists || !committed {
		t.Fatalf("result err=%v outcome=%q committed=%v, want a converged commit; stderr=%s", result.err, result.outcome, committed, stderr)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if v := got.Metadata["started_config_hash"]; v != "hash-new" {
		t.Errorf("started_config_hash = %q, want hash-new committed", v)
	}
	if got := countCalls(sp.Fake, "Stop"); got != 0 {
		t.Errorf("Stop calls = %d, want none", got)
	}
}

// assertWarmReuseUnchanged runs the prepared start of a row whose runtime is up, stamped
// as the row's own incarnation, and asserts AC-5: the start reuses it warm
// (no launch, no stop) and commits.
func assertWarmReuseUnchanged(t *testing.T, extra map[string]string) {
	t.Helper()
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	city, workDir := t.TempDir(), t.TempDir()
	bead := createUncommittedRestartRow(t, store, clk, workDir, extra)
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "claude"}); err != nil {
		t.Fatal(err)
	}
	stampOccupant(t, sp, "worker", bead.ID, "tok-row")
	prepared, err := prepareStartCandidate(startCandidate{
		info: sessiontest.SeedBead(t, bead),
		tp:   TemplateParams{Command: "claude", Prompt: uncommittedStartPrompt, SessionName: "worker", TemplateName: "worker"},
	}, &config.City{}, store, clk)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}
	if prepared.ownLaunchRequired {
		t.Fatal("ownLaunchRequired = true on a row committed before its PreWake")
	}
	item, release := leasedStart(t, city, *prepared)
	defer release()

	result, committed, stderr := runUncommittedStart(t, city, store, sp, clk, item)

	if result.err != nil || result.outcome != TraceOutcomeSuccess || !committed {
		t.Fatalf("result err=%v outcome=%q committed=%v, want a committed warm reuse; stderr=%s", result.err, result.outcome, committed, stderr)
	}
	if got := countCalls(sp, "Start"); got != 1 {
		t.Errorf("Start calls = %d, want only the existing runtime's (warm reuse)", got)
	}
	if got := countCalls(sp, "Stop"); got != 0 {
		t.Errorf("Stop calls = %d, want none", got)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", bead.ID, err)
	}
	if v := got.Metadata["started_config_hash"]; v == "" {
		t.Error("started_config_hash empty, want the warm reuse committed")
	}
}

// TestStartPreparedStartCandidate_CommittedRowWarmReuseUnchanged is AC-5 for a
// committed row.
func TestStartPreparedStartCandidate_CommittedRowWarmReuseUnchanged(t *testing.T) {
	assertWarmReuseUnchanged(t, map[string]string{"started_config_hash": "hash-old"})
}

// TestStartPreparedStartCandidate_FreshModePoolSlotWarmReuseUnchanged is AC-5
// for a wake_mode = "fresh" slot, whose PreWake clears started_config_hash.
func TestStartPreparedStartCandidate_FreshModePoolSlotWarmReuseUnchanged(t *testing.T) {
	assertWarmReuseUnchanged(t, map[string]string{"started_config_hash": "hash-old", "wake_mode": "fresh"})
}

// uncommittedLiveStartEnv is a named session whose start never committed: an
// awake row with no started_config_hash, a prompt-bearing template, and a live
// runtime a non-controller start launched under the row's ID.
type uncommittedLiveStartEnv struct {
	*restartRequestTestEnv
	session beads.Bead
	name    string
}

func newUncommittedLiveStartEnv(t *testing.T) *uncommittedLiveStartEnv {
	t.Helper()
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[name] = TemplateParams{Command: "true", Prompt: uncommittedStartPrompt, SessionName: name, TemplateName: "worker"}
	session := env.createSessionBead(name)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "awake",
		"last_woke_at":               env.clk.Now().UTC().Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	stampOccupant(t, env.sp, name, session.ID, "tok-occupant")
	return &uncommittedLiveStartEnv{restartRequestTestEnv: env, session: session, name: name}
}

// tick runs one reconcile pass over the row's current state.
func (e *uncommittedLiveStartEnv) tick(t *testing.T) {
	t.Helper()
	b, err := e.store.Get(e.session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(), e.city, []beads.Bead{b}, e.desiredState, cfgNames, e.cfg, e.sp, e.store,
		newDrainOps(e.sp), nil, nil, nil, e.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, e.startOptions...,
	)
}

// TestReconcileSessionBeads_EvictsLiveUncommittedStartAfterStartupTimeout is
// AC-6: a live runtime the row never committed a start for is left alone
// through the startup timeout, then stopped, so the controller's own start
// can launch it with its prompt.
func TestReconcileSessionBeads_EvictsLiveUncommittedStartAfterStartupTimeout(t *testing.T) {
	env := newUncommittedLiveStartEnv(t)

	env.tick(t)
	if got := countCalls(env.sp, "Stop"); got != 0 || !env.sp.IsRunning(env.name) {
		t.Fatalf("Stop calls = %d running=%v inside the startup timeout, want the runtime left alone; stderr=%s", got, env.sp.IsRunning(env.name), env.stderr.String())
	}

	env.clk.Time = env.clk.Time.Add(env.cfg.Session.StartupTimeoutDuration() + time.Second)
	env.tick(t)

	if env.sp.IsRunning(env.name) {
		t.Fatalf("runtime still running past the startup timeout with no committed start; stdout=%s stderr=%s", env.stdout.String(), env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "never committed") {
		t.Errorf("stderr = %q, want the eviction logged", env.stderr.String())
	}
	assertNoStartCommitted(t, env.store, env.session.ID)
}

// uncommittedEvictCall is one evictUncommittedLiveStartIfDue call's stubbed
// acts and what they saw.
type uncommittedEvictCall struct {
	attached    bool
	attachedErr error
	stopErr     error
	stops       []sessionpkg.Decided
}

func (c *uncommittedEvictCall) acts() uncommittedStartActs {
	return uncommittedStartActs{
		attached: func() (bool, error) { return c.attached, c.attachedErr },
		stop: func(d sessionpkg.Decided) error {
			c.stops = append(c.stops, d)
			return c.stopErr
		},
	}
}

// TestEvictUncommittedLiveStart_Guards is AC-7: the AC-6 row with exactly one
// condition broken stops nothing, and an unattributed holder is reported once
// per episode.
func TestEvictUncommittedLiveStart_Guards(t *testing.T) {
	const name = "worker"
	t0 := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	timeout := time.Minute
	due := t0.Add(timeout + time.Second)
	row := sessionpkg.Info{ID: "gc-worker", Generation: "3", InstanceToken: "tok-row", LastWokeAt: t0.Format(time.RFC3339)}
	tp := TemplateParams{Prompt: uncommittedStartPrompt, SessionName: name, TemplateName: "worker"}

	for _, c := range []struct {
		name     string
		info     func(*sessionpkg.Info)
		tp       func(*TemplateParams)
		occupant func(*testing.T, *runtime.Fake)
		call     uncommittedEvictCall
		now      time.Time
		wantStop bool
		wantLog  string
	}{
		{name: "due", now: due, wantStop: true, wantLog: "never committed a start"},
		{name: "inside the startup timeout", now: t0.Add(timeout)},
		{name: "a newer reset commit restarts the wait", now: due, info: func(i *sessionpkg.Info) { i.ResetCommittedAt = t0.Add(30 * time.Second).Format(time.RFC3339) }},
		{name: "attached", now: due, call: uncommittedEvictCall{attached: true}},
		{name: "attach probe fails", now: due, call: uncommittedEvictCall{attached: true, attachedErr: errors.New("probe failed")}},
		{name: "no prompt", now: due, tp: func(p *TemplateParams) { p.Prompt = "  " }},
		{name: "start committed", now: due, info: func(i *sessionpkg.Info) { i.StartedConfigHash = "h-1" }},
		{name: "pending-create claim", now: due, info: func(i *sessionpkg.Info) { i.PendingCreateClaim = true }},
		{name: "operator suspend", now: due, info: func(i *sessionpkg.Info) { i.SleepIntent = "user-hold" }},
		{name: "live hold", now: due, info: func(i *sessionpkg.Info) { i.HeldUntil = due.Add(time.Hour).Format(time.RFC3339) }},
		{name: "foreign identity", now: due, wantLog: "not attributed", occupant: func(t *testing.T, sp *runtime.Fake) {
			stampOccupant(t, sp, name, "gc-other", "tok-other")
		}},
		{name: "unstamped identity", now: due, wantLog: "not attributed", occupant: func(*testing.T, *runtime.Fake) {}},
		{name: "unreadable identity", now: due, wantLog: "not attributed", occupant: func(_ *testing.T, sp *runtime.Fake) {
			sp.GetMetaErrors[name] = map[string]error{"GC_SESSION_ID": errors.New("tmux unreachable")}
		}},
		{name: "lease held by a start", now: due, call: uncommittedEvictCall{stopErr: fmt.Errorf("killing: %w", sessionpkg.ErrRuntimeLeaseBusy)}},
		{name: "start in flight", now: due, call: uncommittedEvictCall{stopErr: sessionpkg.ErrSessionStarting}},
		{name: "premise moved after the decision", now: due, call: uncommittedEvictCall{stopErr: sessionpkg.ErrKillPremiseMoved}},
	} {
		t.Run(c.name, func(t *testing.T) {
			info, params := row, tp
			if c.info != nil {
				c.info(&info)
			}
			if c.tp != nil {
				c.tp(&params)
			}
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
				t.Fatal(err)
			}
			if c.occupant != nil {
				c.occupant(t, sp)
			} else {
				stampOccupant(t, sp, name, row.ID, "tok-occupant")
			}
			dt := newDrainTracker()
			dt.uncommittedStartSeen(info, t0)
			var stderr strings.Builder
			call := c.call

			got := evictUncommittedLiveStartIfDue(t.TempDir(), sp, call.acts(), info, params, name, timeout, c.now, dt, &stderr, nil)

			wantStops := 0
			if c.wantStop || c.call.stopErr != nil {
				wantStops = 1
			}
			if got != c.wantStop || len(call.stops) != wantStops {
				t.Fatalf("evicted = %v with %d stops, want %v with %d; stderr=%s", got, len(call.stops), c.wantStop, wantStops, stderr.String())
			}
			if wantStops == 1 {
				committed := info
				committed.StartedConfigHash = "h-1"
				if call.stops[0].Match(committed) {
					t.Error("stop decided on facts a committed start still matches, want FactsLegacyUncommittedEvict's")
				}
			}
			if c.wantLog == "" {
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want nothing logged", stderr.String())
				}
				return
			}
			if n := strings.Count(stderr.String(), c.wantLog); n != 1 {
				t.Fatalf("stderr = %q, want %q logged once", stderr.String(), c.wantLog)
			}
			if c.wantStop {
				return
			}
			// The same episode reports an unattributed holder only once.
			evictUncommittedLiveStartIfDue(t.TempDir(), sp, call.acts(), info, params, name, timeout, c.now.Add(time.Second), dt, &stderr, nil)
			if n := strings.Count(stderr.String(), c.wantLog); n != 1 {
				t.Errorf("stderr = %q after a second tick, want %q logged once per episode", stderr.String(), c.wantLog)
			}
		})
	}
}

// TestEvictUncommittedLiveStart_FirstSightingBasis pins the age basis when
// neither the provider nor the row dates the runtime: the startup timeout runs
// from the first tick that saw it, and a new tracker starts it again.
func TestEvictUncommittedLiveStart_FirstSightingBasis(t *testing.T) {
	const name = "worker"
	t0 := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	info := sessionpkg.Info{ID: "gc-worker", Generation: "3", InstanceToken: "tok-row"}
	tp := TemplateParams{Prompt: uncommittedStartPrompt, SessionName: name, TemplateName: "worker"}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	stampOccupant(t, sp, name, info.ID, "tok-occupant")
	var call uncommittedEvictCall
	later := t0.Add(time.Minute + time.Second)

	dt := newDrainTracker()
	if evictUncommittedLiveStartIfDue(t.TempDir(), sp, call.acts(), info, tp, name, time.Minute, t0, dt, nil, nil) {
		t.Fatal("evicted on first sighting")
	}
	if evictUncommittedLiveStartIfDue(t.TempDir(), sp, call.acts(), info, tp, name, time.Minute, later, newDrainTracker(), nil, nil) {
		t.Fatal("evicted under a new tracker, want its first sighting to restart the wait")
	}
	moved := info
	moved.Generation = "4"
	if evictUncommittedLiveStartIfDue(t.TempDir(), sp, call.acts(), moved, tp, name, time.Minute, later, dt, nil, nil) {
		t.Fatal("evicted a new incarnation on the old incarnation's sighting")
	}
	dt = newDrainTracker()
	dt.uncommittedStartSeen(info, t0)
	if !evictUncommittedLiveStartIfDue(t.TempDir(), sp, call.acts(), info, tp, name, time.Minute, later, dt, nil, nil) {
		t.Fatal("not evicted past the startup timeout from its first sighting")
	}
}
