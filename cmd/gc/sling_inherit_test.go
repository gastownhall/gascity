package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sling"
)

// slingInheritEnv returns a getenv over vars, the env seam inheritSlingInitiator
// reads the caller's session identity through.
func slingInheritEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// mustCreateInheritBead creates b in store, failing the test on error.
func mustCreateInheritBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%q): %v", b.Title, err)
	}
	return created
}

// inheritClaimFor creates an in_progress claim assigned to assignee whose run chain
// names rootID under chainKey ("" for a claim that is its own root).
func inheritClaimFor(t *testing.T, store beads.Store, assignee, chainKey, rootID string, meta map[string]string) beads.Bead {
	t.Helper()
	if meta == nil {
		meta = map[string]string{}
	}
	if chainKey != "" {
		meta[chainKey] = rootID
	}
	b := mustCreateInheritBead(t, store, beads.Bead{Title: "claimed step", Type: "task", Metadata: meta})
	status, owner := "in_progress", assignee
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
		t.Fatalf("Update(%s): %v", b.ID, err)
	}
	return b
}

// inheritRootWithInitiator creates a run root carrying initiator ("" for none).
func inheritRootWithInitiator(t *testing.T, store beads.Store, initiator string) beads.Bead {
	t.Helper()
	meta := map[string]string{}
	if initiator != "" {
		meta[beadmeta.InitiatorMetadataKey] = initiator
	}
	return mustCreateInheritBead(t, store, beads.Bead{Title: "run root", Type: "molecule", Metadata: meta})
}

func TestResolveInheritedInitiator(t *testing.T) {
	const session = "gc-sess-1"
	for _, tc := range []struct {
		name          string
		build         func(t *testing.T, work, graph beads.Store) (currentClaim string)
		want          string
		wantStderr    []string
		wantNoStderr  bool
		identities    []string
		currentClaimE error
	}{
		{
			name: "formula step claim inherits from molecule root",
			build: func(t *testing.T, work, _ beads.Store) string {
				root := inheritRootWithInitiator(t, work, "alice")
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				return ""
			},
			want:         "alice",
			wantNoStderr: true,
		},
		{
			name: "plain bead claim is its own root",
			build: func(t *testing.T, work, _ beads.Store) string {
				inheritClaimFor(t, work, session, "", "", map[string]string{beadmeta.InitiatorMetadataKey: "alice"})
				return ""
			},
			want:         "alice",
			wantNoStderr: true,
		},
		{
			name: "graph workflow root relocated to the graph store resolves",
			build: func(t *testing.T, work, graph beads.Store) string {
				root := inheritRootWithInitiator(t, graph, "carol")
				inheritClaimFor(t, work, session, "workflow_id", root.ID, nil)
				return ""
			},
			want:         "carol",
			wantNoStderr: true,
		},
		{
			name: "graph step claimed in the graph store resolves",
			build: func(t *testing.T, _, graph beads.Store) string {
				root := inheritRootWithInitiator(t, graph, "carol")
				inheritClaimFor(t, graph, session, "workflow_id", root.ID, nil)
				return ""
			},
			want:         "carol",
			wantNoStderr: true,
		},
		{
			name: "session current claim assigned under another identity inherits",
			build: func(t *testing.T, work, _ beads.Store) string {
				root := inheritRootWithInitiator(t, work, "dave")
				c := inheritClaimFor(t, work, "pool-seat-identity", beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				return c.ID
			},
			want:         "dave",
			wantNoStderr: true,
		},
		{
			name: "claims assigned to the session name and alias count",
			build: func(t *testing.T, work, _ beads.Store) string {
				root := inheritRootWithInitiator(t, work, "erin")
				inheritClaimFor(t, work, "rig/worker-1", beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				inheritClaimFor(t, work, "worker-alias", beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				return ""
			},
			identities:   []string{session, "rig/worker-1", "worker-alias"},
			want:         "erin",
			wantNoStderr: true,
		},
		{
			name: "two claims on one root agree",
			build: func(t *testing.T, work, _ beads.Store) string {
				root := inheritRootWithInitiator(t, work, "alice")
				a := inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				return a.ID
			},
			want:         "alice",
			wantNoStderr: true,
		},
		{
			name: "a root without an initiator does not conflict",
			build: func(t *testing.T, work, _ beads.Store) string {
				stamped := inheritRootWithInitiator(t, work, "alice")
				bare := inheritRootWithInitiator(t, work, "")
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, stamped.ID, nil)
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, bare.ID, nil)
				return ""
			},
			want:         "alice",
			wantNoStderr: true,
		},
		{
			name: "conflicting claims warn and stamp nothing",
			build: func(t *testing.T, work, _ beads.Store) string {
				a := inheritRootWithInitiator(t, work, "alice")
				b := inheritRootWithInitiator(t, work, "bob")
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, a.ID, nil)
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, b.ID, nil)
				return ""
			},
			want:       "",
			wantStderr: []string{"gc.initiator", "alice", "bob"},
		},
		{
			name:         "no claims is a no-op",
			build:        func(*testing.T, beads.Store, beads.Store) string { return "" },
			want:         "",
			wantNoStderr: true,
		},
		{
			name: "claims whose roots carry no initiator are a no-op",
			build: func(t *testing.T, work, _ beads.Store) string {
				root := inheritRootWithInitiator(t, work, "")
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				return ""
			},
			want:         "",
			wantNoStderr: true,
		},
		{
			name: "a closed current claim is not consulted",
			build: func(t *testing.T, work, _ beads.Store) string {
				root := inheritRootWithInitiator(t, work, "alice")
				c := inheritClaimFor(t, work, "someone-else", beadmeta.RootBeadIDMetadataKey, root.ID, nil)
				if err := work.Close(c.ID); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return c.ID
			},
			want:         "",
			wantNoStderr: true,
		},
		{
			name: "a dangling current claim id is ignored",
			build: func(*testing.T, beads.Store, beads.Store) string {
				return "gc-does-not-exist"
			},
			want:         "",
			wantNoStderr: true,
		},
		{
			name: "an unresolvable root warns and stamps nothing",
			build: func(t *testing.T, work, _ beads.Store) string {
				inheritClaimFor(t, work, session, beadmeta.RootBeadIDMetadataKey, "gc-missing-root", nil)
				return ""
			},
			want:       "",
			wantStderr: []string{"gc-missing-root"},
		},
		{
			name:          "a failed current-claim read warns and stamps nothing",
			build:         func(*testing.T, beads.Store, beads.Store) string { return "" },
			currentClaimE: errors.New("session store offline"),
			want:          "",
			wantStderr:    []string{"session store offline"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := beads.NewMemStore()
			graph := beads.NewMemStoreFrom(1000, nil, nil)
			current := tc.build(t, work, graph)
			ids := tc.identities
			if ids == nil {
				ids = []string{session}
			}
			var stderr bytes.Buffer
			got := resolveInheritedInitiator(slingInheritSources{
				identities: ids,
				currentClaim: func() (string, error) {
					return current, tc.currentClaimE
				},
				stores: []slingInheritReader{graph, work},
			}, &stderr)
			if got != tc.want {
				t.Fatalf("initiator = %q, want %q; stderr=%q", got, tc.want, stderr.String())
			}
			if tc.wantNoStderr && stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			for _, w := range tc.wantStderr {
				if !strings.Contains(stderr.String(), w) {
					t.Fatalf("stderr = %q, want it to mention %q", stderr.String(), w)
				}
			}
			if n := strings.Count(stderr.String(), "\n"); n > 1 {
				t.Fatalf("stderr has %d lines, want a single warning line: %q", n, stderr.String())
			}
		})
	}
}

// TestResolveInheritedInitiator_ListFailureStampsNothing proves a store that
// cannot be listed disables inheritance with a warning instead of letting a
// partial view pick an initiator a hidden claim would have contradicted.
func TestResolveInheritedInitiator_ListFailureStampsNothing(t *testing.T) {
	work := beads.NewMemStore()
	root := inheritRootWithInitiator(t, work, "alice")
	inheritClaimFor(t, work, "s1", beadmeta.RootBeadIDMetadataKey, root.ID, nil)
	var stderr bytes.Buffer
	got := resolveInheritedInitiator(slingInheritSources{
		identities: []string{"s1"},
		stores:     []slingInheritReader{work, inheritListFailStore{Store: beads.NewMemStore()}},
	}, &stderr)
	if got != "" {
		t.Fatalf("initiator = %q, want none", got)
	}
	if !strings.Contains(stderr.String(), "list exploded") {
		t.Fatalf("stderr = %q, want the list failure named", stderr.String())
	}
}

type inheritListFailStore struct{ beads.Store }

func (inheritListFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list exploded")
}

func TestInheritSlingInitiator_Gates(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts slingOpts
		env  map[string]string
		want string
	}{
		{
			name: "no session env is a no-op",
			opts: slingOpts{BeadOrFormula: "mol-deploy", IsFormula: true},
			env:  map[string]string{"GC_ALIAS": "worker"},
			want: "",
		},
		{
			name: "explicit initiator is never overridden",
			opts: slingOpts{BeadOrFormula: "mol-deploy", IsFormula: true, Initiator: "explicit"},
			env:  map[string]string{"GC_SESSION_ID": "gc-sess-1"},
			want: "explicit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			var stderr bytes.Buffer
			// Empty deps have no claim stores, so a run past the gate would
			// warn: a silent, unchanged result proves the gate held.
			inheritSlingInitiator(&opts, slingDeps{}, slingInheritEnv(tc.env), &stderr)
			if opts.Initiator != tc.want {
				t.Fatalf("Initiator = %q, want %q", opts.Initiator, tc.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

// TestInheritSlingInitiator_CityStores drives the production source wiring over
// an on-disk city with a rig: claims in the rig and city stores, assigned to the
// session's id and name, resolve to the run root's gc.initiator for both a
// formula sling and a plain bead sling, and claims on two initiators stamp
// nothing.
func TestInheritSlingInitiator_CityStores(t *testing.T) {
	cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t, nil)
	cfg, _, err := loadSlingCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadSlingCityConfig: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	// deps carries the same source-workflow store opener cmdSlingWithJSON wires.
	deps := slingDeps{SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
		views, _, err := openSourceWorkflowStoresWithProvider(cfg, cityDir, "", func(scopeRoot string) string {
			return authoritativeBeadsProviderForScope(scopeRoot, cityDir)
		}, func(dir string) (beads.Store, error) {
			return openAuthoritativeStoreAtForCity(dir, cityDir)
		})
		if err != nil {
			return nil, err
		}
		return sourceWorkflowStoresFromViews(cfg, cityDir, loadedCityName(cfg, cityDir), views)
	}}
	root := inheritRootWithInitiator(t, rigStore, "alice")
	inheritClaimFor(t, rigStore, "foundations/worker-a", beadmeta.RootBeadIDMetadataKey, root.ID, nil)
	env := slingInheritEnv(map[string]string{"GC_SESSION_ID": "gc-no-such-session", "GC_SESSION_NAME": "foundations/worker-a"})

	for _, opts := range []slingOpts{
		{BeadOrFormula: "mol-deploy", IsFormula: true},
		{BeadOrFormula: "fo-multi-work"},
	} {
		var stderr bytes.Buffer
		inheritSlingInitiator(&opts, deps, env, &stderr)
		if opts.Initiator != "alice" {
			t.Fatalf("%s: Initiator = %q, want alice; stderr=%q", opts.BeadOrFormula, opts.Initiator, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("%s: stderr = %q, want empty", opts.BeadOrFormula, stderr.String())
		}
	}

	other := inheritRootWithInitiator(t, cityStore, "bob")
	inheritClaimFor(t, cityStore, "gc-no-such-session", beadmeta.RootBeadIDMetadataKey, other.ID, nil)
	opts := slingOpts{BeadOrFormula: "fo-multi-work"}
	var stderr bytes.Buffer
	inheritSlingInitiator(&opts, deps, env, &stderr)
	if opts.Initiator != "" {
		t.Fatalf("conflicting claims: Initiator = %q, want none", opts.Initiator)
	}
	for _, w := range []string{root.ID, other.ID} {
		if !strings.Contains(stderr.String(), w) {
			t.Fatalf("stderr = %q, want root %s named", stderr.String(), w)
		}
	}
}
