package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// frontDoorStoreFreeFiles are the cmd/gc source files whose every function was
// converted to take a dependency-injected typed front door
// (*session.Store / *orders.Store / *nudgequeue.Store) in place of a raw
// bead store. They must never regress to holding a raw store: with no
// beads.Store in scope, a raw bead op on a non-work object (a session
// state-heal, a circuit-breaker metadata write, …) is *untypeable* rather than
// merely absent — the compile-time half of the object-model front-door boundary.
//
// Only files that are ENTIRELY store-free belong here. Mixed/root files
// (session_reconciler.go, cmd_nudge.go, order_dispatch.go, …) legitimately keep
// a raw store for their work/by-id/federation/graph residual and construct the
// front door inline from it — that is the front door being used, not a leak —
// so they are intentionally not listed. Add a file here once all of its
// functions take the injected front door.
var frontDoorStoreFreeFiles = []string{
	"session_circuit_breaker.go",
	"soft_reload.go",
	"adoption_barrier.go",
	"session_index.go",
	"mcp_integration.go",
	"skill_visibility.go",
	"session_logs_resolve.go",
}

// frontDoorForbiddenInStoreFreeFiles are the raw-store parameter types and the
// inline front-door constructors that must not reappear in a store-free file. A
// store-free file receives its front door already constructed at a composition
// root and threaded in.
var frontDoorForbiddenInStoreFreeFiles = []string{
	"beads.Store",
	"beads.SessionStore",
	"beads.OrdersStore",
	"beads.NudgesStore",
	"sessionFrontDoor(",
	"orders.NewStore(",
	"nudgeFrontDoor(",
	"workAssignment{",
}

// TestFrontDoorStoreFreeFilesStayStoreFree pins the front-door dependency-injection
// boundary: the fully-converted files must never reintroduce a raw store —
// neither as a parameter type nor by constructing a front door inline. Mirrors
// TestGCNonTestFilesStayOnWorkerBoundary.
func TestFrontDoorStoreFreeFilesStayStoreFree(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range frontDoorStoreFreeFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range frontDoorForbiddenInStoreFreeFiles {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw-store/front-door-construction pattern %q — this file is dependency-injection store-free; receive the typed front door (*session.Store / *orders.Store / *nudgequeue.Store) as a parameter instead of holding a raw store", name, needle)
			}
		}
	}
}

// snapshotInfoOnlyFiles are the cmd/gc source files whose every session-bead
// snapshot read was converted to the typed session.Info front door
// (snapshot.OpenInfos() / FindInfoBy*) by the non-work field-door cleanup.
// They must never regress to the raw-bead accessors: a raw session bead escaping the
// snapshot is exactly the leak this migration closes — the field would then be
// read straight off bead metadata instead of through the one codec edge.
//
// Add a file here once it calls NONE of the raw snapshot accessors below — i.e.
// every session bead it consumes from the snapshot arrives as a session.Info.
// Files still mid-conversion (build_desired_state.go, city_runtime.go,
// session_reconciler.go, the pool-demand cascade, …) are intentionally absent.
var snapshotInfoOnlyFiles = []string{
	"template_resolve.go",
	"session_name_lookup.go",
	"cmd_citystatus.go",
	"city_status_snapshot.go",
	"session_reconciler_trace_cycle.go",
	"providers.go",
	"nudge_dispatcher.go",
	"named_sessions.go",
	"soft_reload.go",
}

// forbiddenRawSnapshotAccessors are the *sessionBeadSnapshot methods that return
// a raw beads.Bead (or []beads.Bead). The typed mirrors OpenInfos()/FindInfoByID/
// FindInfoByTemplate/FindInfoByNamedIdentity do not contain these substrings, so
// a converted file matching one of these has reintroduced a raw session-bead read.
// The typed mirrors read openInfos + the index maps (not the raw open slice), so
// they return correct results on BOTH a bead-built snapshot and an Info-built one
// (newSessionBeadSnapshotFromInfos leaves open nil); the raw accessors above
// return empty on an Info-built snapshot, which is why they are forbidden here.
var forbiddenRawSnapshotAccessors = []string{
	".Open()",
	".FindByID(",
	".FindSessionBeadByTemplate(",
	".FindSessionBeadByNamedIdentity(",
}

// TestSnapshotInfoOnlyFilesStayOnInfoAccessors pins the read half of the
// non-work field-door boundary: the converted snapshot consumers must keep
// reading session beads through session.Info (OpenInfos/FindInfo*), never the
// raw-bead accessors. Mirrors TestFrontDoorStoreFreeFilesStayStoreFree for the
// read surface.
func TestSnapshotInfoOnlyFilesStayOnInfoAccessors(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range snapshotInfoOnlyFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range forbiddenRawSnapshotAccessors {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw snapshot accessor %q — this file was converted to the session.Info front door; read session beads via snapshot.OpenInfos()/FindInfoByID/FindInfoByTemplate/FindInfoByNamedIdentity instead of the raw-bead accessor", name, needle)
			}
		}
	}
}

// metadataInfoOnlyFiles are the files whose session beads are read AND written
// exclusively through the typed session.Info projection (infoByID /
// InfoFromPersistedBead) / session.CircuitState — never by cracking bead
// metadata off a raw bead. This is the SHAPE half of the object-model front-door
// boundary: the reconciler decision-path files completed by the lockstep drop
// plus the session-class periphery files converted by the periphery closure.
// Once a file routes every session field through the typed projection, a reappearing
// `.Metadata[...]` bead crack is a regression to raw-bead reads.
//
// SHAPE-SEALED IS NOT RELOCATION-SAFE. Membership here means field reads go
// through the Info codec (backend-shape-invariant); it does NOT mean the bead
// LOAD is routed through the session-class store (sessionsBeadStore() /
// resolveSessionStore). That access half is the separate frontDoorStoreFreeFiles
// boundary; a [beads.classes.sessions] relocation captures a file only once BOTH
// halves close. Several files here (e.g. cmd_prime.go, session_template_start.go)
// still load their session bead from a raw store and are shape-sealed only.
//
// Only files that crack NO bead metadata inline (session OR work) belong here —
// each listed file currently contains zero `.Metadata[` of any receiver spelling,
// so the guard forbids the whole family (session.Metadata[, target.session.Metadata[,
// b.Metadata[, bead.Metadata[) with no false positive.
//
// session_reconciler.go and session_reconcile.go are intentionally ABSENT and
// CANNOT be added with a file-level substring guard: they retain a bounded,
// DOCUMENTED raw-by-design census — the raw classifier helpers that take a
// `session beads.Bead` parameter (the oracle-verified siblings of the typed Info
// classifiers, kept for TestSessionClassifierInfoEquivalence and boundary
// projections) plus the start-execution / cross-tick emit-once coupled survivor
// mirrors (S1-S5 in the lockstep-drop census) — which a substring needle cannot
// distinguish from a new decision-path leak. Their protection is the in-code
// census comments plus the LOCKSTEP-DROP census. session_sleep.go /
// session_wake.go / session_lifecycle_parallel.go / session_bead_snapshot.go are
// likewise raw-by-design (sleep-policy helpers, start execution, the bead
// constructor) and stay off this list.
var metadataInfoOnlyFiles = []string{
	"compute_awake_bridge.go",
	"session_progress.go",
	"session_circuit_breaker.go",
	"city_status_snapshot.go",
	"session_template_start.go",
	"adoption_barrier.go",
	"cmd_prime.go",
	"cmd_skill.go",
	"session_resolve.go",
	"cmd_session_logs.go",
	"mcp_integration.go",
	"session_index.go",
	"cmd_session_wake.go",
	"soft_reload.go",
	"cmd_session.go",
}

// forbiddenRawBeadMetadata is the raw bead-metadata crack this guard forbids.
// The needle `.Metadata[` matches every receiver spelling (session.Metadata[,
// target.session.Metadata[, b.Metadata[, bead.Metadata[, item.bead.Metadata[).
// The listed files are Info/CircuitState-only decision helpers that read no bead
// metadata at all, so the broad needle is exact for them and catches the dominant
// `b.Metadata[` / `bead.Metadata[` leak spelling that a `session.`-anchored needle
// would miss.
var forbiddenRawBeadMetadata = []string{
	".Metadata[",
}

// TestMetadataInfoOnlyFilesStayOnInfoSnapshot pins the write+read half of the
// reconciler front-door boundary: the fully-converted decision-path files must
// keep routing every session field through the typed Info/CircuitState
// projection, never cracking bead metadata off a raw bead. Mirrors
// TestSnapshotInfoOnlyFilesStayOnInfoAccessors for the metadata surface.
func TestMetadataInfoOnlyFilesStayOnInfoSnapshot(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range metadataInfoOnlyFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range forbiddenRawBeadMetadata {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains forbidden raw bead-metadata crack %q — this file was converted to the typed session.Info / CircuitState projection; read and write session fields through the typed accessor (info.<Field> / infoByID / ApplyPatch / CircuitState) instead of cracking the raw bead", name, needle)
			}
		}
	}
}

// sessionRelocationRoutedFiles are the CLI one-shot ROOT files whose SESSION-class
// bead access (session lifecycle beads AND durable gc:wait beads — both
// coordclass.ClassSessions) was routed through the session coordination-class
// store via cliSessionStore / cliSessionFrontDoor (→ resolveSessionStore). A
// one-shot CLI command opens the generic city (work) store from openCityStore*;
// left unrouted, its session writes would land in the work store instead of the
// relocated session backend once [beads.classes.sessions] moves — the split-brain
// this migration closes. These files must never regress to
// constructing the session front door straight from the generic work store.
//
// providers.go is a shared provider-construction helper rather than a one-shot
// command root, but it is listed because its loadProviderSessionSnapshot session
// read — the gc:session ListByLabel, off a store it opens itself independent of
// the caller — is routed. It carries no `sessionFrontDoor(store...)` construction,
// so there is no substring false-positive risk and the positive `cliSessionStore(`
// tripwire protects that route the same way it protects the command roots.
// providers.go carries a SECOND routed session path: openCityMailProvider builds
// the CLI mail provider as a two-store beadmail (message persistence on the
// messaging-class store, mail's session reads/writes — session.ListAllSessionBeads
// / ResolveSessionID / RepairEmptyType — on the session-class store via
// cliSessionStore), so the mail addressing/identity session access a
// [beads.classes.sessions] relocation must capture is routed here too (the former
// two-store mail-provider follow-up, now closed for the session class). Both the
// snapshot route and the mail route carry the positive cliSessionStore( tripwire
// and no sessionFrontDoor(store...) construction; the end-to-end acceptance test
// remains the authoritative completeness check.
//
// The gc status trio (cmd_status.go, cmd_citystatus.go, city_status_snapshot.go)
// is listed even though none of them constructs a session front door: their
// session access is non-front-door (loadStatusSessionSnapshot → ListAllSessionBeads,
// namedSessionStatusForCity → resolveSessionIDWithConfig + store.Get,
// collectCitySessionCounts → workerSessionCatalogWithConfig), all routed through
// cliSessionStore, while the store-health probe (buildCityStoreHealth →
// collectStoreHealth → store.List) deliberately stays on the generic work store it
// measures. The negative sessionFrontDoor(store...) needles are inert for these
// files; the positive cliSessionStore( tripwire protects the routed reads the same
// way it protects providers.go's snapshot route, consistent with this guard being
// a regression canary rather than a completeness proof.
//
// controller.go is intentionally ABSENT even though it routes: its
// session-circuit-reset socket handler routes, but the file also holds the
// already-safe param-threaded runtime `sessionFrontDoor(store.Store)` at the
// gracefulStop path, which the substring needle cannot distinguish. It is
// protected by in-code comments and the end-to-end relocation acceptance test
// rather than this file-level guard.
//
// cmd_start.go's standalone reconcile cascade routes its full SESSION arm to
// mirror the daemon's store-role split (CityRuntime.buildDesiredState /
// controlDispatcherTick): the leading store of buildDesiredStateWithSessionBeads,
// loadSessionBeadSnapshot, syncSessionBeadsWithSnapshotAndRigStores, and
// reconcileSessionBeadsAtPathWithNamedDemand all take the session-class store via
// cliSessionStore/cliSessionFrontDoor, with rigStores as the per-rig WORK tail. The
// one residual — releaseOrphanedPoolAssignmentsWhenSnapshotsComplete's
// liveOpenSessionAssignmentExists session read — stays on the plain store exactly as
// the daemon leaves it on cityBeadStore(), a shared work-release-boundary follow-up,
// not a cmd_start gap. The positive cliSessionStore( tripwire protects the routed
// arm; no unrouted sessionFrontDoor(store...) needle is carried. As a non-front-door
// router (its session reads go through store args, not front-door construction) this
// guard is a regression canary for the file, not a completeness proof — the
// end-to-end relocation acceptance test remains authoritative.
//
// cmd_sling.go routes its sling-nudge session access (doSlingNudge session
// lookups, deliverSlingNudge's observe/handle/last-nudge stamp via
// cliSessionStore/cliSessionFrontDoor derived from the target's cfg+cityPath, and
// printNudgePreview) while the queued-nudge enqueue stays on the plain store
// (nudges class, its own E1.2 routing). TWO sling-root session sites remain
// DEFERRED because their store is threaded cross-package and cannot be routed at
// the CLI root: (a) cliDirectSessionResolver (populateSlingDepsCallbacks →
// internal/sling → internal/graphroute → resolveSessionIDMaterializingNamed)
// materializes session beads on the WORK source store and needs a two-store
// SlingDeps/graphroute.Deps split (a session-class store added to the deps); and
// (b) the resolveGraphStepBinding* helpers DEFINED here read session state on a
// store supplied by their cmd_convoy_dispatch.go caller (never called from a sling
// root), owned by that caller's effort. So the positive cliSessionStore( tripwire
// protects the routed sling-nudge arm but NOT those two deferred cross-package
// sites — consistent with this guard being a regression canary, not a completeness
// proof.
//
// cmd_handoff.go routes its full SESSION arm — restartability
// (sessionRestartableByController), restart-request clear (clearRestartRequest ->
// sessionFrontDoor(sessStore).ApplyPatch), restart persist (sessionRestartPersister),
// the remote kill/observe/identity trio, resolveSessionID, sender-identity resolution
// (resolveDefaultMailSenderForCommand), and beadmail's session addressing via
// beadmail.NewWithStores(store, sessStore) — through cliSessionStore, while
// createHandoffMail's message-bead persistence deliberately stays on the plain store
// (messaging class, its own slice, mirroring newCityMailProvider's msgStore/sessStore
// split and cmd_sling.go's deferred nudge enqueue). cmd_runtime_drain.go routes only
// cmdRuntimeRequestRestart's session-bead access; every other command in the file is
// drainOps runtime metadata (no bead store) plus nil-store worker observation, so the
// guard is a regression canary for that one root.
//
// cmd_wait.go additionally hosts the controller-shared wait machinery, so its
// session-class store params are named sessStore (making sessionFrontDoor(sessStore)
// and sessionFrontDoor(sessStore.Store) the sanctioned in-file forms). Its four CLI
// roots — cmdSessionWait, cmdWaitSetStateResult, doWaitListFallback,
// doWaitInspectFallback — derive sessStore := cliSessionStore(store, cfg, cityPath)
// and route the SESSION/wait bead access (wait-bead CRUD, session-bead lookups,
// wait_hold clears, cap-diagnostic stamps) through it, while dependency-bead reads
// (loadWaitDependencyBead, injected into doSessionWait's waitDependencyReader)
// deliberately stay on the plain WORK store (dep beads are work class, federated
// across rig scopes), and wait-nudge shadow lookups ride a NudgesStore over the same
// work store (nudges class,
// its own E1.2 routing). The positive cliSessionStore( tripwire protects the routed
// arm; as a non-front-door router (most session reads go through store args) this
// guard is a regression canary for the file, not a completeness proof — the
// end-to-end relocation acceptance test remains authoritative.
//
// cmd_nudge.go routes its delivery-tree SESSION arm via a two-store split derived
// from the nudge target's cfg+cityPath (the deliverSlingNudge precedent): the raw
// openNudgeBeadStore store keeps threading the nudge-queue currency, while each
// delivery helper derives sessStore := cliSessionStore(store, target.cfg,
// target.cityPath) and routes the session-class ops — worker observe/handle
// (nudgeObserveTarget / workerObserveNudgeTarget / workerHandleForNudgeTarget), the
// managed wake (session.WakeSession via requestManagedNudgeWake), named-session
// materialization (resolveSessionIDMaterializingNamed) and its Get, the last-nudge
// stamp (sessionFrontDoor(sessStore/deliverySessStore)), and the wait-bead reads in
// splitQueuedNudgesForDelivery — through it. tryDeliverQueuedNudgesByPoller and
// dispatchAllQueuedNudges take an explicit sessStore param that the CALLER resolves:
// the controller threads cr.sessionsBeadStore().Store, whose fallback is the WORK
// store (NOT the nudges store) — this closed the live controller-dispatcher class-mix
// where the session store was derived from cr.nudgesBeadStore().Store. The CLI
// poll/drain paths derive sessStore from their openNudgeBeadStore base, which is
// identity to the work store today (nil cfg) but is nudges-class-routed; a tracked
// E1.2 follow-up must thread a genuine work store there once openNudgeBeadStore
// relocates, or the CLI-path sessions fallback would re-land on the nudge DB.
// deliveryStore stays the nudges store for the queue record/dead-letter. The nudge-queue roots deliberately
// stay on openNudgeBeadStore — enqueue/ack/release/record/terminalize/rollback and
// nudgeFrontDoor are nudges class (their own E1.2 routing), mirroring cmd_sling.go's
// deferred nudge enqueue. nudge_dispatcher.go is a pure RECEIVER of that session store
// (routed at the controller boundary in city_runtime.go, a mixed file off this list),
// so it is intentionally absent from the routed-files list. The positive cliSessionStore(
// tripwire protects the routed delivery arm; as a non-front-door router this guard is
// a regression canary for the file, not a completeness proof.
var sessionRelocationRoutedFiles = []string{
	"cmd_session_wake.go",
	"cmd_session_pin.go",
	"cmd_skill.go",
	"cmd_mcp.go",
	"cmd_session_logs.go",
	"cmd_prime.go",
	"cmd_stop.go",
	"cmd_session_reset.go",
	"cmd_restart.go",
	"completion.go",
	"providers.go",
	"cmd_session.go",
	"cmd_status.go",
	"cmd_citystatus.go",
	"city_status_snapshot.go",
	"cmd_start.go",
	"cmd_sling.go",
	"cmd_handoff.go",
	"cmd_runtime_drain.go",
	"cmd_runtime_heartbeat.go",
	"cmd_wait.go",
	"cmd_nudge.go",
}

// sessionRelocationForbidden are the UNROUTED session-front-door constructions a
// routed CLI root must never contain. The routed form cliSessionFrontDoor( does
// not contain the lowercase-s substring "sessionFrontDoor(", and the
// sessionFrontDoor(cliSessionStore(...)) wrap lives only in cli_session_store.go
// (which is off this list), so the needles below fire only on a regression to the
// generic-store form. A sessionFrontDoor(sessStore) call (a routed local already
// in hand) is a distinct substring and is allowed.
var sessionRelocationForbidden = []string{
	"sessionFrontDoor(store)",
	"sessionFrontDoor(store.Store)",
	"sessionFrontDoor(openCityStore",
	"sessionFrontDoor(deliveryStore.Store)",
	"sessionFrontDoor(deliveryStore)",
}

// TestSessionRelocationRootsRouteThroughSessionClassStore pins the CLI relocation
// boundary: the one-shot command roots must construct their session front door
// through the session coordination-class store (cliSessionStore /
// cliSessionFrontDoor), never straight from the generic work store, so a
// [beads.classes.sessions] relocation reaches them. It is a regression canary:
// a substring guard cannot prove every non-front-door session read (store.Get,
// resolveSessionID*) was routed — the end-to-end relocation acceptance test is the
// authoritative check. Mirrors TestFrontDoorStoreFreeFilesStayStoreFree.
func TestSessionRelocationRootsRouteThroughSessionClassStore(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	for _, name := range sessionRelocationRoutedFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		content := string(data)
		for _, needle := range sessionRelocationForbidden {
			if strings.Contains(content, needle) {
				t.Errorf("%s contains unrouted session front door %q — this CLI root must route its session-class access through cliSessionFrontDoor(store, cfg, cityPath) / cliSessionStore(...) so a [beads.classes.sessions] relocation reaches it", name, needle)
			}
		}
		if !strings.Contains(content, "cliSessionStore(") && !strings.Contains(content, "cliSessionFrontDoor(") {
			t.Errorf("%s is listed as session-relocation-routed but never calls cliSessionStore( / cliSessionFrontDoor( — did the routing get dropped?", name)
		}
	}
}
