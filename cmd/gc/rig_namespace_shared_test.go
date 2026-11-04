package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
)

// Tests for the shared pieces of the rig namespace guard
// (rig_namespace_shared.go, rig_store_identity.go).

func TestCityHasGuardedRig(t *testing.T) {
	if cityHasGuardedRig(nil) || cityHasGuardedRig(&config.City{Rigs: []config.Rig{{Name: "beta"}}}) {
		t.Fatal("a city without a guarded rig reports one")
	}
	if !cityHasGuardedRig(&config.City{Rigs: []config.Rig{{Name: "a"}, {Name: "beta", Federation: config.RigFederationRig}}}) {
		t.Fatal("a guarded rig went unseen")
	}
	if !cityHasGuardedRig(&config.City{Rigs: []config.Rig{{Name: "beta", Federation: "bogus"}}}) {
		t.Fatal("an out-of-enum federation value must count as guarded (fail closed)")
	}
}

// TestGuardedRigForScope resolves scopes to the guarded rig whose store they
// open, by store identity, and refuses an unguarded rig sharing it.
func TestGuardedRigForScope(t *testing.T) {
	city := rnsBdCity(t)
	beta := filepath.Join(city, "beta")
	rnsScope(t, beta, `{"backend":"dolt","dolt_database":"beta"}`)
	gamma := filepath.Join(city, "gamma")
	rnsScope(t, gamma, `{"backend":"dolt","dolt_database":"gamma"}`)
	sub := filepath.Join(beta, "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "beta", Path: beta, Federation: config.RigFederationRig},
		{Name: "gamma", Path: gamma},
	}}
	for scope, want := range map[string]string{beta: "beta", sub: "beta", gamma: "", city: ""} {
		rig, err := guardedRigForScope(cfg, city, resolveStoreScopeRoot(city, scope))
		if err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
		got := ""
		if rig != nil {
			got = rig.Name
		}
		if got != want {
			t.Fatalf("scope %s: guarded rig %q, want %q", scope, got, want)
		}
	}
	alias := filepath.Join(city, "alias")
	rnsScope(t, alias, `{"backend":"dolt","dolt_database":"beta"}`)
	shared := &config.City{Rigs: []config.Rig{
		{Name: "beta", Path: beta, Federation: config.RigFederationRig},
		{Name: "alias", Path: alias},
	}}
	for _, scope := range []string{beta, alias} {
		if _, err := guardedRigForScope(shared, city, scope); err == nil {
			t.Fatalf("scope %s opened while an unguarded rig shares the guarded rig's store", scope)
		}
	}
	if rig, err := guardedRigForScope(&config.City{Rigs: []config.Rig{{Name: "beta", Path: beta}}}, city, beta); rig != nil || err != nil {
		t.Fatalf("a city without a guarded rig = %v, %v", rig, err)
	}
	sessions := rnsFakeResolver{}
	ns, err := rigNamespaceForScope(cfg, city, sub, sessions)
	if err != nil || ns.rigName() != "beta" || ns.sessions != ownSessionResolver(sessions) {
		t.Fatalf("rigNamespaceForScope(sub of beta) = %v, %v", ns, err)
	}
	if ns, err := rigNamespaceForScope(shared, city, alias, sessions); ns != nil || !strings.Contains(fmt.Sprint(err), `rig "alias"'s`) {
		t.Fatalf("rigNamespaceForScope over an aliased store = %v, %v", ns, err)
	}
}

// TestRigNamespaceForScopeSharedOrPathless covers the scope decisions the
// removed path-equality lookup (rigNamespaceFor) used to make, now by store
// identity: two guarded rigs on one store are refused rather than resolved,
// a guarded rig with no path of its own never matches a scope, and a scope
// that is no guarded rig's store, the city root included, is unguarded.
func TestRigNamespaceForScopeSharedOrPathless(t *testing.T) {
	city := rnsBdCity(t)
	alpha := filepath.Join(city, "alpha")
	rnsScope(t, alpha, `{"backend":"dolt","dolt_database":"shared"}`)
	beta := filepath.Join(city, "beta")
	rnsScope(t, beta, `{"backend":"dolt","dolt_database":"shared"}`)
	gamma := filepath.Join(city, "gamma")
	rnsScope(t, gamma, `{"backend":"dolt","dolt_database":"gamma"}`)
	sessions := rnsFakeResolver{}

	twoGuarded := &config.City{Rigs: []config.Rig{
		{Name: "alpha", Path: alpha, Federation: config.RigFederationRig},
		{Name: "beta", Path: beta, Federation: config.RigFederationRig},
	}}
	for _, scope := range []string{alpha, beta} {
		ns, err := rigNamespaceForScope(twoGuarded, city, scope, sessions)
		if ns != nil || err == nil {
			t.Fatalf("scope %s of two guarded rigs on one store = %v, %v; want a refusal", scope, ns, err)
		}
	}

	pathless := &config.City{Rigs: []config.Rig{
		{Name: "nopath", Federation: config.RigFederationRig},
		{Name: "gamma", Path: gamma, Federation: config.RigFederationRig},
	}}
	res := newControllerSessionResolver(city, nil, nil)
	ns, err := rigNamespaceForScope(pathless, city, gamma, res)
	if err != nil || ns.rigName() != "gamma" {
		t.Fatalf("gamma's scope beside a pathless guarded rig = %v, %v; want rig gamma", ns, err)
	}
	if ns.sessions != ownSessionResolver(res) {
		t.Fatal("the namespace does not carry the caller's resolver")
	}
	for _, scope := range []string{city, ""} {
		if ns, err := rigNamespaceForScope(pathless, city, scope, sessions); ns != nil || err != nil {
			t.Fatalf("scope %q = %v, %v; want unguarded", scope, ns, err)
		}
	}
}

func TestErrRigNamespaceIsOneSentinel(t *testing.T) {
	if !errors.Is(fmt.Errorf("refused: %w", errRigNamespace), config.ErrRigNamespace) {
		t.Fatal("cmd/gc's sentinel is not config.ErrRigNamespace")
	}
}

// TestNewCityCLISessionResolverOpensTheCitySessionsLazily checks the one-shot
// resolver opens the city's store only for a lookup, learns its prefix
// through the policy layer, and admits this city's pool session.
func TestNewCityCLISessionResolverOpensTheCitySessionsLazily(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "demo", Prefix: "hq"}}
	opens := 0
	store := rnsNewCountingStore(rnsSessionBead(poolIDB, templateB, poolNameB))
	res := newCityCLISessionResolver(cityPathB, cfg, func() (beads.SessionStore, error) {
		opens++
		return beadstest.Sessions(wrapStoreWithBeadPolicies(store, cfg)), nil
	})
	if opens != 0 {
		t.Fatal("the resolver opened the city store before any lookup")
	}
	rec, ok, err := res.lookupOwnSession(poolIDB)
	if err != nil || !ok || rec.Template != templateB {
		t.Fatalf("lookup = %+v, %v, %v", rec, ok, err)
	}
	if opens != 1 {
		t.Fatalf("opened the city store %d times, want 1", opens)
	}
	failing := newCityCLISessionResolver(cityPathB, cfg, func() (beads.SessionStore, error) { return beads.SessionStore{}, errors.New("no store") })
	if _, _, err := failing.lookupOwnSession(poolIDB); err == nil || !strings.Contains(err.Error(), "no store") {
		t.Fatalf("an unopenable city store = %v; want the error (fail closed)", err)
	}
}

// rnsScope makes dir a bd scope with its own .beads and metadata.
func rnsScope(t *testing.T, dir, metadata string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if metadata != "" {
		if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func rnsBdCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rnsScope(t, city, `{"backend":"dolt","dolt_database":"hq"}`)
	return city
}

func TestGuardedRigNeedsAStoreOfItsOwn(t *testing.T) {
	city := rnsBdCity(t)
	mk := func(name, metadata string) config.Rig {
		dir := filepath.Join(city, name)
		if metadata != "-" {
			rnsScope(t, dir, metadata)
		} else if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return config.Rig{Name: name, Path: dir, Federation: config.RigFederationRig}
	}
	own := mk("beta", `{"backend":"dolt","dolt_database":"beta"}`)
	noStore := mk("nostore", "-")
	sameDB := mk("samedb", `{"backend":"dolt","dolt_database":"hq"}`)
	redirected := mk("redir", "")
	if err := os.WriteFile(filepath.Join(city, "redir", ".beads", "redirect"), []byte("../.beads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	twinA := mk("twina", `{"backend":"dolt","dolt_database":"twin"}`)
	twinB := mk("twinb", `{"backend":"dolt","dolt_database":"twin"}`)
	atRoot := config.Rig{Name: "root", Path: city, Federation: config.RigFederationRig}
	sharedWithPlain := mk("sharedplain", `{"backend":"dolt","dolt_database":"plaindb"}`)
	plain := mk("plain", `{"backend":"dolt","dolt_database":"plaindb"}`)
	plain.Federation = ""
	cfg := &config.City{Rigs: []config.Rig{own, noStore, sameDB, redirected, twinA, twinB, atRoot, sharedWithPlain, plain}}

	if err := checkGuardedRigStore(cfg, city, own); err != nil {
		t.Fatalf("a guarded rig with its own store was refused: %v", err)
	}
	for _, tc := range []struct {
		rig  config.Rig
		want string
	}{
		{noStore, "no bead store of its own"},
		{sameDB, "the city's"},
		{redirected, "the city's"},
		{twinA, `rig "twinb"'s`},
		{atRoot, "city root"},
		{sharedWithPlain, `rig "plain"'s`},
	} {
		err := checkGuardedRigStore(cfg, city, tc.rig)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "give it its own store") {
			t.Fatalf("rig %s: %v; want a refusal naming %q", tc.rig.Name, err, tc.want)
		}
	}
}

// prefixStore is a sessions store reporting an id prefix.
type prefixStore struct {
	beads.Store
	prefix string
}

func (s prefixStore) IDPrefix() string { return s.prefix }

func TestOwnSessionIDPrefixes(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "demo", Prefix: "hq"}}
	direct := beadstest.Sessions(prefixStore{Store: beads.NewMemStore(), prefix: "gcs-"})
	if got := strings.Join(ownSessionIDPrefixes(cfg, direct), ","); got != "gcs,hq" {
		t.Fatalf("prefixes = %v", got)
	}
	hidden := beadstest.Sessions(wrapStoreWithBeadPolicies(prefixStore{Store: beads.NewMemStore(), prefix: "gcs"}, cfg))
	if got := beads.IDPrefixFor(hidden); got != "gcs" {
		t.Fatalf("beads.IDPrefixFor under the policy layer = %q; want the engine's", got)
	}
	if got := strings.Join(ownSessionIDPrefixes(cfg, hidden), ","); got != "gcs,hq" {
		t.Fatalf("prefixes under the policy layer = %v", got)
	}
	if got := strings.Join(ownSessionIDPrefixes(cfg, beads.SessionStore{}), ","); got != "hq" {
		t.Fatalf("prefixes before the store is open = %v; want the HQ prefix", got)
	}
	if got := strings.Join(ownSessionIDPrefixes(cfg, beadstest.Sessions(beads.NewMemStore())), ","); got != "hq" {
		t.Fatalf("prefixes of a store with no id prefix = %v; want the HQ prefix", got)
	}
}
