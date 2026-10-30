package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// claimProvenanceTestCity is foreignIdentityTestCity plus a configured named
// session and a rig-scoped bound agent, the shape whose bare short-form claims
// only the bare-agent-name rule resolves.
func claimProvenanceTestCity(t *testing.T) *config.City {
	t.Helper()
	cfg := foreignIdentityTestCity(t)
	cfg.NamedSessions = []config.NamedSession{{Name: "lana", Template: "worker", Dir: "repo"}}
	cfg.Agents = append(cfg.Agents, config.Agent{Name: "backend_dev", Dir: "repo", BindingName: "pack"})
	return cfg
}

// seedProvenanceRecord creates a session record with a pinned ID, closed
// unless open is set.
func seedProvenanceRecord(t *testing.T, store *beads.MemStore, id string, open bool, metadata map[string]string) {
	t.Helper()
	store.HonorExplicitIDs = true
	b, err := store.Create(beads.Bead{
		ID:       id,
		Title:    "session record",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create session record %s: %v", id, err)
	}
	if !open {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("Close session record %s: %v", id, err)
		}
	}
}

// claimProvenanceSessionStore seeds the records the matrix vouches through:
// a closed seat under a qualified alias, a closed ephemeral seat under its
// runtime session_name, and an open and a closed seat known only by bead ID.
func claimProvenanceSessionStore(t *testing.T) *beads.MemStore {
	t.Helper()
	store := beads.NewMemStore()
	seedProvenanceRecord(t, store, "gc-seat1", false, map[string]string{"alias": "repo/retired-seat"})
	seedProvenanceRecord(t, store, "gc-d34d1", false, map[string]string{"session_name": "worker-gc-d34d1"})
	seedProvenanceRecord(t, store, "gc-live1", true, nil)
	seedProvenanceRecord(t, store, "gc-dead2", false, nil)
	return store
}

func storeOpener(store beads.Store) func() (beads.Store, error) {
	return func() (beads.Store, error) { return store, nil }
}

// TestClaimIdentityProvenance_Matrix pins the verdict for every claim shape the
// orphan-sweep order meets. Local and foreign rows share one table so neither
// half can pass vacuously.
func TestClaimIdentityProvenance_Matrix(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	store := claimProvenanceSessionStore(t)
	for _, tc := range []struct {
		identity string
		verdict  claimProvenanceVerdict
		reason   claimProvenanceReason
		detail   string
	}{
		{"repo/worker", claimVerdictLocal, claimReasonRoster, ""},
		{"repo/worker-36", claimVerdictLocal, claimReasonRoster, ""},
		{"repo/furiosa", claimVerdictLocal, claimReasonRoster, ""},
		{"repo/pack.worker-9", claimVerdictLocal, claimReasonRoster, ""},
		{"repo/lana", claimVerdictLocal, claimReasonRoster, ""},
		{"worker", claimVerdictLocal, claimReasonRoster, ""},
		{"backend_dev", claimVerdictLocal, claimReasonRoster, ""},
		{"backend_dev-2", claimVerdictLocal, claimReasonRoster, ""},
		{"repo/worker-adhoc-a1b2c3d4e5", claimVerdictLocal, claimReasonRoster, ""},

		{"repo/retired-seat", claimVerdictLocal, claimReasonSessionRecord, "gc-seat1"},
		{"worker-gc-d34d1", claimVerdictLocal, claimReasonSessionRecord, "gc-d34d1"},
		{"gc-dead2", claimVerdictLocal, claimReasonSessionRecord, "gc-dead2"},
		{"gc-live1", claimVerdictLocal, claimReasonSessionRecord, "gc-live1"},

		{"repo/pool.worker-1", claimVerdictForeign, claimReasonForeignBinding, "pool"},

		{"repo/dalinar", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"westeros/szeth", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"otherpack__polecat-bt-8x2kd", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"bt-q7m3v", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"we-wisp-126vyfx", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"repo--otherpack__polecat-4", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"Pat Example", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"humanoid", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
		{"repo/worker-007", claimVerdictForeign, claimReasonAbsentFromRoster, ""},
	} {
		t.Run(tc.identity, func(t *testing.T) {
			got := claimIdentityProvenance(cfg, "testcity", tc.identity, storeOpener(store))
			want := claimProvenance{Verdict: tc.verdict, Reason: tc.reason, Detail: tc.detail}
			if got != want {
				t.Fatalf("claimIdentityProvenance(%q) = %+v, want %+v", tc.identity, got, want)
			}
		})
	}
}

// TestClaimIdentityProvenance_AgreesWithPoolGateOnQualifiedIdentities keeps one
// implementation: for every well-formed <rig>/<name> identity the verb's
// verdict is local exactly when the Go pool sweep's gate observes it, with and
// without session records to vouch.
func TestClaimIdentityProvenance_AgreesWithPoolGateOnQualifiedIdentities(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	identities := []string{
		"repo/worker", "repo/pack.worker", "repo/worker-1", "repo/worker-36",
		"repo/pack.worker-9", "repo/furiosa", "repo/worker-adhoc-a1b2c3d4e5",
		"repo/pack.worker-adhoc-a1b2c3d4e5", "repo/worker-+1", "repo/worker-007",
		"repo/pack.nux", "repo/lana", "repo/pool.worker", "repo/pool.worker-1",
		"repo/pool.worker-adhoc-a1b2c3d4e5", "repo/pool.nux", "repo/dalinar",
		"westeros/szeth", "repo/retired-hand", "repo/retired-seat",
	}
	for name, store := range map[string]*beads.MemStore{
		"empty store":  beads.NewMemStore(),
		"seeded store": claimProvenanceSessionStore(t),
	} {
		t.Run(name, func(t *testing.T) {
			gate := newForeignClaimGate(cfg, beads.SessionStore{Store: store})
			for _, identity := range identities {
				got := claimIdentityProvenance(cfg, cfg.EffectiveCityName(), identity, storeOpener(store))
				if (got.Verdict == claimVerdictLocal) != gate.observable(identity) {
					t.Errorf("%s: verdict %+v, pool gate observable = %v; want agreement", identity, got, gate.observable(identity))
				}
			}
		})
	}
}

// TestClaimIdentityProvenance_DropsTheShapeExemption documents the deliberate
// difference from the Go pool sweep: that sweep only meets pool-routed work,
// so it exempts bare shapes, while the verb's callers have no route filter and
// must not.
func TestClaimIdentityProvenance_DropsTheShapeExemption(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	for _, identity := range []string{"bt-q7m3v", "otherpack__polecat-bt-8x2kd"} {
		if !poolAssigneeIsLocallyObservable(cfg, "testcity", identity) {
			t.Fatalf("poolAssigneeIsLocallyObservable(%q) = false; the Go sweep's shape exemption must stay unchanged", identity)
		}
		got := claimIdentityProvenance(cfg, "testcity", identity, storeOpener(beads.NewMemStore()))
		if got.Verdict != claimVerdictForeign {
			t.Fatalf("claimIdentityProvenance(%q) = %+v, want foreign", identity, got)
		}
	}
}

// TestClaimIdentityProvenance_NeighborRecordInSharedRigStoreIsNotProvenance
// reads only this city's session store: a neighbor's session record sitting in
// a shared rig store vouches for nothing here.
func TestClaimIdentityProvenance_NeighborRecordInSharedRigStoreIsNotProvenance(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	rigStore := beads.NewMemStore()
	seedProvenanceRecord(t, rigStore, "bt-neighbor", true, map[string]string{"alias": "repo/dalinar"})
	sessionStore := beads.NewMemStore()

	got := claimIdentityProvenance(cfg, "testcity", "repo/dalinar", storeOpener(sessionStore))
	if got.Verdict != claimVerdictForeign || got.Reason != claimReasonAbsentFromRoster {
		t.Fatalf("claimIdentityProvenance(repo/dalinar) = %+v, want foreign/absent_from_roster", got)
	}
}

// provenanceGetFailStore fails every Get with err.
type provenanceGetFailStore struct {
	beads.Store
	err error
}

func (s provenanceGetFailStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestClaimIdentityProvenance_LookupFailures keeps a failed lookup from
// reading as foreign or local: the verdict is unknown, which every caller
// must treat as "protect". A partial result whose rows vouch still answers.
func TestClaimIdentityProvenance_LookupFailures(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	unreachable := errors.New("session store unreachable")
	partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("one row failed to parse")}

	for _, tc := range []struct {
		name     string
		identity string
		open     func() (beads.Store, error)
	}{
		{"opener fails", "repo/dalinar", func() (beads.Store, error) { return nil, unreachable }},
		{"hard listing error", "repo/dalinar", storeOpener(&provenanceLookupStore{Store: beads.NewMemStore(), err: unreachable})},
		{"non-NotFound Get error", "bt-q7m3v", storeOpener(provenanceGetFailStore{Store: beads.NewMemStore(), err: unreachable})},
		{"partial result without a match", "repo/dalinar", storeOpener(&provenanceLookupStore{Store: beads.NewMemStore(), err: partial})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claimIdentityProvenance(cfg, "testcity", tc.identity, tc.open)
			if got.Verdict != claimVerdictUnknown || got.Reason != claimReasonLookupFailed {
				t.Fatalf("claimIdentityProvenance(%q) = %+v, want unknown/lookup_failed", tc.identity, got)
			}
			if !strings.Contains(got.Detail, unreachable.Error()) && !strings.Contains(got.Detail, "one row failed to parse") {
				t.Fatalf("detail %q does not carry the lookup error", got.Detail)
			}
		})
	}

	t.Run("partial result with a matching row", func(t *testing.T) {
		store := claimProvenanceSessionStore(t)
		got := claimIdentityProvenance(cfg, "testcity", "repo/retired-seat", storeOpener(&provenanceLookupStore{Store: store, err: partial}))
		want := claimProvenance{Verdict: claimVerdictLocal, Reason: claimReasonSessionRecord, Detail: "gc-seat1"}
		if got != want {
			t.Fatalf("claimIdentityProvenance(repo/retired-seat) = %+v, want %+v", got, want)
		}
	})
}

// TestClaimIdentityProvenance_EmptyRosterCannotAnswer keeps a config that
// resolved to no agents from reading every identity as foreign: a roster of 0
// is a resolution failure, not an answer.
func TestClaimIdentityProvenance_EmptyRosterCannotAnswer(t *testing.T) {
	for name, cfg := range map[string]*config.City{
		"nil config":   nil,
		"no agents":    {Rigs: []config.Rig{{Name: "repo"}}},
		"empty config": {},
	} {
		t.Run(name, func(t *testing.T) {
			opened := false
			got := claimIdentityProvenance(cfg, "testcity", "repo/dalinar", func() (beads.Store, error) {
				opened = true
				return beads.NewMemStore(), nil
			})
			if got.Verdict != claimVerdictUnknown || got.Reason != claimReasonNoConfig {
				t.Fatalf("claimIdentityProvenance = %+v, want unknown/no_config", got)
			}
			if opened {
				t.Fatal("the session store was opened for a city with no roster")
			}
		})
	}
}

// TestClaimIdentityProvenance_RosterHitNeverOpensTheStore pins the cost: a
// roster hit is a pure read of config.
func TestClaimIdentityProvenance_RosterHitNeverOpensTheStore(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	for _, identity := range []string{"repo/worker", "repo/furiosa", "worker", "backend_dev"} {
		opened := false
		got := claimIdentityProvenance(cfg, "testcity", identity, func() (beads.Store, error) {
			opened = true
			return nil, errors.New("must not open")
		})
		if got.Verdict != claimVerdictLocal || opened {
			t.Fatalf("claimIdentityProvenance(%q) = %+v, opened store = %v; want local without opening it", identity, got, opened)
		}
	}
}

func TestUnqualifiedAgentNameInLocalRoster(t *testing.T) {
	cfg := claimProvenanceTestCity(t)
	for _, tc := range []struct {
		identity string
		want     bool
	}{
		{"backend_dev", true},
		{"backend_dev-2", true},
		{"backend_dev-adhoc-a1b2", true},
		{"sibling", true},
		{"backend_dev-007", false},
		{"repo/backend_dev", false},
		{"pack.backend_dev", false},
		{"humanoid", false},
		{"", false},
	} {
		if got := unqualifiedAgentNameInLocalRoster(cfg, tc.identity); got != tc.want {
			t.Errorf("unqualifiedAgentNameInLocalRoster(%q) = %v, want %v", tc.identity, got, tc.want)
		}
	}
}
