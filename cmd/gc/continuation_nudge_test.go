package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func continuationPoolSession(id, sessionName string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name": sessionName,
			"pool_managed": "true",
			"template":     "agent-a",
			"alias":        "current-alias",
			"generation":   "1",
		},
	}
}

func continuationRoot(storeRef string) beads.Bead {
	return beads.Bead{
		ID:     "root-a",
		Status: "in_progress",
		Type:   "task",
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.RootStoreRefMetadataKey:    storeRef,
			beadmeta.RoutedToMetadataKey:        "fixture/agent-a",
			beadmeta.SessionNameMetadataKey:     "session-a",
		},
	}
}

func continuationStep(rootID, storeRef string) beads.Bead {
	return beads.Bead{
		ID:       "step-a",
		Status:   "open",
		Type:     "task",
		Assignee: "session-a",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.RootStoreRefMetadataKey:      storeRef,
			beadmeta.ContinuationGroupMetadataKey: "polecat-work",
			beadmeta.SessionAffinityMetadataKey:   "require",
			beadmeta.RoutedToMetadataKey:          "fixture/agent-a",
		},
	}
}

func continuationCandidateFixture(
	t *testing.T,
	cityName string,
	actualStoreRef string,
	root beads.Bead,
	step beads.Bead,
	ready bool,
) []ContinuationClaimCandidate {
	t.Helper()
	backing := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	readyAssigned := map[storeScopedBeadKey]bool{}
	if ready {
		readyAssigned[storeScopedBeadKey{StoreRef: actualStoreRef, ID: step.ID}] = true
	}
	return selectReadyContinuationClaimCandidates(
		cityName,
		[]beads.Bead{step},
		[]beads.Store{backing},
		[]string{actualStoreRef},
		readyAssigned,
	)
}

func TestSelectReadyContinuationClaimCandidates_RequiresReadyOpenExactProvenance(t *testing.T) {
	const (
		cityName       = "test-city"
		actualStoreRef = "fixture"
		canonicalRef   = "rig:fixture"
	)
	baseRoot := continuationRoot(canonicalRef)
	baseStep := continuationStep(baseRoot.ID, canonicalRef)

	tests := []struct {
		name  string
		root  beads.Bead
		step  beads.Bead
		ready bool
		want  int
	}{
		{name: "eligible", root: baseRoot, step: baseStep, ready: true, want: 1},
		{name: "not ready", root: baseRoot, step: baseStep, ready: false},
		{name: "blocked", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Status = "blocked"
			return b
		}(), ready: true},
		{name: "in progress", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Status = "in_progress"
			return b
		}(), ready: true},
		{name: "non task step", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Type = "message"
			return b
		}(), ready: true},
		{name: "unassigned", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Assignee = ""
			return b
		}(), ready: true},
		{name: "missing continuation group", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Metadata = cloneStringMap(baseStep.Metadata)
			delete(b.Metadata, beadmeta.ContinuationGroupMetadataKey)
			return b
		}(), ready: true},
		{name: "missing required affinity", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Metadata = cloneStringMap(baseStep.Metadata)
			delete(b.Metadata, beadmeta.SessionAffinityMetadataKey)
			return b
		}(), ready: true},
		{name: "wrong affinity", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Metadata = cloneStringMap(baseStep.Metadata)
			b.Metadata[beadmeta.SessionAffinityMetadataKey] = "prefer"
			return b
		}(), ready: true},
		{name: "missing root id", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Metadata = cloneStringMap(baseStep.Metadata)
			delete(b.Metadata, beadmeta.RootBeadIDMetadataKey)
			return b
		}(), ready: true},
		{name: "missing root store ref", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Metadata = cloneStringMap(baseStep.Metadata)
			delete(b.Metadata, beadmeta.RootStoreRefMetadataKey)
			return b
		}(), ready: true},
		{name: "cross store root ref", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.Metadata = cloneStringMap(baseStep.Metadata)
			b.Metadata[beadmeta.RootStoreRefMetadataKey] = "rig:other"
			return b
		}(), ready: true},
		{name: "missing root row", root: func() beads.Bead {
			b := baseRoot
			b.ID = "different-root"
			return b
		}(), step: baseStep, ready: true},
		{name: "root row wrong store provenance", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			b.Metadata[beadmeta.RootStoreRefMetadataKey] = "rig:other"
			return b
		}(), step: baseStep, ready: true},
		{name: "terminal root", root: func() beads.Bead {
			b := baseRoot
			b.Status = "closed"
			return b
		}(), step: baseStep, ready: true},
		{name: "open root", root: func() beads.Bead {
			b := baseRoot
			b.Status = "open"
			return b
		}(), step: baseStep, ready: true},
		{name: "non task root", root: func() beads.Bead {
			b := baseRoot
			b.Type = "session"
			return b
		}(), step: baseStep, ready: true},
		{name: "missing root session", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			delete(b.Metadata, beadmeta.SessionNameMetadataKey)
			return b
		}(), step: baseStep, ready: true},
		{name: "wrong root session", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			b.Metadata[beadmeta.SessionNameMetadataKey] = "other-session"
			return b
		}(), step: baseStep, ready: true},
		{name: "not graph v2 root", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			delete(b.Metadata, beadmeta.FormulaContractMetadataKey)
			return b
		}(), step: baseStep, ready: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := continuationCandidateFixture(t, cityName, actualStoreRef, tt.root, tt.step, tt.ready)
			if len(got) != tt.want {
				t.Fatalf("candidate count = %d, want %d: %#v", len(got), tt.want, got)
			}
			if tt.want == 1 {
				if got[0].WorkBeadID != baseStep.ID ||
					got[0].RootBeadID != baseRoot.ID ||
					got[0].StoreRef != canonicalRef ||
					got[0].Assignee != "session-a" {
					t.Fatalf("candidate = %#v, want exact work/root/store/assignee provenance", got[0])
				}
			}
		})
	}
}

func TestSelectReadyContinuationClaimCandidates_RequiresExactCityRef(t *testing.T) {
	root := continuationRoot("city:test-city")
	step := continuationStep(root.ID, "city:test-city")
	if got := continuationCandidateFixture(t, "test-city", "", root, step, true); len(got) != 1 {
		t.Fatalf("exact city candidate count = %d, want 1: %#v", len(got), got)
	}

	wrongRoot := continuationRoot("city:other-city")
	wrongStep := continuationStep(wrongRoot.ID, "city:other-city")
	if got := continuationCandidateFixture(t, "test-city", "", wrongRoot, wrongStep, true); len(got) != 0 {
		t.Fatalf("wrong-city candidate = %#v, want none", got)
	}
}

func TestSelectReadyContinuationClaimCandidates_RejectsMisalignedSnapshots(t *testing.T) {
	root := continuationRoot("rig:fixture")
	step := continuationStep(root.ID, "rig:fixture")
	backing := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	ready := map[storeScopedBeadKey]bool{{StoreRef: "fixture", ID: step.ID}: true}

	got := selectReadyContinuationClaimCandidates(
		"test-city",
		[]beads.Bead{step},
		[]beads.Store{backing},
		nil,
		ready,
	)
	if len(got) != 0 {
		t.Fatalf("misaligned snapshot candidates = %#v, want none", got)
	}
}

type continuationMetadataCountingStore struct {
	beads.Store
	metadataWrites int
}

func (s *continuationMetadataCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.metadataWrites++
	return s.Store.SetMetadataBatch(id, kvs)
}

func continuationRunningFake(t *testing.T, names ...string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	for _, name := range names {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("fake start %s: %v", name, err)
		}
	}
	return sp
}

func continuationNudgeCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:  "agent-a",
		Nudge: "Run gc hook --claim --drain-ack --json once and continue the assigned graph.",
	}}}
}

func validContinuationCandidate(id, assignee string) ContinuationClaimCandidate {
	return ContinuationClaimCandidate{
		WorkBeadID: id,
		RootBeadID: "root-a",
		StoreRef:   "rig:fixture",
		Assignee:   assignee,
	}
}

func TestNudgeStalledPoolContinuations_ObserveNudgePersistBackoffAndCap(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	cfg := continuationNudgeCfg()
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	candidates := []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)}
	var out bytes.Buffer

	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside grace", got)
	}
	if store.metadataWrites != 1 {
		t.Fatalf("first tick metadata writes = %d, want one persisted observation", store.metadataWrites)
	}

	session = mustGetTestBead(t, backing, session.ID)
	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("post-grace Nudge calls = %d, want 1", got)
	}

	// Reconstructing the predicate from the persisted session bead simulates a
	// controller restart. The attempt remains inside backoff and must not replay.
	session = mustGetTestBead(t, backing, session.ID)
	clk.Advance(time.Minute)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("restart-inside-backoff Nudge calls = %d, want 1", got)
	}

	for want := 2; want <= idleClaimNudgeMaxAttempts; want++ {
		session = mustGetTestBead(t, backing, session.ID)
		clk.Advance(idleClaimNudgeBackoff + time.Second)
		nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, clk.Now(), &out)
		if got := sp.CountCalls("Nudge", sessionName); got != want {
			t.Fatalf("attempt %d Nudge calls = %d, want %d", want, got, want)
		}
	}

	session = mustGetTestBead(t, backing, session.ID)
	writesAtCap := store.metadataWrites
	clk.Advance(time.Hour)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("past-cap Nudge calls = %d, want %d", got, idleClaimNudgeMaxAttempts)
	}
	if store.metadataWrites != writesAtCap {
		t.Fatalf("past-cap metadata writes = %d, want unchanged %d", store.metadataWrites, writesAtCap)
	}
}

func TestNudgeStalledPoolContinuations_ClaimClearsMarker(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	cfg := continuationNudgeCfg()
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer

	nudgeStalledPoolContinuations(
		sp, cfg, store, []beads.Bead{session},
		[]ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)},
		now, &out,
	)
	session = mustGetTestBead(t, backing, session.ID)
	// The next desired-state snapshot excludes the now-in_progress successor,
	// so the absence of an open candidate clears its exact persisted marker.
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, nil, now.Add(time.Second), &out)

	session = mustGetTestBead(t, backing, session.ID)
	for _, key := range []string{
		continuationClaimNudgeWorkKey,
		continuationClaimNudgeRootKey,
		continuationClaimNudgeStoreRefKey,
		continuationClaimNudgeGenerationKey,
		continuationClaimNudgeCountKey,
		continuationClaimNudgeAtKey,
	} {
		if got := session.Metadata[key]; got != "" {
			t.Fatalf("cleared metadata[%s] = %q, want empty", key, got)
		}
	}
}

func TestNudgeStalledPoolContinuations_RecycledGenerationRestartsGrace(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	cfg := continuationNudgeCfg()
	session := continuationPoolSession("session-bead-a", sessionName)
	session.Metadata["generation"] = "1"
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candidates := []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)}
	var out bytes.Buffer

	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, now, &out)
	if store.metadataWrites != 1 {
		t.Fatalf("generation 1 writes = %d, want one observation", store.metadataWrites)
	}
	if err := backing.SetMetadataBatch(session.ID, map[string]string{"generation": "2"}); err != nil {
		t.Fatalf("advance generation: %v", err)
	}
	session = mustGetTestBead(t, backing, session.ID)
	recycledAt := now.Add(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, recycledAt, &out)

	if got := sp.CountCalls("Nudge", sessionName); got != 0 {
		t.Fatalf("recycled generation Nudge calls = %d, want 0 during fresh grace", got)
	}
	if store.metadataWrites != 2 {
		t.Fatalf("recycled generation writes = %d, want fresh observation", store.metadataWrites)
	}
	session = mustGetTestBead(t, backing, session.ID)
	if got := session.Metadata[continuationClaimNudgeGenerationKey]; got != "2" {
		t.Fatalf("persisted generation = %q, want 2", got)
	}
	if got := session.Metadata[continuationClaimNudgeCountKey]; got != "0" {
		t.Fatalf("recycled attempt count = %q, want 0", got)
	}
	if got := session.Metadata[continuationClaimNudgeAtKey]; got != recycledAt.Format(time.RFC3339) {
		t.Fatalf("recycled grace start = %q, want %q", got, recycledAt.Format(time.RFC3339))
	}
}

func TestNudgeStalledPoolContinuations_DelayedScopeControlStartsGraceAtSuccessor(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	// The predecessor has closed, but the unassigned scope-control bead has not
	// yet produced a ready successor. This phase must be completely write-free.
	nudgeStalledPoolContinuations(
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, nil, clk.Now(), &out,
	)
	clk.Advance(10 * time.Minute)
	if store.metadataWrites != 0 {
		t.Fatalf("scope-control delay writes = %d, want 0 before successor", store.metadataWrites)
	}

	candidates := []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)}
	nudgeStalledPoolContinuations(
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, candidates, clk.Now(), &out,
	)
	if got := sp.CountCalls("Nudge", sessionName); got != 0 {
		t.Fatalf("successor appearance Nudge calls = %d, want 0 during grace", got)
	}
	if store.metadataWrites != 1 {
		t.Fatalf("successor appearance writes = %d, want one observation", store.metadataWrites)
	}

	session = mustGetTestBead(t, backing, session.ID)
	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolContinuations(
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, candidates, clk.Now(), &out,
	)
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("post-successor-grace Nudge calls = %d, want 1", got)
	}
}

func TestNudgeStalledPoolContinuations_NoCandidateDoesNotWrite(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}

	nudgeStalledPoolContinuations(
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, nil,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), &bytes.Buffer{},
	)
	if store.metadataWrites != 0 {
		t.Fatalf("metadata writes = %d, want 0 without a candidate or marker", store.metadataWrites)
	}
}

func TestNudgeStalledPoolContinuations_AcceptsCurrentSessionIdentities(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, assignee := range []string{"session-bead-a", sessionName, "named-a", "current-alias"} {
		t.Run(assignee, func(t *testing.T) {
			sp := continuationRunningFake(t, sessionName)
			session := continuationPoolSession("session-bead-a", sessionName)
			session.Metadata["configured_named_identity"] = "named-a"
			session.Metadata["alias_history"] = `["old-alias"]`
			backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			store := &continuationMetadataCountingStore{Store: backing}

			nudgeStalledPoolContinuations(
				sp,
				continuationNudgeCfg(),
				store,
				[]beads.Bead{session},
				[]ContinuationClaimCandidate{validContinuationCandidate("step-a", assignee)},
				now,
				&bytes.Buffer{},
			)
			if store.metadataWrites != 1 {
				t.Fatalf("metadata writes = %d, want one observation for current identity %q", store.metadataWrites, assignee)
			}
		})
	}
}

func TestNudgeStalledPoolContinuations_RejectsHistoricalAlias(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	session.Metadata["alias_history"] = `["old-alias"]`
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		store,
		[]beads.Bead{session},
		[]ContinuationClaimCandidate{validContinuationCandidate("step-a", "old-alias")},
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		&bytes.Buffer{},
	)
	if store.metadataWrites != 0 {
		t.Fatalf("metadata writes = %d, want 0 for historical alias", store.metadataWrites)
	}
}

func TestNudgeStalledPoolContinuations_FailsClosed(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		session    beads.Bead
		sessions   func(beads.Bead) []beads.Bead
		candidates []ContinuationClaimCandidate
		start      []string
	}{
		{
			name:       "wrong identity",
			session:    continuationPoolSession("session-bead-a", sessionName),
			candidates: []ContinuationClaimCandidate{validContinuationCandidate("step-a", "other-session")},
			start:      []string{sessionName},
		},
		{
			name:    "multiple candidates",
			session: continuationPoolSession("session-bead-a", sessionName),
			candidates: []ContinuationClaimCandidate{
				validContinuationCandidate("step-a", sessionName),
				validContinuationCandidate("step-b", sessionName),
			},
			start: []string{sessionName},
		},
		{
			name:    "same id in different stores is ambiguous",
			session: continuationPoolSession("session-bead-a", sessionName),
			candidates: []ContinuationClaimCandidate{
				validContinuationCandidate("step-a", sessionName),
				func() ContinuationClaimCandidate {
					c := validContinuationCandidate("step-a", sessionName)
					c.RootBeadID = "root-b"
					c.StoreRef = "rig:other"
					return c
				}(),
			},
			start: []string{sessionName},
		},
		{
			name:    "ambiguous current identity",
			session: continuationPoolSession("session-bead-a", sessionName),
			sessions: func(first beads.Bead) []beads.Bead {
				first.Metadata["alias"] = "shared"
				second := continuationPoolSession("session-bead-b", "session-b")
				second.Metadata["alias"] = "shared"
				return []beads.Bead{first, second}
			},
			candidates: []ContinuationClaimCandidate{validContinuationCandidate("step-a", "shared")},
			start:      []string{sessionName, "session-b"},
		},
		{
			name: "non pool",
			session: func() beads.Bead {
				s := continuationPoolSession("session-bead-a", sessionName)
				delete(s.Metadata, "pool_managed")
				return s
			}(),
			candidates: []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)},
			start:      []string{sessionName},
		},
		{
			name: "missing generation",
			session: func() beads.Bead {
				s := continuationPoolSession("session-bead-a", sessionName)
				delete(s.Metadata, "generation")
				return s
			}(),
			candidates: []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)},
			start:      []string{sessionName},
		},
		{
			name:       "stopped",
			session:    continuationPoolSession("session-bead-a", sessionName),
			candidates: []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := continuationRunningFake(t, tt.start...)
			sessions := []beads.Bead{tt.session}
			if tt.sessions != nil {
				sessions = tt.sessions(tt.session)
			}
			backing := beads.NewMemStoreFrom(0, sessions, nil)
			store := &continuationMetadataCountingStore{Store: backing}

			nudgeStalledPoolContinuations(
				sp, continuationNudgeCfg(), store, sessions, tt.candidates, now, &bytes.Buffer{},
			)
			if got := sp.CountCalls("Nudge", sessionName); got != 0 {
				t.Fatalf("Nudge calls = %d, want 0", got)
			}
			if store.metadataWrites != 0 {
				t.Fatalf("metadata writes = %d, want 0 for fail-closed case", store.metadataWrites)
			}
		})
	}
}
