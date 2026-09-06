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

// TestFederatedHookStoresIssueTheCityWideReaderOnce is the cost measurement, and
// after S3 it is a leg-count measurement too.
//
// `gc ready` federates the city store, every bound rig store and the relocated
// binding in ONE call — the legs are Plan(RoutedWork)'s — so its answer is a
// SUPERSET of every extra leg's, for every tier of the generated query. Running
// the extras therefore cannot change the selection; it only re-opens every store
// and pays the federated legs again. The fan-out collapses to the one leg that
// answers for the plan.
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

	stores := fiveRigHookStores()
	scoped := scopeFederatedHookStores(stores, federated, singleStore)
	if len(scoped) != 1 {
		t.Fatalf("a %d-store federated hook tick still fans out to %d legs, want 1: the primary's reader already answers for every leg of the plan, so each extra is a whole extra work-query run for a strictly smaller view",
			len(stores), len(scoped))
	}
	if !sameHookStore(scoped[0], stores[0]) {
		t.Fatalf("the surviving leg is %q, want the agent's own (primary) store %q", scoped[0].dir, stores[0].dir)
	}
	total := 0
	for _, st := range scoped {
		total += strings.Count(hookStoreCommand(st, federated), "gc ready")
	}
	if total != perQuery {
		t.Fatalf("a %d-store hook tick issues %d city-wide `gc ready` reads, want %d (one query's worth)", len(stores), total, perQuery)
	}
	if got := hookStoreCommand(scoped[0], federated); got != federated {
		t.Fatalf("the surviving store no longer runs the federated query; the city-wide read has to happen somewhere")
	}
}

// The other half of the collapse, stated as the property it rests on: every tier
// of the SINGLE-STORE command an extra leg used to run is answered by the
// primary's federated command.
//
// The two tiers this was not always true of are named explicitly, because they
// are the reason the extras existed. Both are closed now: the crash-recovery
// tier swapped from `bd list --status in_progress` to `gc ready --status
// in_progress` (federated), and the ephemeral `bd query` probes are covered
// because every federated leg is read at beads.FederatedReadTier, which spans
// the wisp tier. If either regresses to a single-store read, the extras have to
// come back and this test says so.
func TestTheFederatedReaderAnswersEveryTierTheExtrasUsedTo(t *testing.T) {
	a := &config.Agent{Name: "worker"}
	topo := federatedHookTopology()
	federated := a.EffectiveWorkQueryFor(topo)

	if strings.Contains(federated, bdListInProgressForTest) {
		t.Fatalf("the federated work query still runs a SINGLE-STORE crash-recovery read (%q); an extra leg is the only thing that covered the other stores for it, and the fan-out no longer runs one: %q",
			bdListInProgressForTest, federated)
	}
	if !strings.Contains(federated, "gc ready --status in_progress") {
		t.Fatalf("the federated work query has no federated crash-recovery tier: %q", federated)
	}
	// The control: the SINGLE-STORE form does carry the reads this asserts are
	// gone, so the assertions above are about the federated form and not about a
	// query that lost its tiers entirely.
	singleStoreTopo := topo
	singleStoreTopo.FederatedReady = false
	if !strings.Contains(a.EffectiveWorkQueryFor(singleStoreTopo), bdListInProgressForTest) {
		t.Fatal("the single-store work query has no crash-recovery tier either; this test is comparing two empty things")
	}
}

// bdListInProgressForTest is the single-store crash-recovery reader, spelled
// here so the assertion above reads as the string an operator would grep for.
const bdListInProgressForTest = "bd list --status in_progress"

// TestFederatedClaimTickIssuesOneWorkQueryRun is the latency assertion for
// ga-4qdfn, stated in the unit the incident is measured in: how many work-query
// RUNS one `gc hook --claim` performs.
//
// On mc a run of the federated work query is several `gc ready` invocations,
// each paying the whole plan's legs (a remote-postgres city leg, five rig legs,
// the binding). Before S3 a six-leg city cost six runs to select plus one to
// re-validate. The plan says the primary's reader answers for every one of those
// legs, so the relevant leg count is one — and with one leg there is no later
// store to fall back to, so the re-validation has nothing to do either.
func TestFederatedClaimTickIssuesOneWorkQueryRun(t *testing.T) {
	a := &config.Agent{Name: "worker"}
	topo := federatedHookTopology()
	singleStoreTopo := topo
	singleStoreTopo.FederatedReady = false
	federated := a.EffectiveWorkQueryFor(topo)

	before := fiveRigHookStores()
	after := scopeFederatedHookStores(before, federated, a.EffectiveWorkQueryFor(singleStoreTopo))

	if got, want := hookClaimReadsPerTick(after), 1; got != want {
		t.Fatalf("a federated claim tick runs the work query %d times, want %d", got, want)
	}
	if got := hookClaimReadsPerTick(before); got <= hookClaimReadsPerTick(after) {
		t.Fatalf("the un-collapsed fan-out costs %d runs and the collapsed one %d; the measurement is not measuring anything", got, hookClaimReadsPerTick(after))
	}

	// And the claim really does act on the discovery output rather than paying
	// for a second read of it.
	one := after
	calls := 0
	run := func(string, string, []string) (string, error) {
		calls++
		return `[{"id":"gcg-1","status":"open"}]`, nil
	}
	out, store, err := claimStoreWithFallback(federated, one, one[0], one[0], `[{"id":"gcg-1","status":"open"}]`, run)
	if err != nil {
		t.Fatalf("claimStoreWithFallback: %v", err)
	}
	if calls != 0 {
		t.Fatalf("claim-time re-validation ran %d extra work queries on a single-leg city; there is no later store it could fall back to", calls)
	}
	if out != `[{"id":"gcg-1","status":"open"}]` || !sameHookStore(store, one[0]) {
		t.Fatalf("claimStoreWithFallback = (%q, %q), want the discovery output on the one leg", out, store.dir)
	}

	// The control: with a real second leg the re-validation still happens, so
	// the skip above is about having nowhere to fall back to and not about
	// having removed the freshness read.
	twoLegs := []hookStore{{dir: "city"}, {dir: "riga"}}
	calls = 0
	if _, _, err := claimStoreWithFallback("q", twoLegs, twoLegs[0], twoLegs[0], "stale", run); err != nil {
		t.Fatalf("claimStoreWithFallback (two legs): %v", err)
	}
	if calls != 1 {
		t.Fatalf("a two-leg claim performed %d re-validation reads, want 1", calls)
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
	if _, store, err := claimStoreWithFallback("federated query", stores, stores[1], stores[0], "", run); err != nil || store.dir != "riga" {
		t.Fatalf("claimStoreWithFallback = (%q, %v), want riga", store.dir, err)
	}
	if revalidated != "single-store query" {
		t.Fatalf("claim-time re-validation ran %q, want the selected store's own single-store query", revalidated)
	}
}
