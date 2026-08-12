package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func federatedHookTopology() config.QueryTopology {
	return config.QueryTopology{
		Beads:          config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105},
		FederatedReady: true,
	}
}

// fiveRigHookStores is the store list a city-scoped (cross-store-eligible) agent
// gets on a five-rig city: its own store first, then one per rig.
func fiveRigHookStores() []hookStore {
	stores := []hookStore{{dir: "city", env: []string{"GC_STORE=city"}}}
	for _, rig := range []string{"rig-A", "rig-B", "rig-C", "rig-D", "rig-E"} {
		stores = append(stores, hookStore{dir: rig, env: []string{"GC_STORE=" + rig}})
	}
	return stores
}

// TestFederatedHookStoresIssueTheCityWideReaderOnce is the cost measurement.
// `gc ready` federates the city store, every bound rig store and the relocated
// graph leg in ONE call, so asking it once per hookStore multiplies the leg
// fan-out by the store count for an answer that cannot differ between
// iterations. The whole query must carry the city-wide reader exactly once
// across the store list.
func TestFederatedHookStoresIssueTheCityWideReaderOnce(t *testing.T) {
	a := &config.Agent{Name: "worker"}
	topo := federatedHookTopology()
	singleStoreTopo := topo
	singleStoreTopo.FederatedReady = false
	federated := a.EffectiveWorkQueryFor(topo)
	singleStore := a.EffectiveWorkQueryFor(singleStoreTopo)

	perQuery := strings.Count(federated, "gc ready")
	if perQuery == 0 {
		t.Fatalf("the federated work query names no `gc ready` reader: %q", federated)
	}

	scoped := scopeFederatedHookStores(fiveRigHookStores(), federated, singleStore)
	total := 0
	for _, st := range scoped {
		total += strings.Count(hookStoreCommand(st, federated), "gc ready")
	}
	if total != perQuery {
		t.Fatalf("a %d-store hook tick issues %d city-wide `gc ready` reads, want %d (one query's worth): the federated reader already covers every leg, so the extra %d re-open every store for the same answer",
			len(scoped), total, perQuery, total-perQuery)
	}

	// The extras are scoped, not dropped: the crash-recovery and ephemeral tiers
	// are still per-store `bd` reads the city-wide reader does not answer, so a
	// blanket collapse would silently strip rig coverage from crash recovery.
	for _, st := range scoped[1:] {
		cmd := hookStoreCommand(st, federated)
		if !strings.Contains(cmd, `bd list --status in_progress`) {
			t.Fatalf("federated extra store %q lost the per-store crash-recovery tier: %q", st.dir, cmd)
		}
		if !strings.Contains(cmd, `ephemeral=true AND status=in_progress`) {
			t.Fatalf("federated extra store %q lost the per-store ephemeral tier: %q", st.dir, cmd)
		}
	}
	if got := hookStoreCommand(scoped[0], federated); got != federated {
		t.Fatalf("the primary store no longer runs the federated query; the city-wide read has to happen somewhere")
	}
}

// TestSingleStoreHookStoresKeepOneSharedCommand is the byte-identity half: a
// city that relocates nothing builds the same command for both topologies, so
// the scoping is a no-op and every store runs the one command it always ran.
func TestSingleStoreHookStoresKeepOneSharedCommand(t *testing.T) {
	a := &config.Agent{Name: "worker"}
	singleStoreTopo := config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}
	command := a.EffectiveWorkQueryFor(singleStoreTopo)

	stores := fiveRigHookStores()
	scoped := scopeFederatedHookStores(stores, command, command)
	if len(scoped) != len(stores) {
		t.Fatalf("scoping dropped stores on a single-store city: %d, want %d", len(scoped), len(stores))
	}
	for i, st := range scoped {
		if st.command != "" {
			t.Fatalf("store %d carries a per-store command on a single-store city (%q); every entry must run the one command it always ran", i, st.command)
		}
		if got := hookStoreCommand(st, command); got != command {
			t.Fatalf("store %d runs %q, want the shared command", i, got)
		}
	}
}

// TestSingleStoreHookWorkQueryIsEmptyOffASplitCity pins the caller-side guard:
// the scoping input is only built where there is something to deduplicate.
func TestSingleStoreHookWorkQueryIsEmptyOffASplitCity(t *testing.T) {
	cfg := &config.City{}
	a := &config.Agent{Name: "worker"}
	singleStoreTopo := config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}
	if got := singleStoreHookWorkQuery("/city", "city", cfg, a, singleStoreTopo, nil); got != "" {
		t.Fatalf("singleStoreHookWorkQuery on an unfederated city = %q, want \"\"", got)
	}
	custom := &config.Agent{Name: "custom", WorkQuery: "bd ready --json"}
	if got := singleStoreHookWorkQuery("/city", "city", cfg, custom, federatedHookTopology(), nil); got != "" {
		t.Fatalf("singleStoreHookWorkQuery for a verbatim custom work_query = %q, want \"\" (both topologies build the same string)", got)
	}
	if got := singleStoreHookWorkQuery("/city", "city", cfg, a, federatedHookTopology(), nil); got == "" {
		t.Fatal("singleStoreHookWorkQuery on a split city returned nothing to scope the federated extras with")
	}
}

// TestBestStoreWithWorkRunsEachStoresOwnCommand pins the seam the scoping rides
// on: selection must run the command the store carries, not the shared one.
func TestBestStoreWithWorkRunsEachStoresOwnCommand(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}, command: "single-store query"},
	}
	ran := map[string]string{}
	run := func(command, dir string, _ []string) (string, error) {
		ran[dir] = command
		return `[]`, nil
	}
	if _, _, err := bestStoreWithWork("federated query", stores, stores[0], run); err != nil {
		t.Fatalf("bestStoreWithWork: %v", err)
	}
	if ran["city"] != "federated query" {
		t.Fatalf("primary store ran %q, want the shared federated query", ran["city"])
	}
	if ran["riga"] != "single-store query" {
		t.Fatalf("federated extra ran %q, want its own single-store query", ran["riga"])
	}
}

// TestClaimStoreWithFallbackRunsTheSelectedStoresOwnCommand covers the claim
// half of the same seam: claim-time re-validation must re-ask the selected store
// its OWN question, or the scoped extras would be re-probed city-wide.
func TestClaimStoreWithFallbackRunsTheSelectedStoresOwnCommand(t *testing.T) {
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}, command: "single-store query"},
	}
	var revalidated string
	run := func(command, dir string, _ []string) (string, error) {
		if dir == "riga" {
			revalidated = command
			return `[{"id":"hw-riga","status":"open"}]`, nil
		}
		return `[]`, nil
	}
	if _, store, err := claimStoreWithFallback("federated query", stores, stores[1], stores[0], run); err != nil || store.dir != "riga" {
		t.Fatalf("claimStoreWithFallback = (%q, %v), want riga", store.dir, err)
	}
	if revalidated != "single-store query" {
		t.Fatalf("claim-time re-validation ran %q, want the selected store's own single-store query", revalidated)
	}
}
