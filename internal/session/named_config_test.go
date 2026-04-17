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
