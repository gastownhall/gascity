package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// A parked seat resumes its conversation where it ran (ga-ylpbsy.16, PR
// #8110 review 5592555088). The seat finished its step in one work dir and
// parked holding a pre-assigned pin; the work dir its pin's binding derives
// is another. When the pin turns ready, the same seat must wake with
// --resume and its saved session key, in the dir its transcript is filed
// under: a re-derived dir finds no transcript, so the launch would drop the
// key and start the seat fresh. Each test drives both copies (legacy's ticks
// and v2's passes) from the parked seat through its blocker's close to the
// wake.

// parkedResumeKey is the parked seat's conversation, the key its transcript
// is filed under (observedTranscript).
const parkedResumeKey = "k-1"

// parkedResume is both copies of one parked seat and the city's dirs:
// recorded is the work dir the seat ran in, derived the one its pin's binding
// derives from the worker's configured work_dir.
type parkedResume struct {
	cityPath, rigPath string
	recorded, derived string
	lw                *legacyWorld
	vw                *v2World
}

// parkedResumeCity is the worker city with a claude provider that resumes by
// --resume, a configured work dir per pool instance, and the transcript root
// the stale-resume probe searches.
func parkedResumeCity(transcripts string) func() *config.City {
	return func() *config.City {
		cfg := workerCity(3)
		standalone := ""
		cfg.Providers = map[string]config.ProviderSpec{"claude": {
			Command: "claude", Base: &standalone, PromptMode: "none",
			SessionIDFlag: "--session-id", ResumeFlag: "--resume", ResumeStyle: "flag",
		}}
		cfg.Agents[0].StartCommand = ""
		cfg.Agents[0].Provider = "claude"
		cfg.Agents[0].WorkDir = ".gc/worktrees/{{.AgentBase}}"
		cfg.Daemon.ObservePaths = []string{transcripts}
		return cfg
	}
}

// fakeClaudeOnPath puts an executable named claude first on PATH, which the
// provider's resolution looks up: nothing runs it.
func fakeClaudeOnPath(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
}

// newParkedResume seeds both copies with pool seat gc-1, asleep on its own
// drain an hour ago, its conversation k-1 committed in the work dir place
// names (given the city's and its rig's dirs), which it creates and files
// k-1's transcript under. The seat's last trigger is the step it finished,
// gs-1; it holds its pre-assigned pin gp-1, which gb-1 still blocks.
func newParkedResume(t *testing.T, place func(cityPath, rigPath string) string) *parkedResume {
	t.Helper()
	fakeClaudeOnPath(t)
	cityPath := t.TempDir()
	r := &parkedResume{cityPath: cityPath, rigPath: filepath.Join(cityPath, "rigs", simRigLeg)}
	r.recorded = place(cityPath, r.rigPath)
	r.derived = filepath.Join(cityPath, ".gc", "worktrees", "worker-1")
	for _, dir := range []string{r.rigPath, r.recorded} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	transcripts := observedTranscript(t, r.recorded)
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	f := parityFixture{
		Name: "a parked seat whose pin turns ready",
		City: parkedResumeCity(transcripts),
		Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep",
			"instance_token", "tok-gc-1", "sleep_reason", "idle", "slept_at", at(-time.Hour), "last_woke_at", at(-2*time.Hour),
			"session_key", parkedResumeKey, "started_config_hash", "started-before-the-park",
			beadmeta.LegacyWorkDirMetadataKey, r.recorded, beadmeta.WorkDirMetadataKey, r.recorded, beadmeta.TriggerBeadIDMetadataKey, "gs-1")},
		Work: []parityRow{
			{ID: "gs-1", Title: "rebase-check", Type: "task", Status: "closed", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}},
			{ID: "gb-1", Title: "human-approval", Type: "task", Status: "open"},
			{ID: "gp-1", Title: "finalize", Type: "task", Status: "open", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker", "gc.continuation_group": "main"}},
		},
		Deps: []beads.Dep{{IssueID: "gp-1", DependsOnID: "gb-1", Type: "blocks"}},
	}
	rows := f.converged(t, cityPath)
	r.lw = newLegacyWorld(t, f, cityPath, cloneDiffBeads(rows))
	r.lw.cfg.Rigs = []config.Rig{{Name: simRigLeg, Path: r.rigPath}}
	r.lw.rigs = map[string]beads.Store{simRigLeg: beads.NewMemStoreFrom(0, nil, nil)}
	r.vw = newV2World(t, f, cityPath, rows, []beads.Bead{}, r.rigPath, nil)
	return r
}

// settle runs each copy to its fixed point.
func (r *parkedResume) settle(t *testing.T, phase string) {
	t.Helper()
	r.lw.settle(t, phase)
	stop := advanceStaleKeyWaits(r.vw.s.clk)
	defer stop()
	r.vw.settle(t, phase)
}

// advanceStaleKeyWaits ends a keyed start's stale-key wait on the
// simulator's clock, which the passes do not move while an effect runs: each
// time a timer is armed staleKeyDetectDelay ahead, it advances the clock by
// that much, as TestResumedStartWaitsStaleKeyDetectDelay does by hand. Legacy
// takes the same wait through its immediate stale-key waiter. stop ends it.
func advanceStaleKeyWaits(clk *fakePlannerClock) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			clk.mu.Lock()
			want := clk.now.Add(staleKeyDetectDelay)
			armed := slices.ContainsFunc(clk.timers, func(tm *fakePlannerTimer) bool { return tm.active && tm.at.Equal(want) })
			changed := clk.changed
			clk.mu.Unlock()
			if armed {
				clk.Advance(staleKeyDetectDelay)
				continue
			}
			select {
			case <-changed:
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// closeBlocker closes gb-1 on both copies: gp-1 turns ready.
func (r *parkedResume) closeBlocker(t *testing.T) {
	t.Helper()
	for _, tw := range []twin{r.lw.twin(t), r.vw.twin(t)} {
		st, _ := tw.row("gb-1")
		if err := st.Close("gb-1"); err != nil {
			t.Fatal(err)
		}
	}
	r.vw.delivered()
}

// parkedResumeCopy is one copy as a check reads it: its starts and its seat.
type parkedResumeCopy struct {
	name   string
	starts []simStart
	seat   beads.Bead
}

// copies reads both copies now.
func (r *parkedResume) copies(t *testing.T) []parkedResumeCopy {
	t.Helper()
	read := func(name string, p *simProvider, store beads.Store) parkedResumeCopy {
		p.mu.Lock()
		starts := append([]simStart(nil), p.starts...)
		p.mu.Unlock()
		seat, err := store.Get("gc-1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return parkedResumeCopy{name: name, starts: starts, seat: seat}
	}
	return []parkedResumeCopy{
		read("legacy", r.lw.sp.simProvider, r.lw.store),
		read("v2", r.vw.s.sp, r.vw.s.legs[0].backing),
	}
}

// checkParked checks each copy holds the seat parked: asleep, never started,
// its conversation and recorded work dir kept for its wake.
func (r *parkedResume) checkParked(t *testing.T) {
	t.Helper()
	for _, c := range r.copies(t) {
		if len(c.starts) != 0 || c.seat.Status == "closed" || c.seat.Metadata["state"] != "asleep" {
			t.Fatalf("%s: seat %s %v, starts %+v; want it parked, asleep and never started", c.name, c.seat.Status, c.seat.Metadata, c.starts)
		}
		if got := c.seat.Metadata["session_key"]; got != parkedResumeKey {
			t.Fatalf("%s: parked seat's session_key %q, want its conversation %q kept", c.name, got, parkedResumeKey)
		}
	}
}

// wake is each copy's one start of the seat, for its pin: the same row,
// bound to gp-1.
func (r *parkedResume) wake(t *testing.T) map[string]simStart {
	t.Helper()
	out := map[string]simStart{}
	for _, c := range r.copies(t) {
		if len(c.starts) != 1 || c.starts[0].Name != "s-gc-1" || c.starts[0].ID != "gc-1" {
			t.Fatalf("%s: starts %+v, want the parked seat started once, in place", c.name, c.starts)
		}
		if got := c.seat.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != "gp-1" {
			t.Fatalf("%s: woken seat's trigger %q, want its pin gp-1", c.name, got)
		}
		out[c.name] = c.starts[0]
	}
	return out
}

// The reviewer's blocked-to-ready case: the seat ran in a dir that is not
// the one its pin's binding derives, and both copies resume its saved
// conversation there.
func TestParkedSeatResumesInItsRecordedWorkDirWhenItsPinTurnsReady(t *testing.T) {
	r := newParkedResume(t, func(cityPath, _ string) string {
		return filepath.Join(cityPath, ".gc", "worktrees", "gs-1-rebase-check")
	})
	if r.recorded == r.derived {
		t.Fatalf("recorded %s is the derived dir: the case needs them apart", r.recorded)
	}
	r.settle(t, "parked")
	r.checkParked(t)
	for _, c := range r.copies(t) {
		for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			if got := c.seat.Metadata[key]; got != r.recorded {
				t.Errorf("%s: parked seat's %s %q, want the dir it ran in, %s", c.name, key, got, r.recorded)
			}
		}
	}
	r.closeBlocker(t)
	r.settle(t, "woken")
	starts := r.wake(t)
	for _, c := range r.copies(t) {
		start := starts[c.name]
		if want := "--resume " + parkedResumeKey; !strings.Contains(start.Command, want) || strings.Contains(start.Command, "--session-id") {
			t.Errorf("%s: launch %q, want the saved conversation resumed (%q), not a fresh one", c.name, start.Command, want)
		}
		if start.WorkDir != r.recorded {
			t.Errorf("%s: launched in %q, want the recorded work dir %s, where its transcript is", c.name, start.WorkDir, r.recorded)
		}
		if got := c.seat.Metadata["session_key"]; got != parkedResumeKey {
			t.Errorf("%s: session_key %q, want %q kept", c.name, got, parkedResumeKey)
		}
		for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			if got := c.seat.Metadata[key]; got != r.recorded {
				t.Errorf("%s: woken seat's %s %q, want %s kept", c.name, key, got, r.recorded)
			}
		}
	}
}

// checkFreshInDerived checks each copy started the woken seat fresh in the
// derived work dir: a new conversation under a new key, its binding moved
// off the recorded dir.
func (r *parkedResume) checkFreshInDerived(t *testing.T) {
	t.Helper()
	starts := r.wake(t)
	for _, c := range r.copies(t) {
		start := starts[c.name]
		if strings.Contains(start.Command, "--resume") || !strings.Contains(start.Command, "--session-id") {
			t.Errorf("%s: launch %q, want a fresh conversation (--session-id): the recorded dir cannot host the resume", c.name, start.Command)
		}
		if start.WorkDir != r.derived {
			t.Errorf("%s: launched in %q, want the derived work dir %s", c.name, start.WorkDir, r.derived)
		}
		if got := c.seat.Metadata["session_key"]; got == parkedResumeKey || got == "" {
			t.Errorf("%s: session_key %q, want a new key for the fresh start", c.name, got)
		}
		for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
			if got := c.seat.Metadata[key]; got != r.derived {
				t.Errorf("%s: woken seat's %s %q, want the derived %s", c.name, key, got, r.derived)
			}
		}
	}
}

// A recorded work dir deleted while the seat is parked (a worktree reaper,
// a cleanup) cannot host the resume: both copies re-derive it, and the seat
// starts fresh where its pin's work runs.
func TestParkedSeatWhoseRecordedWorkDirIsGoneWakesFreshInTheDerivedDir(t *testing.T) {
	r := newParkedResume(t, func(cityPath, _ string) string {
		return filepath.Join(cityPath, ".gc", "worktrees", "gs-1-rebase-check")
	})
	r.settle(t, "parked")
	r.checkParked(t)
	if err := os.RemoveAll(r.recorded); err != nil {
		t.Fatal(err)
	}
	r.closeBlocker(t)
	r.settle(t, "woken")
	r.checkFreshInDerived(t)
}

// A recorded work dir in another rig than the one the seat's work runs in
// (here the rig's own checkout, while the worker is the city's) is not kept,
// though it exists and holds the transcript: both copies re-derive it, and
// the seat starts fresh.
func TestParkedSeatWhoseRecordedWorkDirIsInAnotherRigWakesFreshInTheDerivedDir(t *testing.T) {
	r := newParkedResume(t, func(_, rigPath string) string {
		return filepath.Join(rigPath, "worktrees", "gs-1")
	})
	r.settle(t, "parked")
	r.closeBlocker(t)
	r.settle(t, "woken")
	r.checkFreshInDerived(t)
}
