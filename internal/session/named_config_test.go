package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestNamedSessionContinuityEligible_ArchivedRequiresExplicitContinuity(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{
			name: "archived explicit true",
			meta: map[string]string{
				"state":               "archived",
				"continuity_eligible": "true",
			},
			want: true,
		},
		{
			name: "archived missing continuity",
			meta: map[string]string{
				"state": "archived",
			},
			want: false,
		},
		{
			name: "archived explicit false",
			meta: map[string]string{
				"state":               "archived",
				"continuity_eligible": "false",
			},
			want: false,
		},
		{
			name: "closing explicit true",
			meta: map[string]string{
				"state":               "closing",
				"continuity_eligible": "true",
			},
			want: false,
		},
		{
			name: "failed create releases continuity",
			meta: map[string]string{
				"state": string(StateFailedCreate),
			},
			want: false,
		},
		{
			name: "asleep missing continuity",
			meta: map[string]string{
				"state": "asleep",
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NamedSessionContinuityEligible(beads.Bead{Metadata: tt.meta})
			if got != tt.want {
				t.Fatalf("NamedSessionContinuityEligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindCanonicalNamedSessionBead_SkipsFailedCreate(t *testing.T) {
	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	candidates := []beads.Bead{
		{
			ID:     "failed",
			Type:   BeadType,
			Status: "open",
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: spec.Identity,
				"session_name":               spec.SessionName,
				"state":                      string(StateFailedCreate),
			},
		},
	}

	if got, ok := FindCanonicalNamedSessionBead(candidates, spec); ok {
		t.Fatalf("FindCanonicalNamedSessionBead() = %q, want no canonical failed-create bead", got.ID)
	}
}

func TestFindNamedSessionConflict_SelectsLiveNonCanonicalConflict(t *testing.T) {
	spec := NamedSessionSpec{
		Agent:       &config.Agent{Name: "worker", Dir: "myrig"},
		Identity:    "myrig/worker",
		SessionName: "session-city-myrig-worker",
	}
	candidates := []beads.Bead{
		{
			ID:     "closed-conflict",
			Type:   BeadType,
			Status: "closed",
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"alias": "myrig/worker",
			},
		},
		{
			ID:     "canonical",
			Type:   BeadType,
			Status: "open",
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				NamedSessionMetadataKey:      "true",
				NamedSessionIdentityMetadata: "myrig/worker",
				"session_name":               "session-city-myrig-worker",
				"template":                   "myrig/worker",
			},
		},
		{
			ID:     "non-session",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"alias": "myrig/worker",
			},
		},
		{
			ID:     "live-conflict",
			Type:   BeadType,
			Status: "open",
			Labels: []string{LabelSession},
			Metadata: map[string]string{
				"alias":    "myrig/worker",
				"template": "myrig/other",
			},
		},
	}

	bead, ok := FindNamedSessionConflict(candidates, spec)
	if !ok {
		t.Fatal("FindNamedSessionConflict() did not find live conflict")
	}
	if bead.ID != "live-conflict" {
		t.Fatalf("FindNamedSessionConflict() = %q, want live-conflict", bead.ID)
	}
}

func TestFindClosedNamedSessionBeadForSessionName_PrefersMatchingCanonicalCandidate(t *testing.T) {
	store := beads.NewMemStore()
	retired, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(retired): %v", err)
	}
	if err := store.Close(retired.ID); err != nil {
		t.Fatalf("Close(retired): %v", err)
	}
	canonical, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	if err := store.Close(canonical.ID); err != nil {
		t.Fatalf("Close(canonical): %v", err)
	}

	found, ok, err := FindClosedNamedSessionBeadForSessionName(store, "mayor", "test-city--mayor")
	if err != nil {
		t.Fatalf("FindClosedNamedSessionBeadForSessionName: %v", err)
	}
	if !ok {
		t.Fatal("FindClosedNamedSessionBeadForSessionName did not find canonical mayor bead")
	}
	if found.ID != canonical.ID {
		t.Fatalf("found bead ID = %q, want canonical %q", found.ID, canonical.ID)
	}
}

func TestFindClosedNamedSessionBeadForSessionName_SkipsTerminalRetiredCandidate(t *testing.T) {
	store := beads.NewMemStore()
	orphaned, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			"close_reason":               "orphaned",
			"state":                      "orphaned",
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(orphaned): %v", err)
	}
	if err := store.Close(orphaned.ID); err != nil {
		t.Fatalf("Close(orphaned): %v", err)
	}

	found, ok, err := FindClosedNamedSessionBeadForSessionName(store, "mayor", "test-city--mayor")
	if err != nil {
		t.Fatalf("FindClosedNamedSessionBeadForSessionName: %v", err)
	}
	if ok {
		t.Fatalf("FindClosedNamedSessionBeadForSessionName returned %q, want no reusable bead", found.ID)
	}
}

func TestFindClosedNamedSessionBeadForSessionName_SkipsFailedCreateCandidate(t *testing.T) {
	store := beads.NewMemStore()
	failed, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			"close_reason":               string(StateFailedCreate),
			"state":                      string(StateFailedCreate),
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(failed-create): %v", err)
	}
	if err := store.Close(failed.ID); err != nil {
		t.Fatalf("Close(failed-create): %v", err)
	}

	found, ok, err := FindClosedNamedSessionBeadForSessionName(store, "mayor", "test-city--mayor")
	if err != nil {
		t.Fatalf("FindClosedNamedSessionBeadForSessionName: %v", err)
	}
	if ok {
		t.Fatalf("FindClosedNamedSessionBeadForSessionName returned %q, want no reusable failed-create bead", found.ID)
	}
}

func TestFindClosedNamedSessionBead_PrefersNewestClosedCanonical(t *testing.T) {
	store := beads.NewMemStore()
	older, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(older): %v", err)
	}
	if err := store.Close(older.ID); err != nil {
		t.Fatalf("Close(older): %v", err)
	}
	newer, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":               "test-city--mayor",
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(newer): %v", err)
	}
	if err := store.Close(newer.ID); err != nil {
		t.Fatalf("Close(newer): %v", err)
	}

	found, ok, err := FindClosedNamedSessionBead(store, "mayor")
	if err != nil {
		t.Fatalf("FindClosedNamedSessionBead: %v", err)
	}
	if !ok {
		t.Fatal("FindClosedNamedSessionBead did not find closed mayor bead")
	}
	if found.ID != newer.ID {
		t.Fatalf("found bead ID = %q, want newest canonical %q", found.ID, newer.ID)
	}
}

func TestResolveNamedSessionSpecForConfigTarget_BareNameResolvesV2BoundSession(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "mayor",
			BindingName: "gastown",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "mayor",
			BindingName: "gastown",
		}},
	}
	spec, ok, err := ResolveNamedSessionSpecForConfigTarget(cfg, "test-city", "mayor", "")
	if err != nil {
		t.Fatalf("ResolveNamedSessionSpecForConfigTarget(mayor): %v", err)
	}
	if !ok {
		t.Fatal("ResolveNamedSessionSpecForConfigTarget(mayor) = false, want true")
	}
	if spec.Identity != "gastown.mayor" {
		t.Fatalf("spec.Identity = %q, want gastown.mayor", spec.Identity)
	}
}

func TestResolveNamedSessionSpecForConfigTarget_BareNameAmbiguousAcrossBindings(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", BindingName: "gastown"},
			{Name: "mayor", BindingName: "otherpack"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", BindingName: "gastown"},
			{Template: "mayor", BindingName: "otherpack"},
		},
	}
	_, ok, err := ResolveNamedSessionSpecForConfigTarget(cfg, "test-city", "mayor", "")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ResolveNamedSessionSpecForConfigTarget(mayor) ok=%v err=%v, want ErrAmbiguous", ok, err)
	}
	if ok {
		t.Fatal("ResolveNamedSessionSpecForConfigTarget(mayor) = true, want false on ambiguity")
	}
}

func TestResolveNamedSessionSpecForConfigTarget_BareNameAmbiguousAcrossRigAndCity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", BindingName: "citypack"},
			{Name: "mayor", BindingName: "rigpack", Dir: "demo"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", BindingName: "citypack"},
			{Template: "mayor", BindingName: "rigpack", Dir: "demo"},
		},
	}
	_, ok, err := ResolveNamedSessionSpecForConfigTarget(cfg, "test-city", "mayor", "demo")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ResolveNamedSessionSpecForConfigTarget(mayor, demo) ok=%v err=%v, want ErrAmbiguous across rig+city scopes", ok, err)
	}
}

func TestResolveNamedSessionSpecForConfigTarget_BareNameAmbiguousMixesDirectAndBareMatches(t *testing.T) {
	// A V1 rig-scoped entry (direct identity == "demo/mayor") plus a V2
	// city import (bare leaf == "mayor") must surface as ErrAmbiguous
	// when the user types bare "mayor" inside rig "demo". Otherwise the
	// direct-identity loop would silently shadow the V2 import.
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "mayor", Dir: "demo"},
			{Name: "mayor", BindingName: "citypack"},
		},
		NamedSessions: []config.NamedSession{
			{Template: "mayor", Dir: "demo"},
			{Template: "mayor", BindingName: "citypack"},
		},
	}
	_, ok, err := ResolveNamedSessionSpecForConfigTarget(cfg, "test-city", "mayor", "demo")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ResolveNamedSessionSpecForConfigTarget(mayor, demo) ok=%v err=%v, want ErrAmbiguous across direct+bare matches", ok, err)
	}
}

func TestResolveNamedSessionSpecForConfigTarget_BareNameIgnoresRigScopedOutsideRig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "witness",
			Dir:  "demo",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}
	if _, ok, err := ResolveNamedSessionSpecForConfigTarget(cfg, "test-city", "witness", ""); err != nil || ok {
		t.Fatalf("ResolveNamedSessionSpecForConfigTarget(witness) ok=%v err=%v, want not found outside rig context", ok, err)
	}
	spec, ok, err := ResolveNamedSessionSpecForConfigTarget(cfg, "test-city", "witness", "demo")
	if err != nil {
		t.Fatalf("ResolveNamedSessionSpecForConfigTarget(witness, demo): %v", err)
	}
	if !ok {
		t.Fatal("ResolveNamedSessionSpecForConfigTarget(witness, demo) = false, want true")
	}
	if spec.Identity != "demo/witness" {
		t.Fatalf("spec.Identity = %q, want demo/witness", spec.Identity)
	}
}

func TestFindClosedNamedSessionBead_AcceptsLegacySessionType(t *testing.T) {
	store := beads.NewMemStore()
	legacy, err := store.Create(beads.Bead{
		Type:   "gc:session",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":               "mayor",
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(legacy): %v", err)
	}
	if err := store.Close(legacy.ID); err != nil {
		t.Fatalf("Close(legacy): %v", err)
	}

	found, ok, err := FindClosedNamedSessionBead(store, "mayor")
	if err != nil {
		t.Fatalf("FindClosedNamedSessionBead: %v", err)
	}
	if !ok {
		t.Fatal("FindClosedNamedSessionBead did not find legacy typed session bead")
	}
	if found.ID != legacy.ID {
		t.Fatalf("found bead ID = %q, want legacy %q", found.ID, legacy.ID)
	}
}

// listCountingStore wraps a MemStore and records every List query so tests
// can assert on call count and shape.
type listCountingStore struct {
	*beads.MemStore
	queries []beads.ListQuery
}

func (s *listCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.MemStore.List(query)
}

func TestLookupConfiguredNamedSession_BoundedConflictQueries(t *testing.T) {
	store := &listCountingStore{MemStore: beads.NewMemStore()}
	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	conflict, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     "other",
			"agent_name":   "other",
		},
	})
	if err != nil {
		t.Fatalf("Create(conflict): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if !lookup.HasConflict {
		t.Fatal("HasConflict = false, want true")
	}
	if lookup.Conflict.ID != conflict.ID {
		t.Fatalf("Conflict.ID = %q, want %q", lookup.Conflict.ID, conflict.ID)
	}
	if len(store.queries) > 4 {
		t.Fatalf("List calls = %d, want bounded small constant without duplicate session_name lookup", len(store.queries))
	}
	for i, query := range store.queries {
		if len(query.Metadata) == 0 {
			t.Fatalf("query #%d has no metadata filter: %+v", i, query)
		}
	}
}

func TestLookupConfiguredNamedSession_AcceptsTypeOnlyCanonicalBead(t *testing.T) {
	store := beads.NewMemStore()
	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	canonical, err := store.Create(beads.Bead{
		Type: BeadType,
		Metadata: map[string]string{
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: spec.Identity,
			"session_name":               spec.SessionName,
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if !lookup.HasCanonical {
		t.Fatal("HasCanonical = false, want true")
	}
	if lookup.Canonical.ID != canonical.ID {
		t.Fatalf("Canonical.ID = %q, want %q", lookup.Canonical.ID, canonical.ID)
	}
}

// TestLookupConfiguredNamedSession_AliasOnlyBeadWithoutOtherSignalsConflicts
// is the corrected-understanding regression test for ga-t3a0fv round 2: a
// bead whose ONLY signal is a bare alias match (no configured_named_session
// flag, no configured_named_identity, no session_name, no matching template
// or agent_name) is not trusted as spec's own canonical session — it is
// reported as a conflict, exactly like any other unrelated bead that claims
// the same alias. Round 1 of ga-t3a0fv trusted this shape as canonical self
// (on the theory that an empty session_name meant "no competing claim"),
// but that is indistinguishable from a zero-cost decoy an attacker (or an
// unrelated buggy process) can plant with the same three lines of metadata
// — see TestLookupConfiguredNamedSession_UnrelatedAliasOnlyBeadNotTrustedAsSelf,
// which uses a byte-for-byte identical bead/spec shape and must also
// conflict rather than resolve canonical. The original bug this was trying
// to fix (ga-1ycmli: a live singleton named-session bead that never got its
// identity metadata stamped is unmailable while running) is still open;
// closing it safely needs a caller-supplied signal beyond what (bead, spec)
// alone can provide.
func TestLookupConfiguredNamedSession_AliasOnlyBeadWithoutOtherSignalsConflicts(t *testing.T) {
	store := beads.NewMemStore()
	spec := NamedSessionSpec{
		Identity:    "pack-author.pack-author",
		SessionName: "test-city--pack-author",
	}
	aliasOnly, err := store.Create(beads.Bead{
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias": spec.Identity,
		},
	})
	if err != nil {
		t.Fatalf("Create(aliasOnly): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if lookup.HasCanonical {
		t.Fatalf("HasCanonical = true (bead %q), want a bare alias-only bead treated as a conflict, not trusted as canonical self", lookup.Canonical.ID)
	}
	if !lookup.HasConflict {
		t.Fatal("HasConflict = false, want true")
	}
	if lookup.Conflict.ID != aliasOnly.ID {
		t.Fatalf("Conflict.ID = %q, want %q", lookup.Conflict.ID, aliasOnly.ID)
	}
}

// TestLookupConfiguredNamedSession_UnrelatedAliasOnlyBeadNotTrustedAsSelf is
// the reviewer's suggested regression test for ga-t3a0fv round 2 (same
// logic, adapted to this file's error-checking convention): a decoy bead
// that merely claims spec's alias, with nothing else set, must never
// resolve as canonical self. It is metadata-identical to the "self" bead in
// TestLookupConfiguredNamedSession_AliasOnlyBeadWithoutOtherSignalsConflicts
// — the two tests together are the actual boundary this fix draws: no
// predicate over (bead, spec) alone can tell a genuine not-yet-named self
// bead apart from this decoy, so neither is trusted.
func TestLookupConfiguredNamedSession_UnrelatedAliasOnlyBeadNotTrustedAsSelf(t *testing.T) {
	store := beads.NewMemStore()
	spec := NamedSessionSpec{Identity: "mayor", SessionName: "test-city--mayor"}
	decoy, err := store.Create(beads.Bead{
		Type:     BeadType,
		Status:   "open",
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"alias": spec.Identity},
	})
	if err != nil {
		t.Fatalf("Create(decoy): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if lookup.HasCanonical && lookup.Canonical.ID == decoy.ID {
		t.Fatalf("decoy bead with bare alias claim trusted as canonical self")
	}
}

// TestLookupConfiguredNamedSession_DifferentIdentitySimilarAliasStillConflicts
// guards ga-t3a0fv's second acceptance criterion: the self-match fix stays
// keyed on exact identity equivalence, not substring/prefix similarity. A
// bead whose alias merely resembles spec's identity is never fetched as a
// self candidate (the store query itself is an exact-value filter) and must
// not suppress a genuine, unrelated session_name collision.
func TestLookupConfiguredNamedSession_DifferentIdentitySimilarAliasStillConflicts(t *testing.T) {
	store := beads.NewMemStore()
	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	if _, err := store.Create(beads.Bead{
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias": "mayor-backup",
		},
	}); err != nil {
		t.Fatalf("Create(similar but different identity): %v", err)
	}
	conflict, err := store.Create(beads.Bead{
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     "other",
			"agent_name":   "other",
		},
	})
	if err != nil {
		t.Fatalf("Create(conflict): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if !lookup.HasConflict {
		t.Fatal("HasConflict = false, want true — a similarly-named but distinct alias must not suppress a genuine session_name collision")
	}
	if lookup.Conflict.ID != conflict.ID {
		t.Fatalf("Conflict.ID = %q, want %q", lookup.Conflict.ID, conflict.ID)
	}
}

// TestLookupConfiguredNamedSession_AliasMatchWithMismatchedSessionNameStillConflicts
// guards the boundary of the ga-t3a0fv self-match fix: the alias fallback in
// namedSessionCandidateIsSelf only trusts an exact alias match when the
// candidate declares no session_name at all. A bead that reused spec's alias
// while running under a visibly different session_name (a "squatter") is
// real evidence of a distinct live session, not spec's own bead under a
// different label, and must still be reported as a conflict — mirroring
// cmd/gc's TestResolveSessionIDWithConfig_ReservedNamedTargetConflictsWithLiveAlias.
func TestLookupConfiguredNamedSession_AliasMatchWithMismatchedSessionNameStillConflicts(t *testing.T) {
	store := beads.NewMemStore()
	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	squatter, err := store.Create(beads.Bead{
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-squatter",
			"alias":        spec.Identity,
		},
	})
	if err != nil {
		t.Fatalf("Create(squatter): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if lookup.HasCanonical {
		t.Fatalf("HasCanonical = true (bead %q), want the mismatched-session_name squatter treated as a conflict, not self", lookup.Canonical.ID)
	}
	if !lookup.HasConflict {
		t.Fatal("HasConflict = false, want true")
	}
	if lookup.Conflict.ID != squatter.ID {
		t.Fatalf("Conflict.ID = %q, want %q", lookup.Conflict.ID, squatter.ID)
	}
}

// TestLookupConfiguredNamedSession_SessionNameConflictReportedOverBareAliasMatch
// restores the pre-ga-t3a0fv-round-1 ordering: when a session_name conflict
// and a bare alias-only match are both present, the session_name conflict is
// what surfaces. Round 1 of ga-t3a0fv briefly made an exact alias match
// short-circuit to canonical ahead of this collision; round 2 reverted that
// (see TestLookupConfiguredNamedSession_AliasOnlyBeadWithoutOtherSignalsConflicts)
// because the alias-only signal alone cannot distinguish spec's own
// not-yet-named bead from an unrelated decoy claiming the same alias.
func TestLookupConfiguredNamedSession_SessionNameConflictReportedOverBareAliasMatch(t *testing.T) {
	store := beads.NewMemStore()
	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	if _, err := store.Create(beads.Bead{
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias":      spec.Identity,
			"template":   "other",
			"agent_name": "other",
		},
	}); err != nil {
		t.Fatalf("Create(alias-only bead): %v", err)
	}
	conflict, err := store.Create(beads.Bead{
		Type:   BeadType,
		Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     "other",
			"agent_name":   "other",
		},
	})
	if err != nil {
		t.Fatalf("Create(session_name conflict): %v", err)
	}

	lookup, err := LookupConfiguredNamedSession(store, spec)
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession: %v", err)
	}
	if lookup.HasCanonical {
		t.Fatalf("HasCanonical = true (bead %q), want the session_name conflict reported instead of the bare alias match resolved canonical", lookup.Canonical.ID)
	}
	if !lookup.HasConflict {
		t.Fatal("HasConflict = false, want true")
	}
	if lookup.Conflict.ID != conflict.ID {
		t.Fatalf("Conflict.ID = %q, want %q", lookup.Conflict.ID, conflict.ID)
	}
}

func TestLookupConfiguredNamedSession_EmptySpecNoListCall(t *testing.T) {
	store := &listCountingStore{MemStore: beads.NewMemStore()}

	lookup, err := LookupConfiguredNamedSession(store, NamedSessionSpec{})
	if err != nil {
		t.Fatalf("LookupConfiguredNamedSession(empty): %v", err)
	}
	if lookup.HasCanonical || lookup.HasConflict {
		t.Fatalf("lookup = %+v, want empty result", lookup)
	}
	if len(store.queries) != 0 {
		t.Fatalf("List calls = %d, want 0", len(store.queries))
	}
}

func TestNamedSessionResolutionCandidates_SingleListByLabel(t *testing.T) {
	store := &listCountingStore{MemStore: beads.NewMemStore()}
	canonical, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "mayor",
			"session_name":               "test-city--mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(canonical): %v", err)
	}
	// Bead matched only by session_name == identity (legacy / fallback path).
	bareSessionName, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(bareSessionName): %v", err)
	}
	// Bead matched only by alias == identity.
	aliased, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"alias":    "mayor",
			"template": "myrig/other",
		},
	})
	if err != nil {
		t.Fatalf("Create(aliased): %v", err)
	}
	// Bead that should NOT be returned — different identity.
	if _, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			NamedSessionIdentityMetadata: "polecat",
			"session_name":               "test-city--polecat",
		},
	}); err != nil {
		t.Fatalf("Create(polecat): %v", err)
	}
	// Non-session bead with matching alias — must be excluded.
	if _, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			"alias": "mayor",
		},
	}); err != nil {
		t.Fatalf("Create(non-session): %v", err)
	}
	// Closed session with matching identity — must be excluded (live only).
	closed, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			NamedSessionIdentityMetadata: "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close(closed): %v", err)
	}

	spec := NamedSessionSpec{
		Identity:    "mayor",
		SessionName: "test-city--mayor",
	}
	got, err := NamedSessionResolutionCandidates(store, spec)
	if err != nil {
		t.Fatalf("NamedSessionResolutionCandidates: %v", err)
	}
	gotIDs := make(map[string]bool, len(got))
	for _, b := range got {
		gotIDs[b.ID] = true
	}
	for _, want := range []string{canonical.ID, bareSessionName.ID, aliased.ID} {
		if !gotIDs[want] {
			t.Errorf("missing expected candidate %q in %v", want, gotIDs)
		}
	}
	if gotIDs[closed.ID] {
		t.Errorf("closed session %q must not appear in live candidates", closed.ID)
	}

	// One List call total — the contention budget that motivated this
	// implementation. Pre-collapse, this path issued four sequential
	// metadata-field List calls per resolution.
	if len(store.queries) != 1 {
		t.Fatalf("expected 1 store.List call, got %d: %+v", len(store.queries), store.queries)
	}
	q := store.queries[0]
	if q.Label != LabelSession {
		t.Errorf("query.Label = %q, want %q", q.Label, LabelSession)
	}
	if q.IncludeClosed {
		t.Errorf("query.IncludeClosed = true, want false (live candidates only)")
	}
	if len(q.Metadata) != 0 {
		t.Errorf("query.Metadata = %+v, want empty (label-scoped scan, in-process filter)", q.Metadata)
	}
}

func TestNamedSessionResolutionCandidates_EmptySpecNoListCall(t *testing.T) {
	store := &listCountingStore{MemStore: beads.NewMemStore()}
	got, err := NamedSessionResolutionCandidates(store, NamedSessionSpec{})
	if err != nil {
		t.Fatalf("NamedSessionResolutionCandidates(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d candidates for empty spec, want 0", len(got))
	}
	if len(store.queries) != 0 {
		t.Fatalf("expected 0 store.List calls for empty spec, got %d", len(store.queries))
	}
}

func TestNamedSessionResolutionCandidates_NilStore(t *testing.T) {
	got, err := NamedSessionResolutionCandidates(nil, NamedSessionSpec{Identity: "mayor"})
	if err != nil {
		t.Fatalf("NamedSessionResolutionCandidates(nil): %v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil for nil store", got)
	}
}
