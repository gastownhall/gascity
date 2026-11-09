package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// reviewedStoreIdentitySites are this package's store-identity comparisons
// and store-keyed maps (typed-stores T7 plan, OD-T7-15, R2), keyed "file func
// rule", as the scanner found them when the guard was seeded (T7g). Each
// compares or keys raw stores today, which is why it answers correctly; a
// class handle, a leg or a resident handle is never == the store it wraps,
// so each is rewritten to beads.SameStore or beads.PhysicalStoreKey by the
// change that hands it one. The table only shrinks: a new comparison uses
// SameStore, and a new store-keyed map PhysicalStoreKey.
//
// Rows whose reason starts "not a store" are the scanner reading a string
// field named Store (a diagnostic's backend name, a config binding name, a
// work-started key) as a store; they stay until the scanner tells them apart.
var reviewedStoreIdentitySites = map[string]beadstest.IdentitySite{
	"allocator_backstop_lane.go externalReadLegs map-key":                   {Count: 1, Reason: "dedupes the raw external read legs by store value; T9"},
	"api_state.go controllerState.update eq":                                {Count: 1, Reason: "the reloaded raw city store != the one it replaces, to close the old handle; T9 (raw holder fields)"},
	"build_desired_state.go  map-key":                                       {Count: 2, Reason: "readyDemandCache's live and cached maps, keyed by the raw demand-leg store; T9"},
	"build_desired_state.go buildDemandTargets eq":                          {Count: 3, Reason: "a control dispatcher's own raw target store vs the demand leg's raw store; T9"},
	"build_desired_state.go convergedRoutedWorkBinding eq":                  {Count: 1, Reason: "a raw demand leg != the raw routes binding; T9"},
	"build_desired_state.go newTracedReadyDemandCache map-key":              {Count: 2, Reason: "makes readyDemandCache's raw-store-keyed maps; T9"},
	"build_desired_state.go readyDemandCache.entry map-key":                 {Count: 1, Reason: "looks up readyDemandCache's raw-store-keyed map; T9"},
	"by_id_store_route.go classRoutedStoreForIDIn eq":                       {Count: 1, Reason: "the routes' raw class store == the raw work store (nothing relocated); T7h by-id residency"},
	"city_runtime.go sweepOrphanedOrderTrackingAtBoot eq":                   {Count: 1, Reason: "the raw orders store != the raw work store at boot; T7h order sweeps"},
	"city_runtime_v2.go sameClassStore eq":                                  {Count: 1, Reason: "v2's own == over raw stores; T9"},
	"city_status_snapshot.go diagnosticPtr eq":                              {Count: 1, Reason: "not a store: BeadsDiagnostic.Store is the backend name"},
	"city_stores.go storageRoutes.mintBindings map-key":                     {Count: 1, Reason: "the router groups the routes' raw class stores into bindings (allowlisted mint file); stays raw"},
	"class_flip_guard.go routesClassHolds eq":                               {Count: 1, Reason: "the routes' raw class store == a raw store (S2's config-based holds twin); T8"},
	"class_store_cache.go storageRoutes.namespacesServedBy eq":              {Count: 1, Reason: "the routes' raw class store == the raw binding engine (allowlisted cache construction); stays raw"},
	"class_store_cache.go storageRoutes.withControllerCache map-key":        {Count: 1, Reason: "one CachingStore per raw binding engine (allowlisted cache construction); stays raw"},
	"class_store_emit.go storageRoutes.withCLIEmission map-key":             {Count: 1, Reason: "one emitting wrapper per raw binding engine, the CLI twin of withControllerCache; stays raw"},
	"cmd_convoy.go convoyStoreViewsWithBinding eq":                          {Count: 1, Reason: "a raw convoy store view == the raw binding; T9"},
	"cmd_convoy_dispatch.go controlBeadLedger eq":                           {Count: 1, Reason: "the raw primary store != the raw scope store, to pick the control bead's relic leg; T7h"},
	"cmd_formula.go newFormulaCookCmd eq":                                   {Count: 2, Reason: "the raw attach and placement stores != the raw scope store; T7h (cook attach)"},
	"cmd_hook_claim.go hookHeldStartedClaim eq":                             {Count: 1, Reason: "not a store: the work-started entry's Store is a store ref"},
	"convergence_store.go convergenceStoreAdapter.pourWisp eq":              {Count: 2, Reason: "the raw pour target == the adapter's raw store; T7h (convergence)"},
	"convergence_tick.go rigStoreIsCityStore eq":                            {Count: 1, Reason: "a raw rig store == the raw city store; T9"},
	"cross_store_gate_lane.go CityRuntime.releaseCrossStoreGates eq":        {Count: 1, Reason: "the raw graph store == the raw city store (early return); T9e: under handle identity the lane would run under the gate"},
	"cross_store_gate_lane.go crossStoreBlockerResolver eq":                 {Count: 4, Reason: "== dedupe of the raw copy legs (city, rigs, binding); T9e, not converted in T7"},
	"demand_cross_store_gate.go crossStoreGateLeg eq":                       {Count: 1, Reason: "the raw demand leg's label key == a key; T9"},
	"demand_trace.go  map-key":                                              {Count: 1, Reason: "demandPassTrace's labels map, keyed by the raw demand-leg store; T9"},
	"demand_trace.go newDemandPassTrace map-key":                            {Count: 1, Reason: "makes demandPassTrace's raw-store-keyed labels map; T9"},
	"doctor_cross_store.go danglingBlockingEdgesCheck.collect map-key":      {Count: 2, Reason: "records which raw stores hold each blocker id; T9"},
	"doctor_executor_identity_residue_check.go sameStoreEngine eq":          {Count: 1, Reason: "== of two raw stores' engines; T7h (session relic scans)"},
	"doctor_order_tracking_retention.go orderTrackingRetentionCheck.Run eq": {Count: 1, Reason: "the raw orders store != the raw scope store; T7h order sweeps"},
	"formula_cook_attach_class.go attachGraftClassRefusal eq":               {Count: 2, Reason: "the raw class and attach stores == the raw scope store; T7h (cook attach)"},
	"graph_placement.go graphResidenceForTree eq":                           {Count: 1, Reason: "the raw relic leg == the raw placement; T7h (graph relic rule)"},
	"hook_session_claim.go decideWorkStarted eq":                            {Count: 1, Reason: "not a store: the work-started key's Store is a store ref"},
	"infra_class_migrate.go resolveInfraBindingTarget eq":                   {Count: 1, Reason: "not a store: storage.Classes.Work is a binding name"},
	"infra_class_recover_held.go applyInfraHeldChoices eq":                  {Count: 1, Reason: "not a store: the recovery record's Store is a choice name"},
	"op_sweep.go sameBeadStore eq":                                          {Count: 1, Reason: "the package's raw == helper, used by every sameBeadStore caller; goes when its last caller takes SameStore"},
	"order_dispatch.go storeListContains eq":                                {Count: 1, Reason: "== membership over gateStoresFor's raw legs; T7f/T7h (OrderRuns)"},
	"order_dispatch.go sweepStoreListContains eq":                           {Count: 1, Reason: "== membership over the tracking sweep's raw legs; T7h order sweeps"},
	"order_store.go appendOrdersClassHandle eq":                             {Count: 1, Reason: "== dedupe of the orders legs; T7h order sweeps"},
	"order_store.go appendOrdersClassStore eq":                              {Count: 1, Reason: "a leg's raw unwrap == the raw orders store; T7h order sweeps"},
	"order_store.go appendOrdersSweepStore eq":                              {Count: 1, Reason: "a sweep leg's raw unwrap == the raw orders store; T7h order sweeps"},
	"pool_detached_orphan_sweep.go detachedOrphanRoutesForLeg eq":           {Count: 1, Reason: "the raw route store != the raw leg store; T7h (session relic scans)"},
	"ready_federation.go readyLegReservedPrefixes eq":                       {Count: 1, Reason: "the topology binding's raw leg == the plan's raw leg; T7f (readywork legs)"},
	"ready_federation.go relocatedGraphLegFrom eq":                          {Count: 1, Reason: "the raw binding == the raw city store; T7f (readywork legs)"},
	"residency_topology.go residencyBindingsFor map-key":                    {Count: 1, Reason: "groups the routes' raw class stores into bindings; T9 (the plane topologies)"},
	"residency_topology.go residencyBindingsFromRoutesWithProof map-key":    {Count: 1, Reason: "the grouping map residencyBindingsFor reads; T9 (the plane topologies)"},
	"seat_parked.go liveParkedWork eq":                                      {Count: 1, Reason: "== of two raw seat-work leg stores; T9"},
	"seat_work.go SeatWork.stores eq":                                       {Count: 1, Reason: "== dedupe of raw seat-work hit stores; T9"},
	"seat_work_close.go appendUniqueSeatWork eq":                            {Count: 1, Reason: "== dedupe of raw seat-work leg stores; T9"},
	"storage_boot.go  map-key":                                              {Count: 1, Reason: "storageRoutes.relics, keyed by the raw binding engine (allowlisted boot file); stays raw"},
	"storage_boot.go censusBindingRelics map-key":                           {Count: 1, Reason: "fills storageRoutes.relics by raw binding engine (allowlisted boot file); stays raw"},
	"storage_boot.go storageRoutes.distinctEngines map-key":                 {Count: 1, Reason: "dedupes the raw binding engines (allowlisted boot file); stays raw"},
	"storage_boot.go storageSplitShapeOf eq":                                {Count: 1, Reason: "not a store: storage.Classes.Work is a binding name"},
	"storage_maintenance.go migrationSourceLaneRefusal switch":              {Count: 1, Reason: "not a store: BeadsDiagnostic.Store is the backend name"},
	"storage_preflight_split.go splitClearLaneFor eq":                       {Count: 4, Reason: "not a store: BeadsDiagnostic.Store is the backend name"},
	"storage_preflight_split.go splitPreflightEstimate eq":                  {Count: 1, Reason: "not a store: BeadsDiagnostic.Store is the backend name"},
	"wisp_autoclose.go collectInputConvoyWorkflowRoots eq":                  {Count: 1, Reason: "the raw graph store != the raw work store (the two-store convoy probe); T7g (RoutedWork over a rig-less frame)"},
	"wisp_step_inject.go sameStoreHandle eq":                                {Count: 1, Reason: "the step reminder's one-store test over raw stores; T7g (MoleculeActive)"},
	"work_location.go WorkLegs.isWork eq":                                   {Count: 1, Reason: "the raw unwraps of a store and the work leg; T9"},
}

// TestStoreIdentityGuard holds cmd/gc's store comparisons to the reviewed
// table (OD-T7-15).
func TestStoreIdentityGuard(t *testing.T) {
	beadstest.CheckStoreIdentity(t, ".", reviewedStoreIdentitySites)
}
