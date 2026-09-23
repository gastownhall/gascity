package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The composition root of the proxied-native lane.
//
// Everything below this line is gc's half of the split store: which bd verbs
// admission may spend and what they mean, what environment the linked library is
// opened with, what happens when a read's reconnect finds the proxy replaced,
// and where the two halves are joined into a beads.ProxiedStore. The decisions —
// whether the scope may be served natively at all, what a refusal is called,
// when a handle stands down — belong to internal/beads and are not restated here.
//
// Two invariants hold across the whole file:
//
//   - gc never spawns dolt and never starts a proxy. The only lifecycle moves
//     available are the two provider verbs below, both of which ask BD to make
//     bd's proxy healthy.
//   - nothing here runs unless the persisted topology is proxied-server, the
//     rollout flag is on, and the factory decided to consult the opener. The
//     openers are built unconditionally at every composition root; they are inert
//     until all three hold.

const (
	// proxiedProviderProbeOp is the provider verb behind ProviderOps.Ping. On a
	// proxied scope the script's probe arm is exactly `bd ping`
	// (gc-beads-bd.sh), which adopts or restarts BD's proxy. gc spawns nothing.
	proxiedProviderProbeOp = "probe"
	// proxiedProviderRecoverOp is the verb behind ProviderOps.Recover: `bd dolt
	// stop` followed by `bd ping`, except for a scope that shares the CITY's
	// proxy root, where the script degrades to a ping alone rather than cycling
	// the one proxy serving hq and every other rig. Admission never spends it
	// for such a scope: the recover rung is the city scope's (round4 recheck
	// M2, beads.AdmissionInput.CityRoot).
	proxiedProviderRecoverOp = "recover"

	// proxiedOneShotAdmissionBudget bounds admission for a command a human is
	// waiting on. A one-shot that cannot be admitted quickly takes the bd front
	// door, which is the store it has today.
	proxiedOneShotAdmissionBudget = 10 * time.Second
	// proxiedLongLivedAdmissionBudget bounds admission for a store held for the
	// process lifetime. It is the drain ceiling plus room for the ladder: a
	// controller boot may legitimately wait out a proxy that is shutting down.
	proxiedLongLivedAdmissionBudget = 90 * time.Second

	// proxiedAdmissionPasses caps the outer ladder. The rungs themselves are
	// bounded by the generation sets (one ping and one recover per generation,
	// ever), so this only stops a record that keeps changing under us from
	// spinning.
	proxiedAdmissionPasses = 3
	// proxiedAdmissionBackoff spaces those passes.
	proxiedAdmissionBackoff = 200 * time.Millisecond
)

// The process-local escalation ledgers.
//
// They are package-level because "once per generation, per process" is the scope
// the design asks for: a gc command opens many scopes, several of which may
// share one bd proxy root, and a per-open set would turn "recover once" into
// "recover on every open". They are also what makes the readiness memo work —
// see proxiedProviderOps.noteReady.
var (
	proxiedObservedGenerations  = beads.NewGenerationSet()
	proxiedRecoveredGenerations = beads.NewGenerationSet()
)

// providerOwnedScopeLifecycleOp is the seam the proxied lane runs provider verbs
// through. Production is the real, semaphore-guarded, traced provider op; tests
// script it to assert WHICH verbs a ladder spent, which is the only way to prove
// "at most one ping and one recover per generation" without a bd binary. The
// precondition, when non-nil, is checked with the lifecycle slot held and
// before the verb runs (see runProviderOwnedScopeLifecycleOpGuarded).
var providerOwnedScopeLifecycleOp = runProviderOwnedScopeLifecycleOpGuarded

// proxiedProviderOps is the ENTIRE bd-verb surface admission may reach, bound to
// one city.
//
// Both verbs are provider-owned lifecycle ops gc already runs elsewhere (health
// passes, recovery sweeps), with the same semaphore, the same per-op budget and
// the same tracing. Neither spawns anything gc owns.
type proxiedProviderOps struct {
	cityPath string
	observed *beads.GenerationSet
}

// Ping asks bd to make the scope's proxy healthy.
//
// A failure bd itself reported is marked as such (beads.ProviderReportedFailure),
// because it is the evidence admission's no-greeting ladder escalates to a
// recover on, and a failure of gc's own is not. See markProviderReportedFailure.
func (o proxiedProviderOps) Ping(ctx context.Context, scopeRoot string) error {
	if err := providerOwnedScopeLifecycleOp(ctx, o.cityPath, scopeRoot, proxiedProviderProbeOp, nil); err != nil {
		return markProviderReportedFailure(err)
	}
	o.noteReady(scopeRoot)
	return nil
}

// providerOpExitNotNeeded is the provider script's "not needed / not mine"
// status (runProviderOp's convention). The strict runner reports it as a
// failure, but it is the script declining the op — GC_DOLT=skip, an op its
// arm does not handle — not bd answering about the proxy.
const providerOpExitNotNeeded = 2

// markProviderReportedFailure marks a provider-op failure as bd's own answer
// when, and only when, the script RAN and exited with a status of its own.
//
// On a proxied scope the probe op's arm is exactly `bd ping` and the recover
// op's is `bd dolt stop` then `bd ping`, so that status is bd's: the real
// zombie (a Dolt child that exited 0 behind a proxy that lives on) makes the
// ping exit 1, and that is the design's trigger for the recover rung; a
// recover bd refused the same way is the one recover failure that ends the
// lane (round4 recheck M1).
// Everything else stays unmarked, because none of it is an answer about the
// proxy: the lifecycle semaphore or the op budget running out (a deadline, not
// an exit), an ownership or environment refusal before the script started, a
// child killed by a signal, and the script's own "not needed" status. Unmarked
// failures hold the ping rung for a backoff and never escalate (council A-F5).
func markProviderReportedFailure(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !exitErr.Exited() || exitErr.ExitCode() == providerOpExitNotNeeded {
		return err
	}
	return beads.ProviderReportedFailure(err)
}

// Recover asks bd to retire and re-establish a proxy that listens but never
// greets.
//
// Its failure is marked on exactly Ping's terms, and here the mark decides
// whether the handle survives (round4 recheck M1): admission ends the lane
// only on a recover bd ran and refused, and holds the rung for a backoff on
// everything else — gc's own read budget SIGKILLing a cold start, the
// lifecycle slot the health loop's recover of the same zombie holds, an
// environment gc could not build.
//
// It is aimed at one generation (round4 review F3): the recover queues on the
// per-city lifecycle slot, and the health loop's recover of the same zombie
// may hold it and bring up a healthy proxy first. So the provider's recover —
// `bd dolt stop` then `bd ping` — runs only if, with the slot held, the
// scope's proxy record still names that generation; otherwise nothing runs
// and the error wraps beads.ErrRecoverTargetMoved (unmarked: bd was not asked).
func (o proxiedProviderOps) Recover(ctx context.Context, scopeRoot, generation string) error {
	if err := providerOwnedScopeLifecycleOp(ctx, o.cityPath, scopeRoot, proxiedProviderRecoverOp,
		proxiedRecoverStillAimed(scopeRoot, generation)); err != nil {
		return markProviderReportedFailure(err)
	}
	o.noteReady(scopeRoot)
	return nil
}

// proxiedRecoverStillAimed is Recover's precondition: the scope's proxy record
// still names generation. A root or record it cannot read counts as moved,
// because `bd dolt stop` is never aimed at a proxy gc cannot name — and the
// admission ladder that asked re-reads the record and asks again if the
// zombie really is still there.
func proxiedRecoverStillAimed(scopeRoot, generation string) func() error {
	return func() error {
		root, err := proxyendpoint.ProviderRoot(scopeRoot)
		if err != nil {
			return fmt.Errorf("proxied recover of %s: resolving the proxy root: %w: %w", scopeRoot, err, beads.ErrRecoverTargetMoved)
		}
		record, err := proxyendpoint.Read(root)
		if err != nil {
			return fmt.Errorf("proxied recover of %s: reading the proxy record: %w: %w", scopeRoot, err, beads.ErrRecoverTargetMoved)
		}
		if current := proxyendpoint.NewPoolKey(record, "").Generation(); current != generation {
			return fmt.Errorf("proxied recover of %s: aimed at generation %s, the proxy is now %s: %w",
				scopeRoot, generation, current, beads.ErrRecoverTargetMoved)
		}
		return nil
	}
}

// noteReady is the readiness memo of design 3.3 step 2: the generation bd
// published right after a verb succeeded is recorded as one gc has already spent
// a verb on.
//
// The consequence is deliberate and worth stating plainly. Admission's ladder
// keys its rungs on the generation, so a proxy that goes SILENT immediately after
// gc pinged it does not get pinged a second time in the same process — it goes
// straight to the recover rung, which is the right escalation for "we just asked
// bd for this and this is what we got". Without the memo the ladder would spend
// its ping on a generation gc itself had just produced.
//
// Every read here is best-effort: a record gc cannot read is a memo entry gc
// does not make, and the ladder simply spends a rung it could have skipped.
func (o proxiedProviderOps) noteReady(scopeRoot string) {
	if o.observed == nil {
		return
	}
	root, err := proxyendpoint.ProviderRoot(scopeRoot)
	if err != nil {
		return
	}
	record, err := proxyendpoint.Read(root)
	if err != nil {
		return
	}
	// The generation is the {pid, birth} pair alone: one proxy legitimately
	// serves several databases, and a rung spent on the process is spent for all
	// of them.
	o.observed.Add(proxyendpoint.NewPoolKey(record, "").Generation())
}

// proxiedNativeOpener opens one scope's split store.
//
// Every effect that is not a plain file read is a field, so the ladder is
// provable without a proxy, a database or a bd binary: the provider ops, the
// process table, the probe, the clock, and the two library opens.
type proxiedNativeOpener struct {
	cityPath  string
	scopeRoot string
	cityName  string
	// database is the Dolt database to admit. It is resolved lazily (and
	// cached here by a test that presets it) rather than at construction,
	// because the opener is built for EVERY scope and only ever called for a
	// proxied one: resolving the canonical connection target at construction
	// would read config files for every direct city in the process.
	database string

	// openBd builds the WRITE leaf. It is the factory's own bd opener, so the
	// leaf inside the split store is byte-identical to the store this scope
	// would fall back to.
	openBd func() (beads.Store, error)

	ops          beads.ProviderOps
	processTable proxyendpoint.ProcessTable
	probe        func(ctx context.Context, ep proxyendpoint.Endpoint, database string) proxyendpoint.ProbeResult
	observed     *beads.GenerationSet
	recovered    *beads.GenerationSet
	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error

	// openNative and openNativeStorage are the two library opens, injected so a
	// test can drive the ladder on a host with no Dolt at all.
	openNative        func(ctx context.Context, scopeRoot string, env map[string]string, opts ...beads.NativeDoltStoreOption) (*beads.NativeDoltStore, error)
	openNativeStorage func(ctx context.Context, scopeRoot string, env map[string]string) (beads.NativeStorage, error)

	// leafObserve and storageObserve are the POST-open half of the
	// observation: HEAD and the ignored plane's sentinel reality, in one
	// statement issued twice on one pinned connection of the pool the library
	// open just built (the first advances a connection beads left on the
	// pre-open root, be-itm5), so the re-read costs round trips on a
	// connection gc already holds and never a session of its own. Injected for
	// the same reason as the two opens; nil means the production readers, so
	// omitting them cannot switch the check off.
	leafObserve    func(ctx context.Context, native *beads.NativeDoltStore) (proxyendpoint.PostOpenReport, error)
	storageObserve func(ctx context.Context, storage beads.NativeStorage) (proxyendpoint.PostOpenReport, error)

	// logger receives the post-open check's WARN when the re-read itself
	// fails. Nil is slog.Default(): the composition roots build this opener
	// without one, and the controller's rig stores pass no logger anywhere, so
	// a nil here must not mean silence (council pr2 E-S2).
	logger *slog.Logger
}

// newProxiedNativeOpener builds the opener for one scope.
func newProxiedNativeOpener(cityPath, scopeRoot string, cfg *config.City, openBd func() (beads.Store, error)) *proxiedNativeOpener {
	return &proxiedNativeOpener{
		cityPath:  cityPath,
		scopeRoot: scopeRoot,
		cityName:  proxiedNativeAuthorCityName(cfg),
		openBd:    openBd,
		ops: proxiedProviderOps{
			cityPath: cityPath,
			observed: proxiedObservedGenerations,
		},
		observed:          proxiedObservedGenerations,
		recovered:         proxiedRecoveredGenerations,
		openNative:        beads.OpenNativeDoltStoreAtProxied,
		openNativeStorage: beads.OpenNativeStorageAtProxied,
		leafObserve:       beads.ProxiedLeafObservation,
		storageObserve:    beads.ProxiedOpenedObservation,
	}
}

// scopeDatabase resolves the database this scope's proxy serves, once per open.
// An unresolvable one is passed through as empty on purpose: admission refuses
// an empty database with a TYPED verdict, so the factory falls back to the bd
// front door with a visible reason instead of the lane quietly not existing.
func (o *proxiedNativeOpener) scopeDatabase() string {
	if o.database != "" {
		return o.database
	}
	return proxiedScopeDatabase(o.cityPath, o.scopeRoot)
}

// proxiedScopeDatabase resolves the Dolt database name for a scope from the
// connection contract — the same resolution doctor's beads-store check and every
// bd child's environment use, so the native handle and the bd leaf agree on which
// database they are talking about.
//
// What "resolve" means here is narrower than an earlier version of this comment
// claimed, and the claim was wrong rather than imprecise (council C-F13). It
// said a scope the contract cannot resolve yields "" and "the lane declines
// rather than guessing". ResolveDoltConnectionTarget initializes
// Database: "beads" unconditionally and overwrites it only when
// .beads/metadata.json actually names one, so a proxied scope whose metadata
// lacks dolt_database but whose config validates gets the GUESSED name "beads",
// and admission pins and probes it. Only a total resolution failure — the error
// return — yields "".
//
// The guess is fenced rather than harmless, which is why this is a comment fix
// and not a code one: a wrong database fails the probe and the lane refuses
// with a typed verdict, and a shared proxy root serving a differently-prefixed
// database is caught by proxiedPrefixAgreement with both handles open. And
// "beads" is bd's own default, so the guess is right for every scope bd
// created without being asked otherwise. An empty name is still refused by
// admission, because the cursors it gates on are DATABASE()-scoped.
//
// It deliberately does NOT go through canonicalScopeDoltTarget, and this is the
// first defect the acceptance gate found. That helper requires
// ResolveScopeConfigState to return ScopeConfigAuthoritative — which means the
// scope's .beads/config.yaml carries gc's own endpoint keys. A gc-initialized
// PROXIED city carries none of them: bd owns the topology, gc retires its
// canonical Dolt config for such a scope on purpose, and what is left is bd's own
// `issue_prefix:`-only file, which the contract classifies as
// ScopeConfigLegacyMinimal. So the lane resolved "" for the one shape it exists
// to serve and every proxied open on a gc-initialized city refused with
// no_ownership_record ("admission was given no database name"), silently taking
// the bd front door with the flag on.
//
// The database itself never depended on that authority: ResolveDoltConnectionTarget
// reads it from .beads/metadata.json's dolt_database, which is bd's own record of
// the database it serves, and validates the rest of the config on the way past.
func proxiedScopeDatabase(cityPath, scopeRoot string) string {
	target, err := contract.ResolveDoltConnectionTarget(fsys.OSFS{}, cityPath, scopeRoot)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(target.Database)
}

// storeOpener is the closure the factory calls.
func (o *proxiedNativeOpener) storeOpener() func(context.Context, bool) (beads.Store, beads.ProxiedOpenReport, error) {
	if o == nil {
		return nil
	}
	return o.open
}

// open runs admission, opens the library against the pinned generation, and
// joins the two leaves.
//
// The order matters and is the whole safety argument: NOTHING is opened until
// admission has returned a Pin, and a Pin can only come from beads.Admit. A
// caller that assembled the env map by hand has nothing to pass to
// beads.NewProxiedStore.
func (o *proxiedNativeOpener) open(parent context.Context, longLived bool) (beads.Store, beads.ProxiedOpenReport, error) {
	ctx, cancel := context.WithTimeout(parent, proxiedAdmissionBudget(longLived))
	defer cancel()

	pin, err := o.admit(ctx, longLived)
	if err != nil {
		return nil, pin.Report(), err
	}

	native, err := o.openNativeLeaf(ctx, pin, longLived, beads.ProxiedIncidentSiteOpen)
	if err != nil {
		return nil, pin.Report(), err
	}

	bd, err := o.openBd()
	if err != nil {
		closeProxiedLeafQuietly(native)
		return nil, pin.Report(), err
	}
	if verdict := proxiedPrefixAgreement(pin, native, bd); verdict != nil {
		// H10: the two leaves take their id prefix from different authorities —
		// the bd leaf from config, the native leaf from the database's own
		// issue_prefix row — and a split whose halves disagree about which beads
		// are foreign is worse than no split. Both handles are open by now, which
		// is why this is the opener's check and not admission's: admission never
		// opens the library and cannot ask the database.
		closeProxiedLeafQuietly(native)
		closeProxiedLeafQuietly(bd)
		return nil, pin.Report(), verdict
	}

	// The recovery path for a NON-terminal stand-down, and only for a
	// long-lived store: a one-shot has no guard to call it, and its handle does
	// not outlive the command. See beads.NativeLeafReopener (council A-F1).
	var opts []beads.ProxiedStoreOption
	if longLived {
		opts = append(opts, beads.WithNativeLeafReopener(o.recoverNativeLeaf()))
	}
	store, err := beads.NewProxiedStore(native, bd, pin, opts...)
	if err != nil {
		closeProxiedLeafQuietly(native)
		closeProxiedLeafQuietly(bd)
		return nil, pin.Report(), err
	}
	if longLived {
		// Only a store held for the process lifetime is worth a ticker; a
		// one-shot's evidence cannot go stale inside its own lifetime in any way
		// a read would not surface.
		store.StartGuard()
	}
	return store, store.Report(), nil
}

// openNativeLeaf opens the library against an admitted pin. It is shared by the
// first open and by the guard tick's recovery, so a replacement leaf is
// configured exactly like the one it replaces — the read-only fence, the proxied
// read budget and the reconnect hook are not things a second call site may
// forget. site names the open for the post-open check's log line.
func (o *proxiedNativeOpener) openNativeLeaf(ctx context.Context, pin beads.Pin, longLived bool, site string) (*beads.NativeDoltStore, error) {
	native, err := o.openNative(ctx, o.scopeRoot, nativeDoltProxiedOpenEnvForPin(o.cityName, pin, longLived),
		// The reconnect hook is where a re-pin actually happens: it re-runs
		// admission and re-projects the CURRENT generation's endpoint, on the
		// reader's goroutine.
		beads.WithNativeReopen(o.reopen(longLived)),
		// Ten seconds, not the direct lane's ninety: a bd-owned proxy is not
		// gc's to restart, so a read that cannot reach it should demote in
		// seconds rather than hold a caller through a minute and a half of
		// mysql i/o timeouts.
		beads.WithNativeReadRetryBudget(beads.ProxiedReadBudget()),
		// The second read-only fence (the first is that the wrapper claims no
		// graph-apply interface), in case a bare leaf ever escapes the wrapper.
		beads.WithProxiedReadOnly())
	if err != nil {
		return nil, fmt.Errorf("open native store over bd's proxy at %s: %w", o.scopeRoot, err)
	}
	leafObserve := o.leafObserve
	if leafObserve == nil {
		leafObserve = beads.ProxiedLeafObservation
	}
	if verdict := o.openUnmoved(ctx, pin, site, func(ctx context.Context) (proxyendpoint.PostOpenReport, error) {
		return leafObserve(ctx, native)
	}); verdict != nil {
		closeProxiedLeafQuietly(native)
		return nil, verdict
	}
	return native, nil
}

// openUnmoved is the post-open half of the observation (council pr2 D-F3,
// E-S4).
//
// The library open gc just performed is WRITABLE — beads exports no read-only
// open to an embedder at v1.3.0 — and on a database the schema gate admits it
// can still seed dolt_ignore, heal the tracked cursor table or add a
// content_hash column (see beads/schema_cursor.go, "The residual"). No cursor
// comparison can see any of that. A HEAD hash sees each of those writes that
// COMMITS; the dolt_ignore'd plane is never committed, so the same statement
// also reads that plane's cursor table and sentinel reality, and what that half
// can and cannot conclude is on beads.ProxiedOpenUnmoved.
//
// The pre-open hash came free with the probe session's first statement; the
// re-read is read(), over the library's own pool (beads.ProxiedOpenedObservation:
// the SECOND statement on one pinned connection, because the first answers from
// the pre-open root on a connection the open's checks ran on). A pin served
// from the admission memo carries no hash, so it spends nothing and concludes
// nothing. A re-read that fails concludes nothing either: the pool that just
// served the open cannot answer, the first read will meet the same failure on
// the read path, and this check is detection over a gate that already passed.
// It is not SILENT, though (council pr2 E-S2): the check that exists to catch
// gc writing to bd's database did not run, and that is logged at WARN with
// the site and the reason.
//
// A verdict is not logged here. It is returned, and every consumer that meets
// it — the factory, the read path, the guard's recovery — logs it through the
// lane's one incident line (internal/beads proxied_incident_log.go), so a
// head_moved is reported once, at the place that decided what to do about it.
//
// On a verdict the memoized admission is forgotten. Otherwise the next open in
// this process would be served from the memo — which carries no hash — and walk
// past the one check that just fired.
func (o *proxiedNativeOpener) openUnmoved(ctx context.Context, pin beads.Pin, site string, read func(context.Context) (proxyendpoint.PostOpenReport, error)) error {
	if pin.Head() == "" {
		return nil
	}
	observed, err := read(ctx)
	if err != nil {
		beads.LogProxiedPostOpenUnobserved(o.logger, o.scopeRoot, site, err)
		return nil
	}
	verdict := beads.ProxiedOpenUnmoved(pin, observed)
	if verdict != nil {
		beads.ForgetProxiedPin(o.scopeRoot, pin.Database())
	}
	return verdict
}

// recoverNativeLeaf is the guard tick's recovery for a handle that stood down
// NON-terminally, and it is the production caller (*beads.ProxiedStore).repin
// did not have (council A-F1).
//
// It re-runs admission with NO provider ops. That is what keeps the tick's
// "never forks bd" invariant true through the recovery: a proxy that needs bd
// to make it healthy comes back with a non-terminal verdict and the tick stays
// undecided. This comment used to add that "the rung is spent later by a read
// through the reconnect hook"; it is not (round3 review). The reconnect hook
// is reached only from a read the native leaf serves, and a handle that needs
// this recovery has no native leaf: its reads go through the bd front door
// until a tick's one-session recovery admits, however many intervals that
// takes, and no provider verb is spent through it at all.
//
// It is always the LONG-LIVED admission shape, because only a long-lived store
// has a guard: the finite-idle rule must apply to the replacement exactly as it
// applied to the original, or a re-pin would quietly acquire a resident handle
// on a proxy bd is going to retire.
//
// And it re-runs admission ONCE, with ProbeOnce (council pr2 D-F5): one pass,
// at most one probe session, no sleeps. The ordinary long-lived shape — three
// outer passes, each able to walk the three-attempt no-greeting ladder or the
// 60s drain — is for a caller that needs an answer now. The tick is not one:
// it runs again one interval later, which IS the retry. Without the cap a
// demoted controller on a silent proxy spent up to nine probe sessions and ~6s
// of sleeps every interval, on a proxy bd may be trying to retire.
//
// Its whole budget is the READ budget, not the long-lived admission budget
// (council pr2 D-F16). The library open inside it holds nativeDoltOpenEnvMu —
// process-global, and what every other scope's native open and
// ProcessEnvSnapshotExcludingNativeDoltOpen wait on — and the read path's
// equivalent reopen already runs under the read's ctx, ProxiedReadBudget. The
// 90s long-lived budget exists for a controller BOOT that may wait out a drain;
// a ProbeOnce recovery never waits, so the only thing the extra 80s bought was
// a background timer, with no caller waiting, able to hold that lock for a
// minute and a half against a proxy that served the probe and then wedged.
func (o *proxiedNativeOpener) recoverNativeLeaf() beads.NativeLeafReopener {
	return func(parent context.Context) (*beads.NativeDoltStore, beads.Pin, error) {
		ctx, cancel := context.WithTimeout(parent, beads.ProxiedReadBudget())
		defer cancel()
		in := o.admissionInput(true, nil)
		in.ProbeOnce = true
		pin, err := beads.Admit(ctx, in)
		if err != nil {
			return nil, beads.Pin{}, err
		}
		native, err := o.openNativeLeaf(ctx, pin, true, beads.ProxiedIncidentSiteGuardRecovery)
		if err != nil {
			return nil, beads.Pin{}, err
		}
		return native, pin, nil
	}
}

// admit is the escalation ladder: beads.Admit, plus a bounded outer retry for
// the verdicts that say "ask again".
//
// The RUNGS themselves live inside admission — one ping per generation, one
// recover per generation, the 3-attempt no-greeting wait, the bounded drain —
// because that is where the evidence is, and because two ladders over one
// generation set would spend each rung twice. What this loop adds is the part
// admission cannot decide: whether THIS caller should wait at all.
//
//   - A terminal verdict is returned immediately. There is nothing to ask again.
//   - A one-shot takes the bd front door on the first non-terminal verdict too:
//     the command in front of it would rather run now on the store it has today
//     than wait for a proxy to finish draining.
//   - A long-lived open retries, spaced, inside its budget, because the
//     alternative is a controller that forks bd for the rest of the process
//     because of a two-second restart at boot.
func (o *proxiedNativeOpener) admit(ctx context.Context, longLived bool) (beads.Pin, error) {
	return o.admitWith(ctx, longLived, o.ops)
}

// admitWith is admit with the provider surface chosen by the caller. A nil ops
// means the ladder cannot escalate, which is what the guard tick's recovery
// passes: a tick never forks bd.
func (o *proxiedNativeOpener) admitWith(ctx context.Context, longLived bool, ops beads.ProviderOps) (beads.Pin, error) {
	in := o.admissionInput(longLived, ops)
	var lastErr error
	for pass := 0; pass < proxiedAdmissionPasses; pass++ {
		pin, err := beads.Admit(ctx, in)
		if err == nil {
			return pin, nil
		}
		lastErr = err

		verdict, typed := beads.ProxiedVerdictOf(err)
		if !typed || verdict.Terminal() || !longLived {
			return beads.Pin{}, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return beads.Pin{}, err
		}
		if sleepErr := o.backoff(ctx, proxiedAdmissionBackoff); sleepErr != nil {
			return beads.Pin{}, err
		}
	}
	return beads.Pin{}, lastErr
}

// admissionInput is the one place the opener's admission shape is assembled,
// so the ordinary ladder and the guard recovery cannot drift apart on anything
// but the field the recovery sets on purpose.
func (o *proxiedNativeOpener) admissionInput(longLived bool, ops beads.ProviderOps) beads.AdmissionInput {
	return beads.AdmissionInput{
		ScopeRoot: o.scopeRoot,
		// The city decides which scope may spend the recover rung: a rig
		// sharing the city's proxy root never does (round4 recheck M2).
		CityRoot:     o.cityPath,
		Database:     o.scopeDatabase(),
		ProcessTable: o.processTable,
		Probe:        o.probe,
		Ops:          ops,
		LongLived:    longLived,
		Observed:     o.observed,
		Recovered:    o.recovered,
		Now:          o.now,
		Sleep:        o.sleep,
	}
}

// reopen is the read path's re-pin: re-run admission, re-project the CURRENT
// generation's endpoint, and hand back a fresh storage handle.
//
// It happens on the goroutine of the read that needed it — never on the guard
// tick's, which marks a pool stale and lets the next reader do this. It is the
// only library open that happens after the store exists WHILE THE NATIVE LEAF
// IS SERVING; once a handle has stood down non-terminally there is no read on
// the native leaf to carry it, and recoverNativeLeaf is the path instead. A typed
// verdict returned from here ends the read on its first pass (the native read
// path propagates it) so the wrapper can stand the handle down instead of
// spending the whole read budget re-learning the same refusal.
func (o *proxiedNativeOpener) reopen(longLived bool) beads.NativeReopenFunc {
	return func(ctx context.Context) (beads.NativeStorage, error) {
		pin, err := o.admit(ctx, longLived)
		if err != nil {
			return nil, err
		}
		storage, err := o.openNativeStorage(ctx, o.scopeRoot, nativeDoltProxiedOpenEnvForPin(o.cityName, pin, longLived))
		if err != nil {
			return nil, err
		}
		// This is a library open like any other, so it takes the same
		// post-open observation. A verdict returned from here ends the read on
		// its first pass and the wrapper stands the handle down.
		storageObserve := o.storageObserve
		if storageObserve == nil {
			storageObserve = beads.ProxiedOpenedObservation
		}
		if verdict := o.openUnmoved(ctx, pin, beads.ProxiedIncidentSiteReadReopen, func(ctx context.Context) (proxyendpoint.PostOpenReport, error) {
			return storageObserve(ctx, storage)
		}); verdict != nil {
			_ = storage.Close()
			return nil, verdict
		}
		return storage, nil
	}
}

func (o *proxiedNativeOpener) backoff(ctx context.Context, d time.Duration) error {
	if o.sleep != nil {
		return o.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func proxiedAdmissionBudget(longLived bool) time.Duration {
	if longLived {
		return proxiedLongLivedAdmissionBudget
	}
	return proxiedOneShotAdmissionBudget
}

// proxiedPrefixAgreement is H10, checked once, with both handles open.
//
// The bd leaf's prefix comes from the scope's config; the native leaf's comes
// from the database's own issue_prefix row. CachingStore filters foreign bead
// events by the backing store's prefix, and the two leaves of a split that
// disagreed would classify the same bead differently depending on which leaf
// answered. An empty prefix on either side is not a disagreement: an unfenced
// store is the shipped default for scopes an operator never configured.
func proxiedPrefixAgreement(pin beads.Pin, native *beads.NativeDoltStore, bd beads.Store) error {
	return beads.ProxiedPrefixAgreement(pin.Database(), native, bd)
}

// closeProxiedLeafQuietly releases a leaf an open is abandoning. A failed open
// that left a live Dolt connection behind would hold a pool slot against bd's
// proxy for the process lifetime.
func closeProxiedLeafQuietly(store any) {
	if closer, ok := store.(interface{ CloseStore() error }); ok {
		_ = closer.CloseStore()
	}
}

// proxiedNativeStoreOpenerForScope builds the factory's OpenProxiedStore for a
// city or rig scope.
//
// openBd is the factory's own bd opener, passed through rather than rebuilt: the
// write leaf inside the split store must be the SAME store the scope falls back
// to, or a demotion would change which store is doing the writing.
func proxiedNativeStoreOpenerForScope(cityPath, scopeRoot string, cfg *config.City, openBd func() (beads.Store, error)) func(context.Context, bool) (beads.Store, beads.ProxiedOpenReport, error) {
	return newProxiedNativeOpener(cityPath, scopeRoot, cfg, openBd).storeOpener()
}
