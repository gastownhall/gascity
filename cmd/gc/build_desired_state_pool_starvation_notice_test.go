package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

const poolStarvationTag = "POOL-STARVATION-SKIP:"

// resetPoolWorktreeEvidenceSkipNotices clears the process-wide notice state
// before and after a test so each one sees the first hit for its beads.
func resetPoolWorktreeEvidenceSkipNotices(t *testing.T) {
	t.Helper()
	reset := func() {
		poolWorktreeEvidenceSkipNoticed.Range(func(k, _ any) bool {
			poolWorktreeEvidenceSkipNoticed.Delete(k)
			return true
		})
	}
	reset()
	t.Cleanup(reset)
}

// The notice must be greppable by a distinctive tag and name the pool, the
// session, the work bead, and the one command an operator runs next.
func TestNoticePoolWorktreeEvidenceSkipNamesBeadAndHint(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	var stderr bytes.Buffer
	err := fmt.Errorf("%w: bead %q does not match request bead %q", errPoolTriggerWorktreeEvidence, "gc-old", "gc-new")

	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err)

	got := stderr.String()
	if !strings.HasPrefix(got, poolStarvationTag) {
		t.Fatalf("stderr = %q, want it to start with %q", got, poolStarvationTag)
	}
	for _, want := range []string{`"rig/pool"`, "session sess-1", "work bead gc-new", err.Error(), "bd show gc-new"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr = %q, want it to contain %q", got, want)
		}
	}
}

// A fresh-create skip has no session yet; the notice must not print an empty
// session field.
func TestNoticePoolWorktreeEvidenceSkipOmitsEmptySession(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	var stderr bytes.Buffer

	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "", "gc-new", fmt.Errorf("%w invalid: partial", errPoolTriggerWorktreeEvidence))

	if got, want := stderr.String(), `pool "rig/pool" work bead gc-new:`; !strings.Contains(got, want) {
		t.Fatalf("stderr = %q, want %q with no session field for a fresh-create skip", got, want)
	}
}

// buildDesiredState reaches the skip every tick while the evidence stays bad;
// the same error for the same bead logs once.
func TestNoticePoolWorktreeEvidenceSkipDedupsIdenticalHits(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	var stderr bytes.Buffer
	err := fmt.Errorf("%w: verification failed", errPoolTriggerWorktreeEvidence)

	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err)
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-2", "gc-new", err)

	if count := strings.Count(stderr.String(), poolStarvationTag); count != 1 {
		t.Fatalf("stderr = %q, saw %d notices, want 1", stderr.String(), count)
	}
}

// A changed error on the same bead is a state change and logs again, including
// a return to an earlier error; other beads are tracked independently.
func TestNoticePoolWorktreeEvidenceSkipReLogsOnStateChange(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	var stderr bytes.Buffer
	err1 := fmt.Errorf("%w: verification failed", errPoolTriggerWorktreeEvidence)
	err2 := fmt.Errorf("%w: bead mismatch", errPoolTriggerWorktreeEvidence)

	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err1)
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err2)
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err1)
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-other", err1)

	if count := strings.Count(stderr.String(), poolStarvationTag); count != 4 {
		t.Fatalf("stderr = %q, saw %d notices, want 4", stderr.String(), count)
	}
}

// Once the bead stops being skipped, a relapse into the same error logs again.
func TestNoticePoolWorktreeEvidenceSkipReLogsAfterClear(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	var stderr bytes.Buffer
	err := fmt.Errorf("%w: verification failed", errPoolTriggerWorktreeEvidence)

	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err)
	clearPoolWorktreeEvidenceSkipNotice("gc-new")
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", err)

	if count := strings.Count(stderr.String(), poolStarvationTag); count != 2 {
		t.Fatalf("stderr = %q, saw %d notices, want 2", stderr.String(), count)
	}
}

func TestNoticePoolWorktreeEvidenceSkipIsNilSafe(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	noticePoolWorktreeEvidenceSkip(nil, "rig/pool", "sess-1", "gc-new", errors.New("boom"))
	var stderr bytes.Buffer
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", nil)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty on nil err", stderr.String())
	}
	// Neither call recorded state, so a real hit still logs.
	noticePoolWorktreeEvidenceSkip(&stderr, "rig/pool", "sess-1", "gc-new", errors.New("boom"))
	if count := strings.Count(stderr.String(), poolStarvationTag); count != 1 {
		t.Fatalf("stderr = %q, saw %d notices, want 1", stderr.String(), count)
	}
}

func poolStarvationNoticeWorkerConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
}

// A ready work bead whose worktree evidence is unusable gets no fresh session.
// Across repeated reconciler ticks the skip logs one tagged notice instead of
// one generic line per tick.
func TestRealizePoolDesiredSessionsFreshCreateEvidenceSkipLogsOnce(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	store := beads.NewMemStore()
	cfg := poolStarvationNoticeWorkerConfig()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	state := PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{
			Template:      "worker",
			Tier:          "new",
			WorkBeadID:    "gc-stuck",
			WorkStoreRef:  "rig:demo",
			WorktreeError: "work bead gc-stuck has gc.work_branch without complete worktree ownership evidence",
		}},
	}

	for tick := 0; tick < 3; tick++ {
		desired := map[string]TemplateParams{}
		realizePoolDesiredSessions(bp, &cfg.Agents[0], state, desired, &stderr)
		if len(desired) != 0 {
			t.Fatalf("tick %d: desired = %v, want the request skipped", tick, mapKeys(desired))
		}
	}

	if got := bp.sessionBeads.OpenInfos(); len(got) != 0 {
		t.Fatalf("created session beads = %d, want 0", len(got))
	}
	got := stderr.String()
	if count := strings.Count(got, poolStarvationTag); count != 1 {
		t.Fatalf("stderr = %q, saw %d notices over 3 ticks, want 1", got, count)
	}
	if !strings.Contains(got, "work bead gc-stuck") || !strings.Contains(got, "bd show gc-stuck") {
		t.Fatalf("stderr = %q, want the notice to name gc-stuck and the bd show hint", got)
	}
	if strings.Contains(got, "(skipping)") {
		t.Fatalf("stderr = %q, want the generic per-tick skip line replaced by the notice", got)
	}
}

// A reused pool session whose new trigger bead carries unusable evidence is
// left out of desired. The skip names the session and logs once per state.
func TestRealizePoolDesiredSessionsBindEvidenceSkipLogsOnce(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	store := beads.NewMemStore()
	reusable, err := store.Create(beads.Bead{
		Title:  "worker reusable",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                        "worker",
			"agent_name":                      "worker-1",
			"alias":                           "worker-1",
			"session_name":                    "worker-reusable-1",
			"state":                           string(sessionpkg.StateAsleep),
			"pool_slot":                       "1",
			poolManagedMetadataKey:            boolMetadata(true),
			beadmeta.TriggerBeadIDMetadataKey: "gc-old",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := poolStarvationNoticeWorkerConfig()
	snapshot := &sessionBeadSnapshot{}
	snapshot.addInfo(sessiontest.SeedBead(t, reusable))
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = snapshot
	state := PoolDesiredState{
		Template: "worker",
		Requests: []SessionRequest{{
			Template:      "worker",
			Tier:          "resume",
			SessionBeadID: reusable.ID,
			WorkBeadID:    "gc-stuck",
			WorktreeError: "work bead gc-stuck has gc.work_branch without complete worktree ownership evidence",
		}},
	}

	for tick := 0; tick < 3; tick++ {
		desired := map[string]TemplateParams{}
		realizePoolDesiredSessions(bp, &cfg.Agents[0], state, desired, &stderr)
		if len(desired) != 0 {
			t.Fatalf("tick %d: desired = %v, want the reused session left out", tick, mapKeys(desired))
		}
	}

	got := stderr.String()
	if count := strings.Count(got, poolStarvationTag); count != 1 {
		t.Fatalf("stderr = %q, saw %d notices over 3 ticks, want 1", got, count)
	}
	if !strings.Contains(got, "session "+reusable.ID) || !strings.Contains(got, "bd show gc-stuck") {
		t.Fatalf("stderr = %q, want the notice to name session %s and the bd show hint", got, reusable.ID)
	}
}

// When the evidence is repaired and the bead binds, the notice state is
// forgotten, so a later relapse is visible again.
func TestRealizePoolDesiredSessionsClearsNoticeWhenBeadBinds(t *testing.T) {
	resetPoolWorktreeEvidenceSkipNotices(t)
	store := beads.NewMemStore()
	cfg := poolStarvationNoticeWorkerConfig()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	broken := SessionRequest{
		Template:      "worker",
		Tier:          "new",
		WorkBeadID:    "gc-stuck",
		WorktreeError: "partial evidence",
	}
	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{Template: "worker", Requests: []SessionRequest{broken}}, map[string]TemplateParams{}, &stderr)
	if _, ok := poolWorktreeEvidenceSkipNoticed.Load("gc-stuck"); !ok {
		t.Fatalf("notice state for gc-stuck missing after the skip; stderr=%q", stderr.String())
	}

	repaired := broken
	repaired.WorktreeError = ""
	desired := map[string]TemplateParams{}
	realizePoolDesiredSessions(bp, &cfg.Agents[0], PoolDesiredState{Template: "worker", Requests: []SessionRequest{repaired}}, desired, &stderr)
	if len(desired) != 1 {
		t.Fatalf("desired = %v, want the repaired bead to get a session; stderr=%q", mapKeys(desired), stderr.String())
	}
	if _, ok := poolWorktreeEvidenceSkipNoticed.Load("gc-stuck"); ok {
		t.Fatal("notice state for gc-stuck kept after the bead bound, want it forgotten")
	}
}
