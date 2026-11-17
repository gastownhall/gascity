//go:build integration

package session

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/test/toolhome"
)

// newParityBdStore stands up a real bd scope (git init + bd init in embedded
// mode, gc's custom types registered) and returns the production BdStore over
// it. It is the recipe of internal/beads' newConditionalIntegrationBdStore:
// BEADS_DIR is pinned to the scope and bd runs re-homed, so neither an ambient
// BEADS_DIR nor a user-level dolt.shared-server can redirect it.
func newParityBdStore(t *testing.T) *beads.BdStore {
	t.Helper()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skipf("bd not on PATH: %v", err)
	}
	dir := beadstest.GuardedTempDir(t)
	git := exec.Command("git", "init", "--quiet", dir)
	env := git.Environ()
	kept := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") {
			continue
		}
		kept = append(kept, kv)
	}
	git.Env = kept
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	bdEnv := map[string]string{}
	for _, kv := range toolhome.Environ(os.Environ(), beadstest.GuardedTempDir(t)) {
		k, v, _ := strings.Cut(kv, "=")
		bdEnv[k] = v
	}
	bdEnv["BEADS_DIR"] = filepath.Join(dir, ".beads")
	bdEnv["BEADS_DOLT_AUTO_START"] = "0"
	runner := beads.ExecCommandRunnerWithExactEnvContext(context.Background(), beadstest.BdSubprocessEnv(bdEnv))
	if out, err := runner(dir, "bd", "init", "-p", "tst", "--skip-hooks", "--skip-agents"); err != nil {
		t.Fatalf("bd init: %v\n%s", err, out)
	}
	if out, err := runner(dir, "bd", "config", "set", "types.custom", strings.Join(beads.RequiredCustomTypes, ",")); err != nil {
		t.Fatalf("bd config set types.custom: %v\n%s", err, out)
	}
	if out, err := runner(dir, "bd", "version"); err == nil {
		t.Logf("bd under test: %s", strings.TrimSpace(string(out)))
	}
	return beads.NewBdStore(dir, runner)
}

// policyTierStore mirrors cmd/gc's beadPolicyStore read rewrite: a TierIssues
// List is widened to TierBoth before it reaches the store. Every production
// named-session lookup runs under that wrapper.
type policyTierStore struct{ beads.Store }

func (s policyTierStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.TierMode == beads.TierIssues {
		q.TierMode = beads.TierBoth
	}
	return s.Store.List(q)
}

type paritySeed struct {
	title   string
	noLabel bool
	meta    map[string]string
	// gapBefore delays this seed's creation. bd stores created_at at whole
	// seconds, so a gap past one second is what makes its created_at order
	// deterministic rather than an id tie-break.
	gapBefore time.Duration
}

// parityPick marks a scenario whose candidates are created inside one second.
// bd stores created_at at whole seconds, so such candidates tie on created_at
// and bd lists them by id while MemStore lists them by insertion: the two
// stores legitimately disagree on the candidates' order, so a verdict
// comparison across them proves nothing. The verdict is instead asserted, per
// backend, against the pick the election rule makes over the rows that
// backend's own List returned.
type parityPick struct {
	// query is the metadata filter whose rows are the election's candidates.
	query [2]string
	// prefix selects the candidates among those rows by seed title.
	prefix string
	// newest elects by the canonical-repair rule (all candidates share a
	// generation, so newest CreatedAt then largest ID); otherwise the first
	// candidate in (CreatedAt ASC, ID ASC) order, the conflict rule.
	newest bool
}

type parityScenario struct {
	name  string
	spec  NamedSessionSpec
	seeds []paritySeed
	// pick is set only for the same-second multi-candidate scenarios.
	pick *parityPick
}

// parityScenarios are the decoy shapes of named_config_test.go and
// named_session_ineligible_conflict_test.go, each in its own identity
// namespace so every scenario can share one store per backend, plus the shapes
// that probe the question the parity test exists to settle: whether the alias
// query can surface a canonical bead the three earlier queries missed, and
// what each backend does when more than one candidate qualifies.
func parityScenarios() []parityScenario {
	plain := func(ns string) NamedSessionSpec {
		return NamedSessionSpec{Identity: ns + "-mayor", SessionName: "test-city--" + ns + "-mayor"}
	}
	other := func(m map[string]string) map[string]string {
		m["template"] = "other"
		m["agent_name"] = "other"
		return m
	}
	var out []parityScenario

	s := plain("p01")
	out = append(out, parityScenario{"bounded-conflict", s, []paritySeed{
		{title: "conflict", meta: other(map[string]string{"session_name": s.SessionName})},
	}, nil})

	s = plain("p02")
	out = append(out, parityScenario{"type-only-canonical", s, []paritySeed{
		{title: "canonical", noLabel: true, meta: map[string]string{
			NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: s.Identity, "session_name": s.SessionName,
		}},
	}, nil})

	s = NamedSessionSpec{Identity: "p03-pack-author.pack-author", SessionName: "test-city--p03-pack-author"}
	out = append(out, parityScenario{"alias-only-conflicts", s, []paritySeed{
		{title: "alias-only", meta: map[string]string{"alias": s.Identity}},
	}, nil})

	s = plain("p04")
	out = append(out, parityScenario{"unrelated-alias-decoy", s, []paritySeed{
		{title: "decoy", meta: map[string]string{"alias": s.Identity}},
	}, nil})

	s = plain("p05")
	out = append(out, parityScenario{"similar-alias-does-not-mask-conflict", s, []paritySeed{
		{title: "similar-alias", meta: map[string]string{"alias": s.Identity + "-backup"}},
		{title: "conflict", meta: other(map[string]string{"session_name": s.SessionName})},
	}, nil})

	s = plain("p06")
	out = append(out, parityScenario{"alias-squatter-other-session-name", s, []paritySeed{
		{title: "squatter", meta: map[string]string{"session_name": "s-gc-p06-squatter", "alias": s.Identity}},
	}, nil})

	s = plain("p07")
	out = append(out, parityScenario{"session-name-conflict-over-bare-alias", s, []paritySeed{
		{title: "alias-only", meta: other(map[string]string{"alias": s.Identity})},
		{title: "conflict", meta: other(map[string]string{"session_name": s.SessionName})},
	}, nil})

	// Reached by the alias query only: the identity value carries a leading
	// space, so the exact-value configured_named_identity query misses it,
	// while the strict canonical predicate trims before comparing.
	s = plain("p08")
	out = append(out, parityScenario{"padded-identity-reached-only-by-alias", s, []paritySeed{
		{title: "padded-canonical", meta: map[string]string{
			NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: " " + s.Identity, "alias": s.Identity,
		}},
	}, nil})

	s = NamedSessionSpec{
		Agent:       &config.Agent{Name: "p09-deployer", Dir: "beads"},
		Identity:    "beads/p09-deployer",
		SessionName: "beads--p09-deployer",
	}
	out = append(out, parityScenario{"alias-owned-pool-bead", s, []paritySeed{
		{title: "alias-owned", meta: map[string]string{
			"state": "awake", "session_name": "p09-deployer-inst-live",
			"template": s.Identity, "agent_name": s.Identity, "alias": s.Identity,
		}},
	}, nil})

	s = plain("p10")
	canonical := func(s NamedSessionSpec) map[string]string {
		return map[string]string{
			NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: s.Identity, "session_name": s.SessionName,
		}
	}
	out = append(out, parityScenario{"two-canonical-candidates", s, []paritySeed{
		{title: "canonical-first", meta: canonical(s)},
		{title: "canonical-second", meta: canonical(s)},
	}, &parityPick{query: [2]string{NamedSessionIdentityMetadata, s.Identity}, prefix: "canonical", newest: true}})

	s = plain("p11")
	out = append(out, parityScenario{"two-conflict-candidates", s, []paritySeed{
		{title: "conflict-first", meta: other(map[string]string{"session_name": s.SessionName})},
		{title: "conflict-second", meta: other(map[string]string{"session_name": s.SessionName})},
	}, &parityPick{query: [2]string{"session_name", s.SessionName}, prefix: "conflict"}})

	s = plain("p12")
	out = append(out, parityScenario{"two-canonical-candidates-a-second-apart", s, []paritySeed{
		{title: "canonical-first", meta: canonical(s)},
		{title: "canonical-second", meta: canonical(s), gapBefore: 1200 * time.Millisecond},
	}, nil})

	s = plain("p13")
	out = append(out, parityScenario{"two-conflict-candidates-a-second-apart", s, []paritySeed{
		{title: "conflict-first", meta: other(map[string]string{"session_name": s.SessionName})},
		{title: "conflict-second", meta: other(map[string]string{"session_name": s.SessionName}), gapBefore: 1200 * time.Millisecond},
	}, nil})
	return out
}

// lookupQueries are the exact metadata filters lookupConfiguredNamedSession
// issues for spec, in its order.
func lookupQueries(spec NamedSessionSpec) [][2]string {
	return [][2]string{
		{NamedSessionIdentityMetadata, spec.Identity},
		{"session_name", spec.SessionName},
		{"session_name", spec.Identity},
		{"alias", spec.Identity},
	}
}

type parityBackend struct {
	name  string
	store beads.Store
	title map[string]string // bead ID -> seed title
}

func (b parityBackend) titles(items []beads.Bead) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if title, ok := b.title[it.ID]; ok {
			out = append(out, title)
			continue
		}
		out = append(out, "?"+it.ID)
	}
	return out
}

func (b parityBackend) outcome(t *testing.T, store beads.Store, spec NamedSessionSpec) string {
	t.Helper()
	lookup, err := LookupConfiguredNamedSession(beadstest.Sessions(store), spec)
	if err != nil {
		t.Fatalf("%s: LookupConfiguredNamedSession: %v", b.name, err)
	}
	switch {
	case lookup.HasCanonical:
		return "canonical=" + b.titles([]beads.Bead{lookup.Canonical})[0]
	case lookup.HasConflict:
		return "conflict=" + b.titles([]beads.Bead{lookup.Conflict})[0]
	default:
		return "none"
	}
}

// expectedOutcome is the verdict the election rule makes over the rows this
// backend's own List returns for pick.query: the canonical-repair winner
// (newest CreatedAt, then largest ID) or the conflict rule's first row
// ((CreatedAt ASC, ID ASC)). It is computed here, from the rows, and not through
// the production selectors, so a regression in them cannot also move the
// expectation.
func (b parityBackend) expectedOutcome(t *testing.T, pick parityPick) string {
	t.Helper()
	items, err := b.store.List(beads.ListQuery{Metadata: map[string]string{pick.query[0]: pick.query[1]}, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("%s: List(%s=%q): %v", b.name, pick.query[0], pick.query[1], err)
	}
	var cands []beads.Bead
	for _, it := range items {
		if strings.HasPrefix(b.title[it.ID], pick.prefix) {
			cands = append(cands, it)
		}
	}
	if len(cands) < 2 {
		t.Fatalf("%s: %d %q candidates in List(%s=%q), want at least 2", b.name, len(cands), pick.prefix, pick.query[0], pick.query[1])
	}
	win := cands[0]
	for _, c := range cands[1:] {
		later := c.CreatedAt.After(win.CreatedAt) || (c.CreatedAt.Equal(win.CreatedAt) && c.ID > win.ID)
		earlier := c.CreatedAt.Before(win.CreatedAt) || (c.CreatedAt.Equal(win.CreatedAt) && c.ID < win.ID)
		if (pick.newest && later) || (!pick.newest && earlier) {
			win = c
		}
	}
	return pick.prefix + "=" + b.title[win.ID]
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// TestNamedSessionDecoyShapesMemStoreBdStoreParity settles ga-mp6ty6g: the
// named-session alias-resolution tests run on MemStore, so does a real bd
// agree with MemStore on store.List(ListQuery{Metadata: ...}) for those decoy
// shapes, and so on what LookupConfiguredNamedSession concludes?
//
// For every scenario it compares, MemStore against BdStore over a real bd:
// the SET of rows each of the lookup's four metadata queries returns (as the
// lookup issues it, and widened to TierBoth the way the production policy
// layer widens it), the label-scoped NamedSessionResolutionCandidates set, and
// the lookup's verdict, bare and policy-wrapped. Seeds are written with
// history and, in a second pair of stores, with no-history storage, the tier
// session beads get in production. The per-query ORDER each backend returned
// is logged: the lookup takes the first qualifying candidate, so order decides
// the verdict whenever more than one candidate qualifies.
func TestNamedSessionDecoyShapesMemStoreBdStoreParity(t *testing.T) {
	scenarios := parityScenarios()
	for _, storage := range []string{"history", "no-history"} {
		t.Run(storage, func(t *testing.T) {
			mem := parityBackend{name: "MemStore", store: beads.NewMemStore(), title: map[string]string{}}
			bd := parityBackend{name: "BdStore", store: newParityBdStore(t), title: map[string]string{}}
			for _, sc := range scenarios {
				for _, seed := range sc.seeds {
					time.Sleep(seed.gapBefore)
					for _, backend := range []parityBackend{mem, bd} {
						b := beads.Bead{
							Title:     sc.name + "/" + seed.title,
							Type:      BeadType,
							Metadata:  maps.Clone(seed.meta),
							NoHistory: storage == "no-history",
						}
						if !seed.noLabel {
							b.Labels = []string{LabelSession}
						}
						created, err := backend.store.Create(b)
						if err != nil {
							t.Fatalf("%s: Create(%s): %v", backend.name, b.Title, err)
						}
						backend.title[created.ID] = seed.title
					}
				}
			}

			for _, sc := range scenarios {
				t.Run(sc.name, func(t *testing.T) {
					for _, q := range lookupQueries(sc.spec) {
						for _, tier := range []beads.TierMode{beads.TierIssues, beads.TierBoth} {
							query := beads.ListQuery{Metadata: map[string]string{q[0]: q[1]}, TierMode: tier}
							got := map[string][]string{}
							for _, backend := range []parityBackend{mem, bd} {
								items, err := backend.store.List(query)
								if err != nil {
									t.Fatalf("%s: List(%s=%q, tier %d): %v", backend.name, q[0], q[1], tier, err)
								}
								got[backend.name] = backend.titles(items)
							}
							t.Logf("List %s=%q tier=%d order: MemStore=%v BdStore=%v", q[0], q[1], tier, got["MemStore"], got["BdStore"])
							if !slices.Equal(sortedCopy(got["MemStore"]), sortedCopy(got["BdStore"])) {
								t.Errorf("List %s=%q tier=%d: MemStore rows %v, BdStore rows %v", q[0], q[1], tier, got["MemStore"], got["BdStore"])
							}
						}
					}

					cands := map[string][]string{}
					for _, backend := range []parityBackend{mem, bd} {
						items, err := NamedSessionResolutionCandidates(beadstest.Sessions(backend.store), sc.spec)
						if err != nil {
							t.Fatalf("%s: NamedSessionResolutionCandidates: %v", backend.name, err)
						}
						cands[backend.name] = backend.titles(items)
					}
					t.Logf("NamedSessionResolutionCandidates order: MemStore=%v BdStore=%v", cands["MemStore"], cands["BdStore"])
					if !slices.Equal(sortedCopy(cands["MemStore"]), sortedCopy(cands["BdStore"])) {
						t.Errorf("NamedSessionResolutionCandidates: MemStore %v, BdStore %v", cands["MemStore"], cands["BdStore"])
					}

					for _, wrap := range []struct {
						name string
						fn   func(beads.Store) beads.Store
					}{
						{"bare", func(s beads.Store) beads.Store { return s }},
						{"policy-tier", func(s beads.Store) beads.Store { return policyTierStore{s} }},
					} {
						memOut := mem.outcome(t, wrap.fn(mem.store), sc.spec)
						bdOut := bd.outcome(t, wrap.fn(bd.store), sc.spec)
						t.Logf("LookupConfiguredNamedSession %s: MemStore=%s BdStore=%s", wrap.name, memOut, bdOut)
						if sc.pick == nil {
							if memOut != bdOut {
								t.Errorf("LookupConfiguredNamedSession %s: MemStore=%s BdStore=%s", wrap.name, memOut, bdOut)
							}
							continue
						}
						// Same-second candidates: the stores order them differently,
						// so each verdict is held to the pick over its own rows.
						for _, c := range []struct {
							backend parityBackend
							got     string
						}{{mem, memOut}, {bd, bdOut}} {
							if want := c.backend.expectedOutcome(t, *sc.pick); c.got != want {
								t.Errorf("LookupConfiguredNamedSession %s: %s=%s, want %s (the pick over its own rows)", wrap.name, c.backend.name, c.got, want)
							}
						}
					}
				})
			}
		})
	}
}
