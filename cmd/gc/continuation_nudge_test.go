package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/storeref"
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
) ([]ContinuationClaimCandidate, bool) {
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
		name        string
		root        beads.Bead
		step        beads.Bead
		ready       bool
		want        int
		wantPartial bool
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
		{name: "non canonical padded id", root: baseRoot, step: func() beads.Bead {
			b := baseStep
			b.ID = " " + baseStep.ID + " "
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
		}(), step: baseStep, ready: true, wantPartial: true},
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
		// The root's gc.session_name is a dashboard back-fill from the most
		// recent in_progress step, not an ownership record, so it never gates
		// candidacy. A multi-agent molecule's root can only ever name one of its
		// templates; gating on it stranded every cross-template successor.
		{name: "root session stamp absent", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			delete(b.Metadata, beadmeta.SessionNameMetadataKey)
			return b
		}(), step: baseStep, ready: true, want: 1},
		{name: "root session stamp names another template", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			b.Metadata[beadmeta.SessionNameMetadataKey] = "other-session"
			return b
		}(), step: baseStep, ready: true, want: 1},
		{name: "not graph v2 root", root: func() beads.Bead {
			b := baseRoot
			b.Metadata = cloneStringMap(baseRoot.Metadata)
			delete(b.Metadata, beadmeta.FormulaContractMetadataKey)
			return b
		}(), step: baseStep, ready: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, partial := continuationCandidateFixture(t, cityName, actualStoreRef, tt.root, tt.step, tt.ready)
			if partial != tt.wantPartial {
				t.Fatalf("candidate projection partial = %v, want %v", partial, tt.wantPartial)
			}
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
	got, partial := continuationCandidateFixture(t, "test-city", "", root, step, true)
	if partial || len(got) != 1 {
		t.Fatalf("exact city candidate count = %d, want 1: %#v", len(got), got)
	}

	wrongRoot := continuationRoot("city:other-city")
	wrongStep := continuationStep(wrongRoot.ID, "city:other-city")
	got, partial = continuationCandidateFixture(t, "test-city", "", wrongRoot, wrongStep, true)
	if partial || len(got) != 0 {
		t.Fatalf("wrong-city candidate = %#v, want none", got)
	}
}

func TestSelectReadyContinuationClaimCandidates_RejectsMisalignedSnapshots(t *testing.T) {
	root := continuationRoot("rig:fixture")
	step := continuationStep(root.ID, "rig:fixture")
	backing := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	ready := map[storeScopedBeadKey]bool{{StoreRef: "fixture", ID: step.ID}: true}

	got, partial := selectReadyContinuationClaimCandidates(
		"test-city",
		[]beads.Bead{step},
		[]beads.Store{backing},
		nil,
		ready,
	)
	if len(got) != 0 || !partial {
		t.Fatalf("misaligned snapshot = {%#v partial:%v}, want no candidates and partial", got, partial)
	}

	got, partial = selectReadyContinuationClaimCandidates(
		"test-city",
		nil,
		[]beads.Store{backing},
		nil,
		ready,
	)
	if len(got) != 0 || !partial {
		t.Fatalf("empty-work misalignment = {%#v partial:%v}, want no candidates and partial", got, partial)
	}
}

func TestSelectReadyContinuationClaimCandidates_RootReadFailureIsPartial(t *testing.T) {
	const (
		cityName       = "test-city"
		actualStoreRef = "fixture"
		canonicalRef   = "rig:fixture"
	)
	root := continuationRoot(canonicalRef)
	step := continuationStep(root.ID, canonicalRef)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	unreadable := &continuationGetErrorStore{Store: backing, failID: root.ID}
	ready := map[storeScopedBeadKey]bool{{StoreRef: actualStoreRef, ID: step.ID}: true}

	got, partial := selectReadyContinuationClaimCandidates(
		cityName,
		[]beads.Bead{step},
		[]beads.Store{unreadable},
		[]string{actualStoreRef},
		ready,
	)
	if len(got) != 0 || !partial {
		t.Fatalf("root read failure = {%#v partial:%v}, want no candidate and partial", got, partial)
	}
}

func TestSelectReadyContinuationClaimCandidates_DuplicateAgreementRequired(t *testing.T) {
	const (
		cityName       = "test-city"
		actualStoreRef = "fixture"
		canonicalRef   = "rig:fixture"
	)
	root := continuationRoot(canonicalRef)
	step := continuationStep(root.ID, canonicalRef)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	ready := map[storeScopedBeadKey]bool{{StoreRef: actualStoreRef, ID: step.ID}: true}

	t.Run("identical duplicate deduplicates", func(t *testing.T) {
		got, partial := selectReadyContinuationClaimCandidates(
			cityName,
			[]beads.Bead{step, step},
			[]beads.Store{backing, backing},
			[]string{actualStoreRef, actualStoreRef},
			ready,
		)
		if partial || len(got) != 1 {
			t.Fatalf("identical duplicate = {%#v partial:%v}, want one exact candidate", got, partial)
		}
	})

	t.Run("valid and ineligible copies hold snapshot", func(t *testing.T) {
		ineligible := step
		ineligible.Metadata = cloneStringMap(step.Metadata)
		delete(ineligible.Metadata, beadmeta.ContinuationGroupMetadataKey)
		got, partial := selectReadyContinuationClaimCandidates(
			cityName,
			[]beads.Bead{step, ineligible},
			[]beads.Store{backing, backing},
			[]string{actualStoreRef, actualStoreRef},
			ready,
		)
		if len(got) != 0 || !partial {
			t.Fatalf("disagreeing duplicate = {%#v partial:%v}, want no candidate and partial", got, partial)
		}
	})

	t.Run("divergent valid copies hold snapshot", func(t *testing.T) {
		otherRoot := continuationRoot(canonicalRef)
		otherRoot.ID = "root-b"
		otherRoot.Metadata[beadmeta.SessionNameMetadataKey] = "session-b"
		otherStep := continuationStep(otherRoot.ID, canonicalRef)
		otherStep.ID = step.ID
		otherStep.Assignee = "session-b"
		divergentStore := beads.NewMemStoreFrom(0, []beads.Bead{root, otherRoot}, nil)
		got, partial := selectReadyContinuationClaimCandidates(
			cityName,
			[]beads.Bead{step, otherStep},
			[]beads.Store{divergentStore, divergentStore},
			[]string{actualStoreRef, actualStoreRef},
			ready,
		)
		if len(got) != 0 || !partial {
			t.Fatalf("divergent valid duplicate = {%#v partial:%v}, want no candidate and partial", got, partial)
		}
	})

	t.Run("valid and unreadable root copies hold snapshot", func(t *testing.T) {
		unreadable := &continuationGetErrorStore{Store: backing, failID: root.ID}
		got, partial := selectReadyContinuationClaimCandidates(
			cityName,
			[]beads.Bead{step, step},
			[]beads.Store{backing, unreadable},
			[]string{actualStoreRef, actualStoreRef},
			ready,
		)
		if len(got) != 0 || !partial {
			t.Fatalf("unreadable duplicate = {%#v partial:%v}, want no candidate and partial", got, partial)
		}
	})
}

// A class binding is a city-scope STORE but a many-scope CONTAINER: the same
// doctrine PR #5918 (ga-oytw9) established for control-route repair. The leg
// canonicalizes to the city because that is the scope it serves, but the rows
// inside it keep their own owner — a rig-scoped workflow's steps are minted
// with gc.root_store_ref=rig:<name>. Comparing such a row against the
// leg-derived city ref answers "absent" for every rig-rooted successor on a
// split city, which silently retires the continuation backstop for that whole
// class of rows (ga-erfca).
func TestEvaluateReadyContinuationClaimCandidate_BindingLegTakesOwnerFromRootRef(t *testing.T) {
	const cityName = "split-city"
	classLeg := string(storeref.ClassRef(wholeSplitClasses()))

	tests := []struct {
		name           string
		rawStoreRef    string
		stepOwnerRef   string
		rootOwnerRef   string
		wantOwnerRef   string
		wantResolution continuationCandidateResolution
	}{
		{
			name:           "binding leg rig rooted row",
			rawStoreRef:    classLeg,
			stepOwnerRef:   "rig:fixture",
			rootOwnerRef:   "rig:fixture",
			wantOwnerRef:   "rig:fixture",
			wantResolution: continuationCandidateValid,
		},
		{
			name:           "binding leg city rooted row",
			rawStoreRef:    classLeg,
			stepOwnerRef:   "city:" + cityName,
			rootOwnerRef:   "city:" + cityName,
			wantOwnerRef:   "city:" + cityName,
			wantResolution: continuationCandidateValid,
		},
		{
			name:           "legacy rig leg rig rooted row",
			rawStoreRef:    "fixture",
			stepOwnerRef:   "rig:fixture",
			rootOwnerRef:   "rig:fixture",
			wantOwnerRef:   "rig:fixture",
			wantResolution: continuationCandidateValid,
		},
		{
			name:           "binding leg row owned by the leg itself",
			rawStoreRef:    classLeg,
			stepOwnerRef:   classLeg,
			rootOwnerRef:   classLeg,
			wantResolution: continuationCandidateAbsent,
		},
		{
			name:           "binding leg row owned by another city",
			rawStoreRef:    classLeg,
			stepOwnerRef:   "city:other-city",
			rootOwnerRef:   "city:other-city",
			wantResolution: continuationCandidateAbsent,
		},
		{
			name:           "binding leg row with a malformed rig owner",
			rawStoreRef:    classLeg,
			stepOwnerRef:   "rig:",
			rootOwnerRef:   "rig:",
			wantResolution: continuationCandidateAbsent,
		},
		{
			name:           "binding leg row with a non canonical owner",
			rawStoreRef:    classLeg,
			stepOwnerRef:   "fixture",
			rootOwnerRef:   "fixture",
			wantResolution: continuationCandidateAbsent,
		},
		{
			name:           "binding leg root disagrees with the step owner",
			rawStoreRef:    classLeg,
			stepOwnerRef:   "rig:fixture",
			rootOwnerRef:   "city:" + cityName,
			wantResolution: continuationCandidateAbsent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canonicalStoreRef, ok := canonicalContinuationClaimStoreRef(cityName, tt.rawStoreRef)
			if !ok {
				t.Fatalf("leg %q did not canonicalize", tt.rawStoreRef)
			}
			root := continuationRoot(tt.rootOwnerRef)
			step := continuationStep(root.ID, tt.stepOwnerRef)
			binding := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
			readyAssigned := map[storeScopedBeadKey]bool{{StoreRef: tt.rawStoreRef, ID: step.ID}: true}

			got, resolution := evaluateReadyContinuationClaimCandidate(
				step,
				binding,
				tt.rawStoreRef,
				canonicalStoreRef,
				readyAssigned,
			)
			if resolution != tt.wantResolution {
				t.Fatalf("resolution = %v, want %v (candidate %#v)", resolution, tt.wantResolution, got)
			}
			if tt.wantResolution != continuationCandidateValid {
				return
			}
			if got.StoreRef != tt.wantOwnerRef {
				t.Fatalf("candidate store ref = %q, want the row's own owner %q", got.StoreRef, tt.wantOwnerRef)
			}
			if got.WorkBeadID != step.ID || got.RootBeadID != root.ID || got.Assignee != step.Assignee {
				t.Fatalf("candidate = %#v, want exact work/root/assignee provenance", got)
			}
			if got.Store != binding {
				t.Fatalf("candidate store = %#v, want the binding leg the row was read from", got.Store)
			}
		})
	}
}

// The selector must hand the backstop a rig-rooted binding row with its own
// owner ref, not the city ref the binding leg groups under.
func TestSelectReadyContinuationClaimCandidates_BindingRigRootedRowIsACandidate(t *testing.T) {
	classLeg := string(storeref.ClassRef(wholeSplitClasses()))
	root := continuationRoot("rig:fixture")
	step := continuationStep(root.ID, "rig:fixture")

	got, partial := continuationCandidateFixture(t, "split-city", classLeg, root, step, true)
	if partial || len(got) != 1 {
		t.Fatalf("binding rig-rooted candidates = {%#v partial:%v}, want exactly one candidate", got, partial)
	}
	if got[0].StoreRef != "rig:fixture" ||
		got[0].WorkBeadID != step.ID ||
		got[0].RootBeadID != root.ID ||
		got[0].Assignee != step.Assignee {
		t.Fatalf("candidate = %#v, want the row's own rig owner and exact provenance", got[0])
	}
}

// End to end through the backstop: a rig-rooted successor living in a class
// binding must still be re-delivered its claim nudge, and the durable marker
// must record the owner ref the revalidation reads back.
func TestNudgeStalledPoolContinuations_BindingRigRootedCandidateIsNudged(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	classLeg := string(storeref.ClassRef(wholeSplitClasses()))

	root, step := continuationCandidateBeads("step-a", sessionName)
	binding := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	candidates, partial := selectReadyContinuationClaimCandidates(
		"split-city",
		[]beads.Bead{step},
		[]beads.Store{binding},
		[]string{classLeg},
		map[storeScopedBeadKey]bool{{StoreRef: classLeg, ID: step.ID}: true},
	)
	if partial || len(candidates) != 1 {
		t.Fatalf("binding candidates = {%#v partial:%v}, want exactly one candidate", candidates, partial)
	}

	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	seedContinuationMarker(t, backing, session, candidates[0], 0, now.Add(-idleClaimNudgeGrace-time.Second))
	session = mustGetTestBead(t, backing, session.ID)

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		&continuationMetadataCountingStore{Store: backing},
		[]beads.Bead{session},
		candidates,
		false,
		now,
		&bytes.Buffer{},
	)
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 for a rig-rooted row inside the binding", got)
	}
	session = mustGetTestBead(t, backing, session.ID)
	if got := session.Metadata[continuationClaimNudgeStoreRefKey]; got != "rig:fixture" {
		t.Fatalf("marker store ref = %q, want the row's own owner rig:fixture", got)
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

type continuationMetadataCallbackStore struct {
	beads.Store
	beforeMetadataWrite func()
}

func (s *continuationMetadataCallbackStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.beforeMetadataWrite != nil {
		s.beforeMetadataWrite()
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

type continuationFailingMetadataStore struct {
	beads.Store
	metadataWrites int
}

func (s *continuationFailingMetadataStore) SetMetadataBatch(string, map[string]string) error {
	s.metadataWrites++
	return errors.New("injected metadata write failure")
}

type continuationGetErrorStore struct {
	beads.Store
	failID string
}

func (s *continuationGetErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("injected get failure")
	}
	return s.Store.Get(id)
}

type continuationFailingNudgeProvider struct {
	runtime.Provider
	nudgeCalls int
}

func (p *continuationFailingNudgeProvider) Nudge(string, []runtime.ContentBlock) error {
	p.nudgeCalls++
	return errors.New("injected delivery failure")
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

func TestPoolContinuationBackstopContent_SkipsBlankNudge(t *testing.T) {
	cfg := continuationNudgeCfg()
	cfg.Agents[0].Nudge = " \t "

	if got := (poolContinuationBackstop{cfg: cfg}).content(continuationPoolSession("session-bead-a", "session-a")); got != "" {
		t.Fatalf("continuation claim nudge = %q, want empty", got)
	}
}

func continuationCandidateBeads(id, assignee string) (beads.Bead, beads.Bead) {
	root := continuationRoot("rig:fixture")
	root.Metadata[beadmeta.SessionNameMetadataKey] = assignee
	step := continuationStep(root.ID, "rig:fixture")
	step.ID = id
	step.Assignee = assignee
	return root, step
}

func validContinuationCandidate(id, assignee string) ContinuationClaimCandidate {
	root, step := continuationCandidateBeads(id, assignee)
	workStore := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
	return ContinuationClaimCandidate{
		WorkBeadID: id,
		RootBeadID: root.ID,
		StoreRef:   "rig:fixture",
		Assignee:   assignee,
		Store:      workStore,
	}
}

func seedContinuationMarker(
	t *testing.T,
	store beads.Store,
	session beads.Bead,
	candidate ContinuationClaimCandidate,
	attempts int,
	at time.Time,
) {
	t.Helper()
	target := backstopTarget{
		ID:         candidate.WorkBeadID,
		RootID:     candidate.RootBeadID,
		StoreRef:   candidate.StoreRef,
		Generation: "1",
		Assignee:   candidate.Assignee,
		Store:      candidate.Store,
	}
	if !writeContinuationClaimMarker(store, &session, target, attempts, at, &bytes.Buffer{}) {
		t.Fatal("seed continuation marker failed")
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

	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside grace", got)
	}
	if store.metadataWrites != 1 {
		t.Fatalf("first tick metadata writes = %d, want one persisted observation", store.metadataWrites)
	}

	session = mustGetTestBead(t, backing, session.ID)
	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("post-grace Nudge calls = %d, want 1", got)
	}

	// Reconstructing the predicate from the persisted session bead simulates a
	// controller restart. The attempt remains inside backoff and must not replay.
	session = mustGetTestBead(t, backing, session.ID)
	clk.Advance(time.Minute)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("restart-inside-backoff Nudge calls = %d, want 1", got)
	}

	for want := 2; want <= idleClaimNudgeMaxAttempts; want++ {
		session = mustGetTestBead(t, backing, session.ID)
		clk.Advance(idleClaimNudgeBackoff + time.Second)
		nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, clk.Now(), &out)
		if got := sp.CountCalls("Nudge", sessionName); got != want {
			t.Fatalf("attempt %d Nudge calls = %d, want %d", want, got, want)
		}
	}

	session = mustGetTestBead(t, backing, session.ID)
	writesAtCap := store.metadataWrites
	clk.Advance(time.Hour)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", sessionName); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("past-cap Nudge calls = %d, want %d", got, idleClaimNudgeMaxAttempts)
	}
	if store.metadataWrites != writesAtCap {
		t.Fatalf("past-cap metadata writes = %d, want unchanged %d", store.metadataWrites, writesAtCap)
	}
}

func TestNudgeStalledPoolContinuations_WriteAheadFailurePreventsDelivery(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	candidate := validContinuationCandidate("step-a", sessionName)
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	observedAt := now.Add(-idleClaimNudgeGrace - time.Second)
	seedContinuationMarker(t, backing, session, candidate, 0, observedAt)
	session = mustGetTestBead(t, backing, session.ID)
	store := &continuationFailingMetadataStore{Store: backing}

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		store,
		[]beads.Bead{session},
		[]ContinuationClaimCandidate{candidate},
		false,
		now,
		&bytes.Buffer{},
	)

	if got := sp.CountCalls("Nudge", sessionName); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 when write-ahead reservation fails", got)
	}
	if store.metadataWrites != 1 {
		t.Fatalf("reservation writes = %d, want 1 failed attempt", store.metadataWrites)
	}
	session = mustGetTestBead(t, backing, session.ID)
	if got := session.Metadata[continuationClaimNudgeCountKey]; got != "0" {
		t.Fatalf("persisted attempt count = %q, want unchanged 0", got)
	}
	if got := session.Metadata[continuationClaimNudgeAtKey]; got != observedAt.Format(time.RFC3339) {
		t.Fatalf("persisted attempt time = %q, want unchanged %q", got, observedAt.Format(time.RFC3339))
	}
}

func TestNudgeStalledPoolContinuations_ReservesBeforeSuccessfulDelivery(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	candidate := validContinuationCandidate("step-a", sessionName)
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	seedContinuationMarker(t, backing, session, candidate, 0, now.Add(-idleClaimNudgeGrace-time.Second))
	session = mustGetTestBead(t, backing, session.ID)
	reservationObserved := 0
	store := &continuationMetadataCallbackStore{
		Store: backing,
		beforeMetadataWrite: func() {
			reservationObserved++
			if got := sp.CountCalls("Nudge", sessionName); got != 0 {
				t.Fatalf("Nudge calls during reservation = %d, want 0", got)
			}
		},
	}

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		store,
		[]beads.Bead{session},
		[]ContinuationClaimCandidate{candidate},
		false,
		now,
		&bytes.Buffer{},
	)
	if reservationObserved != 1 {
		t.Fatalf("reservation callbacks = %d, want 1", reservationObserved)
	}
	if got := sp.CountCalls("Nudge", sessionName); got != 1 {
		t.Fatalf("Nudge calls after reservation = %d, want 1", got)
	}
	session = mustGetTestBead(t, backing, session.ID)
	if got := session.Metadata[continuationClaimNudgeCountKey]; got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1", got)
	}
}

func TestNudgeStalledPoolContinuations_DeliveryFailureConsumesAttempt(t *testing.T) {
	const sessionName = "session-a"
	fake := continuationRunningFake(t, sessionName)
	sp := &continuationFailingNudgeProvider{Provider: fake}
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	store := &continuationMetadataCountingStore{Store: backing}
	candidate := validContinuationCandidate("step-a", sessionName)
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	seedContinuationMarker(t, backing, session, candidate, 0, now.Add(-idleClaimNudgeGrace-time.Second))
	session = mustGetTestBead(t, backing, session.ID)

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		store,
		[]beads.Bead{session},
		[]ContinuationClaimCandidate{candidate},
		false,
		now,
		&bytes.Buffer{},
	)
	if sp.nudgeCalls != 1 {
		t.Fatalf("delivery calls = %d, want 1 failed attempt", sp.nudgeCalls)
	}
	if store.metadataWrites != 1 {
		t.Fatalf("metadata writes = %d, want write-ahead reservation", store.metadataWrites)
	}
	session = mustGetTestBead(t, backing, session.ID)
	if got := session.Metadata[continuationClaimNudgeCountKey]; got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1 despite delivery failure", got)
	}

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		store,
		[]beads.Bead{session},
		[]ContinuationClaimCandidate{candidate},
		false,
		now.Add(time.Second),
		&bytes.Buffer{},
	)
	if sp.nudgeCalls != 1 || store.metadataWrites != 1 {
		t.Fatalf("inside backoff = {delivery:%d writes:%d}, want unchanged {1 1}", sp.nudgeCalls, store.metadataWrites)
	}
}

func TestNudgeStalledPoolContinuations_PartialSnapshotPreservesMarker(t *testing.T) {
	const sessionName = "session-a"
	sp := continuationRunningFake(t, sessionName)
	session := continuationPoolSession("session-bead-a", sessionName)
	backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	candidate := validContinuationCandidate("step-a", sessionName)
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	seedContinuationMarker(t, backing, session, candidate, 2, now.Add(-time.Hour))
	session = mustGetTestBead(t, backing, session.ID)
	store := &continuationMetadataCountingStore{Store: backing}

	nudgeStalledPoolContinuations(
		sp,
		continuationNudgeCfg(),
		store,
		[]beads.Bead{session},
		nil,
		true,
		now,
		&bytes.Buffer{},
	)
	if store.metadataWrites != 0 {
		t.Fatalf("metadata writes = %d, want 0 for partial snapshot hold", store.metadataWrites)
	}
	session = mustGetTestBead(t, backing, session.ID)
	if got := session.Metadata[continuationClaimNudgeCountKey]; got != "2" {
		t.Fatalf("persisted attempt count = %q, want preserved 2", got)
	}
}

func TestNudgeStalledPoolContinuations_AmbiguityPreservesMarker(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	t.Run("multiple candidates", func(t *testing.T) {
		sp := continuationRunningFake(t, sessionName)
		session := continuationPoolSession("session-bead-a", sessionName)
		first := validContinuationCandidate("step-a", sessionName)
		second := validContinuationCandidate("step-b", sessionName)
		backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
		seedContinuationMarker(t, backing, session, first, 2, now.Add(-time.Hour))
		session = mustGetTestBead(t, backing, session.ID)
		store := &continuationMetadataCountingStore{Store: backing}

		nudgeStalledPoolContinuations(
			sp,
			continuationNudgeCfg(),
			store,
			[]beads.Bead{session},
			[]ContinuationClaimCandidate{first, second},
			false,
			now,
			&bytes.Buffer{},
		)
		if store.metadataWrites != 0 {
			t.Fatalf("metadata writes = %d, want 0 while candidate set is ambiguous", store.metadataWrites)
		}
		session = mustGetTestBead(t, backing, session.ID)
		if got := session.Metadata[continuationClaimNudgeWorkKey]; got != first.WorkBeadID {
			t.Fatalf("persisted work marker = %q, want preserved %q", got, first.WorkBeadID)
		}
	})

	t.Run("shared current identity", func(t *testing.T) {
		firstSession := continuationPoolSession("session-bead-a", sessionName)
		firstSession.Metadata["alias"] = "shared"
		secondSession := continuationPoolSession("session-bead-b", "session-b")
		secondSession.Metadata["alias"] = "shared"
		sp := continuationRunningFake(t, sessionName, "session-b")
		candidate := validContinuationCandidate("step-a", "shared")
		backing := beads.NewMemStoreFrom(0, []beads.Bead{firstSession, secondSession}, nil)
		seedContinuationMarker(t, backing, firstSession, candidate, 2, now.Add(-time.Hour))
		firstSession = mustGetTestBead(t, backing, firstSession.ID)
		secondSession = mustGetTestBead(t, backing, secondSession.ID)
		store := &continuationMetadataCountingStore{Store: backing}

		nudgeStalledPoolContinuations(
			sp,
			continuationNudgeCfg(),
			store,
			[]beads.Bead{firstSession, secondSession},
			[]ContinuationClaimCandidate{candidate},
			false,
			now,
			&bytes.Buffer{},
		)
		if store.metadataWrites != 0 {
			t.Fatalf("metadata writes = %d, want 0 while identity ownership is ambiguous", store.metadataWrites)
		}
		firstSession = mustGetTestBead(t, backing, firstSession.ID)
		if got := firstSession.Metadata[continuationClaimNudgeWorkKey]; got != candidate.WorkBeadID {
			t.Fatalf("persisted work marker = %q, want preserved %q", got, candidate.WorkBeadID)
		}
	})

	t.Run("missing generation", func(t *testing.T) {
		sp := continuationRunningFake(t, sessionName)
		session := continuationPoolSession("session-bead-a", sessionName)
		delete(session.Metadata, "generation")
		candidate := validContinuationCandidate("step-a", sessionName)
		backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
		seedContinuationMarker(t, backing, session, candidate, 2, now.Add(-time.Hour))
		session = mustGetTestBead(t, backing, session.ID)
		store := &continuationMetadataCountingStore{Store: backing}

		nudgeStalledPoolContinuations(
			sp,
			continuationNudgeCfg(),
			store,
			[]beads.Bead{session},
			[]ContinuationClaimCandidate{candidate},
			false,
			now,
			&bytes.Buffer{},
		)
		if store.metadataWrites != 0 {
			t.Fatalf("metadata writes = %d, want 0 without exact generation", store.metadataWrites)
		}
		session = mustGetTestBead(t, backing, session.ID)
		if got := session.Metadata[continuationClaimNudgeCountKey]; got != "2" {
			t.Fatalf("persisted attempt count = %q, want preserved 2", got)
		}
	})
}

func TestNudgeStalledPoolContinuations_RevalidatesImmediatelyBeforeDelivery(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	for _, tt := range []struct {
		name     string
		mutateID string
		status   string
	}{
		{name: "successor already claimed", mutateID: "step-a", status: "in_progress"},
		{name: "root already closed", mutateID: "root-a", status: "closed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := continuationRunningFake(t, sessionName)
			session := continuationPoolSession("session-bead-a", sessionName)
			candidate := validContinuationCandidate("step-a", sessionName)
			backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			seedContinuationMarker(t, backing, session, candidate, 0, now.Add(-idleClaimNudgeGrace-time.Second))
			status := tt.status
			if err := candidate.Store.Update(tt.mutateID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("mutate revalidation target: %v", err)
			}
			session = mustGetTestBead(t, backing, session.ID)
			store := &continuationMetadataCountingStore{Store: backing}

			nudgeStalledPoolContinuations(
				sp,
				continuationNudgeCfg(),
				store,
				[]beads.Bead{session},
				[]ContinuationClaimCandidate{candidate},
				false,
				now,
				&bytes.Buffer{},
			)
			if got := sp.CountCalls("Nudge", sessionName); got != 0 {
				t.Fatalf("Nudge calls = %d, want 0 after live target transition", got)
			}
			if store.metadataWrites != 1 {
				t.Fatalf("metadata writes = %d, want one marker clear", store.metadataWrites)
			}
			session = mustGetTestBead(t, backing, session.ID)
			if got := session.Metadata[continuationClaimNudgeWorkKey]; got != "" {
				t.Fatalf("work marker = %q, want cleared after definite transition", got)
			}
		})
	}

	t.Run("root read failure holds marker", func(t *testing.T) {
		sp := continuationRunningFake(t, sessionName)
		session := continuationPoolSession("session-bead-a", sessionName)
		candidate := validContinuationCandidate("step-a", sessionName)
		candidate.Store = &continuationGetErrorStore{Store: candidate.Store, failID: candidate.RootBeadID}
		backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
		seedContinuationMarker(t, backing, session, candidate, 1, now.Add(-idleClaimNudgeBackoff-time.Second))
		session = mustGetTestBead(t, backing, session.ID)
		store := &continuationMetadataCountingStore{Store: backing}

		nudgeStalledPoolContinuations(
			sp,
			continuationNudgeCfg(),
			store,
			[]beads.Bead{session},
			[]ContinuationClaimCandidate{candidate},
			false,
			now,
			&bytes.Buffer{},
		)
		if got := sp.CountCalls("Nudge", sessionName); got != 0 {
			t.Fatalf("Nudge calls = %d, want 0 on root read failure", got)
		}
		if store.metadataWrites != 0 {
			t.Fatalf("metadata writes = %d, want 0 while revalidation is incomplete", store.metadataWrites)
		}
		session = mustGetTestBead(t, backing, session.ID)
		if got := session.Metadata[continuationClaimNudgeCountKey]; got != "1" {
			t.Fatalf("persisted attempt count = %q, want preserved 1", got)
		}
	})
}

func TestNudgeStalledPoolContinuations_RevalidationIgnoresRootSessionStamp(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	for _, tt := range []struct {
		name  string
		stamp string // "" = key deleted
	}{
		{name: "root session stamp absent"},
		{name: "root session stamp names another template", stamp: "other-session"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, step := continuationCandidateBeads("step-a", sessionName)
			if tt.stamp == "" {
				delete(root.Metadata, beadmeta.SessionNameMetadataKey)
			} else {
				root.Metadata[beadmeta.SessionNameMetadataKey] = tt.stamp
			}
			candidate := ContinuationClaimCandidate{
				WorkBeadID: step.ID,
				RootBeadID: root.ID,
				StoreRef:   "rig:fixture",
				Assignee:   sessionName,
				Store:      beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil),
			}

			sp := continuationRunningFake(t, sessionName)
			session := continuationPoolSession("session-bead-a", sessionName)
			backing := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			seedContinuationMarker(t, backing, session, candidate, 0, now.Add(-idleClaimNudgeGrace-time.Second))
			session = mustGetTestBead(t, backing, session.ID)

			nudgeStalledPoolContinuations(
				sp,
				continuationNudgeCfg(),
				&continuationMetadataCountingStore{Store: backing},
				[]beads.Bead{session},
				[]ContinuationClaimCandidate{candidate},
				false,
				now,
				&bytes.Buffer{},
			)
			if got := sp.CountCalls("Nudge", sessionName); got != 1 {
				t.Fatalf("Nudge calls = %d, want 1 — the root stamp must not gate delivery", got)
			}
		})
	}
}

func TestNudgeStalledPoolContinuations_RevalidationBypassesPrimedCache(t *testing.T) {
	const sessionName = "session-a"
	now := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	for _, tt := range []struct {
		name       string
		mutateID   string
		liveStatus string
	}{
		{name: "successor claimed outside cache", mutateID: "step-a", liveStatus: "in_progress"},
		{name: "root closed outside cache", mutateID: "root-a", liveStatus: "closed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, step := continuationCandidateBeads("step-a", sessionName)
			workBacking := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
			cache := beads.NewCachingStoreForTest(workBacking, nil)
			if err := cache.PrimeActive(); err != nil {
				t.Fatalf("prime work cache: %v", err)
			}
			candidate := ContinuationClaimCandidate{
				WorkBeadID: step.ID,
				RootBeadID: root.ID,
				StoreRef:   "rig:fixture",
				Assignee:   sessionName,
				Store:      cache,
			}

			cachedBefore, err := cache.Get(tt.mutateID)
			if err != nil {
				t.Fatalf("cached Get before external transition: %v", err)
			}
			status := tt.liveStatus
			if err := workBacking.Update(tt.mutateID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("mutate live backing: %v", err)
			}
			cachedAfter, err := cache.Get(tt.mutateID)
			if err != nil {
				t.Fatalf("cached Get after external transition: %v", err)
			}
			if cachedAfter.Status != cachedBefore.Status {
				t.Fatalf("cache unexpectedly refreshed status = %q, want stale %q", cachedAfter.Status, cachedBefore.Status)
			}
			liveAfter, err := beads.HandlesFor(cache).Live.Get(tt.mutateID)
			if err != nil {
				t.Fatalf("live Get after external transition: %v", err)
			}
			if liveAfter.Status != tt.liveStatus {
				t.Fatalf("live status = %q, want %q", liveAfter.Status, tt.liveStatus)
			}

			sp := continuationRunningFake(t, sessionName)
			session := continuationPoolSession("session-bead-a", sessionName)
			sessionBacking := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			seedContinuationMarker(t, sessionBacking, session, candidate, 0, now.Add(-idleClaimNudgeGrace-time.Second))
			session = mustGetTestBead(t, sessionBacking, session.ID)
			sessionStore := &continuationMetadataCountingStore{Store: sessionBacking}

			nudgeStalledPoolContinuations(
				sp,
				continuationNudgeCfg(),
				sessionStore,
				[]beads.Bead{session},
				[]ContinuationClaimCandidate{candidate},
				false,
				now,
				&bytes.Buffer{},
			)
			if got := sp.CountCalls("Nudge", sessionName); got != 0 {
				t.Fatalf("Nudge calls = %d, want 0 after authoritative live transition", got)
			}
			if sessionStore.metadataWrites != 1 {
				t.Fatalf("metadata writes = %d, want one stale-marker clear", sessionStore.metadataWrites)
			}
			session = mustGetTestBead(t, sessionBacking, session.ID)
			if got := session.Metadata[continuationClaimNudgeWorkKey]; got != "" {
				t.Fatalf("work marker = %q, want cleared after authoritative live transition", got)
			}
		})
	}

	t.Run("live root read error holds stale marker", func(t *testing.T) {
		root, step := continuationCandidateBeads("step-a", sessionName)
		workBacking := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
		failingBacking := &continuationGetErrorStore{Store: workBacking}
		cache := beads.NewCachingStoreForTest(failingBacking, nil)
		if err := cache.PrimeActive(); err != nil {
			t.Fatalf("prime work cache: %v", err)
		}
		if _, err := cache.Get(root.ID); err != nil {
			t.Fatalf("prime cached root read: %v", err)
		}
		failingBacking.failID = root.ID
		if _, err := cache.Get(root.ID); err != nil {
			t.Fatalf("plain cached root Get unexpectedly reached live failure: %v", err)
		}
		candidate := ContinuationClaimCandidate{
			WorkBeadID: step.ID,
			RootBeadID: root.ID,
			StoreRef:   "rig:fixture",
			Assignee:   sessionName,
			Store:      cache,
		}

		sp := continuationRunningFake(t, sessionName)
		session := continuationPoolSession("session-bead-a", sessionName)
		sessionBacking := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
		seedContinuationMarker(t, sessionBacking, session, candidate, 1, now.Add(-idleClaimNudgeBackoff-time.Second))
		session = mustGetTestBead(t, sessionBacking, session.ID)
		sessionStore := &continuationMetadataCountingStore{Store: sessionBacking}

		nudgeStalledPoolContinuations(
			sp,
			continuationNudgeCfg(),
			sessionStore,
			[]beads.Bead{session},
			[]ContinuationClaimCandidate{candidate},
			false,
			now,
			&bytes.Buffer{},
		)
		if got := sp.CountCalls("Nudge", sessionName); got != 0 {
			t.Fatalf("Nudge calls = %d, want 0 on authoritative root read failure", got)
		}
		if sessionStore.metadataWrites != 0 {
			t.Fatalf("metadata writes = %d, want 0 while authoritative root read is incomplete", sessionStore.metadataWrites)
		}
		session = mustGetTestBead(t, sessionBacking, session.ID)
		if got := session.Metadata[continuationClaimNudgeCountKey]; got != "1" {
			t.Fatalf("persisted attempt count = %q, want preserved 1", got)
		}
	})
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
		false, now, &out,
	)
	session = mustGetTestBead(t, backing, session.ID)
	// The next desired-state snapshot excludes the now-in_progress successor,
	// so the absence of an open candidate clears its exact persisted marker.
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, nil, false, now.Add(time.Second), &out)

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

	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, now, &out)
	if store.metadataWrites != 1 {
		t.Fatalf("generation 1 writes = %d, want one observation", store.metadataWrites)
	}
	if err := backing.SetMetadataBatch(session.ID, map[string]string{"generation": "2"}); err != nil {
		t.Fatalf("advance generation: %v", err)
	}
	session = mustGetTestBead(t, backing, session.ID)
	recycledAt := now.Add(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolContinuations(sp, cfg, store, []beads.Bead{session}, candidates, false, recycledAt, &out)

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
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, nil, false, clk.Now(), &out,
	)
	clk.Advance(10 * time.Minute)
	if store.metadataWrites != 0 {
		t.Fatalf("scope-control delay writes = %d, want 0 before successor", store.metadataWrites)
	}

	candidates := []ContinuationClaimCandidate{validContinuationCandidate("step-a", sessionName)}
	nudgeStalledPoolContinuations(
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, candidates, false, clk.Now(), &out,
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
		sp, continuationNudgeCfg(), store, []beads.Bead{session}, candidates, false, clk.Now(), &out,
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
		false, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), &bytes.Buffer{},
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
				false,
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
		false,
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
				sp, continuationNudgeCfg(), store, sessions, tt.candidates, false, now, &bytes.Buffer{},
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
