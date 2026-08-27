package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"k8s.io/client-go/util/workqueue"
)

const sessionStartAdmissionMaxIDBytes = 256

var errSessionStartLegacyFallbackRequired = errors.New("session start requires legacy fallback")

var errSessionStartPoolDrainAckPending = errors.New("pool drain acknowledgement stop remains pending")

type sessionStartAdmissionSource string

const (
	sessionStartAdmissionPendingCreate  sessionStartAdmissionSource = "pending_create"
	sessionStartAdmissionExplicitWake   sessionStartAdmissionSource = "explicit_wake"
	sessionStartAdmissionInProcess      sessionStartAdmissionSource = "in_process"
	sessionStartAdmissionSocket         sessionStartAdmissionSource = "socket"
	sessionStartAdmissionAntiEntropy    sessionStartAdmissionSource = "anti_entropy"
	sessionStartAdmissionWaitDependency sessionStartAdmissionSource = "wait_dependency"
	// sessionStartAdmissionDeadline is the detector sweep's D-DEADLINE key
	// (DETECTOR.md §3): an idle-timeout or max-session-age stop for one exact
	// durable session ID. Later WD slices add one value per condition family.
	sessionStartAdmissionDeadline sessionStartAdmissionSource = "deadline"
	// sessionStartAdmissionOrphanClose is the detector sweep's D-ORPHAN CLOSE
	// key (DETECTOR.md §3): an undesired row whose runtime is provably absent,
	// or a failed-create row whose create lease has expired.
	sessionStartAdmissionOrphanClose sessionStartAdmissionSource = "orphan_close"
	// sessionStartAdmissionOrphanDrain is the same family's DRAIN key: an
	// undesired row whose runtime is still live. It is a separate source from
	// the close because each arm has its own legacy yield, and one source
	// serving both would make each yield stand down for the other arm's rows.
	sessionStartAdmissionOrphanDrain sessionStartAdmissionSource = "orphan_drain"
	// sessionStartAdmissionStaleCreate is the detector sweep's D-STALE-CREATE
	// key: a crash-stranded pending create whose lease has expired with no
	// runtime, for one exact durable session ID.
	sessionStartAdmissionStaleCreate sessionStartAdmissionSource = "stale_create"
	// sessionStartAdmissionConfigDrift is the detector sweep's D-DRIFT key: a
	// session whose stored core or live fingerprint no longer matches the config
	// it is declared with. One source serves both legacy sites (ConfigDrift and
	// LiveDrift) because they are two spellings of one convergence ladder behind
	// one legacy yield.
	sessionStartAdmissionConfigDrift sessionStartAdmissionSource = "config_drift"
	// sessionStartAdmissionDuplicateNamed is the detector sweep's D-DUP key
	// (DETECTOR.md §3): the retire of ONE duplicate configured-named session row,
	// keyed on that loser's exact durable session ID. One key per loser, so a
	// three-way duplicate is three independent fenced effects, never a batch.
	sessionStartAdmissionDuplicateNamed sessionStartAdmissionSource = "duplicate_named"
	// sessionStartAdmissionSleepDrain is the detector sweep's D-SLEEP key
	// (DETECTOR.md §3): one alive session the awake set no longer wants, keyed
	// on its exact durable session ID. The same source carries the family's
	// idle-probe launch — a probe and a drain are two rungs of one ladder on one
	// key, and the handler picks the rung from the durable row plus the probe
	// tracker, never from the source.
	sessionStartAdmissionSleepDrain sessionStartAdmissionSource = "sleep_drain"
	// sessionStartAdmissionProgressStall is the detector sweep's D-STALL key
	// (DETECTOR.md §3): one live session whose provider-reported activity gap
	// has passed the configured stall threshold. The min-floor exemption
	// suppresses this key for the claim-less family only, so a floor worker
	// still arrives here whenever claim_holder_stall_timeout is positive and the
	// handler owes it the per-session claim lookup.
	sessionStartAdmissionProgressStall sessionStartAdmissionSource = "progress_stall"
	// sessionStartAdmissionDrainAdvance is the detector sweep's D-DRAIN key: a
	// session with drain intent recorded in the shared in-memory tracker. The
	// handler behind it discovers the acknowledgement, applies the cancel arms,
	// and drives the drain to its terminal state; the stop leg belongs to the
	// keyed drain-ack stop, which owns the row from the stop-pending transition
	// on.
	sessionStartAdmissionDrainAdvance sessionStartAdmissionSource = "drain_advance"
	// sessionStartAdmissionStrandedRepair is the detector sweep's D-STRANDED
	// key (DETECTOR.md §3): a pool slot whose runtime is gone, whose bead sits
	// in a terminal sleep state, and whose confirmed stranding episode has
	// outlived strandedRepairConfirmGrace while it still holds assigned work.
	sessionStartAdmissionStrandedRepair sessionStartAdmissionSource = "stranded_repair"
	// sessionStartAdmissionZombieMark is the detector sweep's D-ZOMBIE key
	// (DETECTOR.md §3): a session whose runtime is still PRESENT while its agent
	// process is gone — `running ∧ !alive` from the sweep's two-bit liveness
	// probe, which is legacy's own zombie predicate. It is deliberately not
	// "absent from the running set": a zombie IS in that names-only set, which
	// is why absence belongs to D-ORPHAN and D-WAKE instead.
	sessionStartAdmissionZombieMark sessionStartAdmissionSource = "zombie_mark"
	// sessionStartAdmissionWakeFill is the detector sweep's D-WAKE key: a row
	// the awake set wants and the two-bit liveness probe found dead. It names the
	// SOURCE only; the admission itself always carries one of the certified wake
	// leases (AdmitConfiguredNamedWake / AdmitConfiguredDependency /
	// AdmitStrictDefaultPoolWake), because a wake is a start and a start is the
	// exclusive effect boundary a lease exists to fence.
	sessionStartAdmissionWakeFill sessionStartAdmissionSource = "wake_fill"
)

type sessionStartAdmission struct {
	SessionID                    string
	Source                       sessionStartAdmissionSource
	Version                      uint64
	PoolAllocation               *routedWorkPoolStartLease
	PoolDrainAck                 *routedWorkPoolDrainAckLease
	WaitDependency               *sessionWaitDependencyStartLease
	ConfiguredDependency         *configuredDependencyStartLease
	ConfiguredDependencyEntered  bool
	StrictDefaultPoolWake        *strictDefaultPoolWakeStartLease
	StrictDefaultPoolWakeEntered bool
	ConfiguredNamedWake          *configuredNamedWakeStartLease
	ConfiguredNamedWakeEntered   bool
	// PoolDrainAckUncertain retains a durable stop-pending row when an
	// anti-entropy seed cannot reconstruct its agent acknowledgement lease.
	// It is a retry fence, never destructive-stop authority.
	PoolDrainAckUncertain bool
	// PoolDrainAckUncertainToken is the ROW's instance token, read by the
	// seed when it built the uncertain retention. The lease is not
	// reconstructable for these rows, so without this the refusal streak has
	// no obligation identity at all and a fresh incarnation's drain would
	// inherit its predecessor's count through the token-less uncertain path
	// (ga-c9m4g — the field's 22 unbounded release climbers were exactly this
	// class). It scopes the streak only; it grants nothing.
	PoolDrainAckUncertainToken string
	PoolStartEntered           bool
	CensusGeneration           uint64
	Culled                     bool
	AdmittedAt                 time.Time
	// DrainAckDeadline bounds the retained drain-ack re-queue below. A drain-ack
	// is a durable obligation that must never be dropped, so it is deliberately
	// NOT bounded by maxRetries — but while it is parked the keyed controller
	// EXCLUDES legacy from the row (ownsPoolDrainAckStop), so an authorization
	// that permanently refuses would block the drain from finishing under ANY
	// owner. The bound is therefore the DRAIN's own ack-or-timeout deadline
	// (ga-f7v2ft.112 ruling 1b): stamped when the obligation is first retained,
	// carried across coalescing so a re-admission storm cannot roll it forward.
	DrainAckDeadline time.Time
	// DrainAckCycleStartRefusals is the obligation's streak count at the
	// moment DrainAckDeadline was stamped — the start of this deadline cycle.
	// The deadline release reports its own cycle's refusals from it
	// (ga-c9m4g), so the release line stays a bounded per-cycle number instead
	// of re-printing the obligation's lifetime climb. Carried across
	// coalescing exactly like the deadline it anchors to.
	DrainAckCycleStartRefusals int
	// DrainAckRefusals mirrors the OBLIGATION-scoped consecutive-refusal count
	// (sessionStartController.drainAckRefusalHistory) at the last bound check.
	// Repeated (false, nil) authorization refusals are indistinguishable from
	// transient by construction, so the counter classifies nothing: it throttles
	// the observability escalation and, at drainAckRefusalEscalationThreshold,
	// moves the retained obligation onto the slow re-examination cadence. It
	// survives version coalescing and the deadline release; any resolution of
	// the admission clears it (ga-f7v2ft.173).
	DrainAckRefusals int
}

type sessionStartAdmissionOutcome string

const (
	sessionStartAdmissionAccepted  sessionStartAdmissionOutcome = "accepted"
	sessionStartAdmissionCoalesced sessionStartAdmissionOutcome = "coalesced"
	sessionStartAdmissionOverflow  sessionStartAdmissionOutcome = "overflow"
	sessionStartAdmissionStale     sessionStartAdmissionOutcome = "stale"
)

type sessionStartReconcileOutcome string

const (
	sessionStartReconcileSucceeded  sessionStartReconcileOutcome = "succeeded"
	sessionStartReconcileSuperseded sessionStartReconcileOutcome = "superseded"
	sessionStartReconcileRetrying   sessionStartReconcileOutcome = "retrying"
	sessionStartReconcileExhausted  sessionStartReconcileOutcome = "exhausted"
	sessionStartReconcileCanceled   sessionStartReconcileOutcome = "canceled"
	// sessionStartReconcileDeadlineExceeded is the drain-ack obligation's own
	// terminal outcome. It is NOT exhaustion: the admission was never bounded by
	// a retry count, it was bounded by the drain's ack-or-timeout deadline, and
	// reaching that deadline RELEASES ownership so a surviving owner can finish
	// the row (ga-f7v2ft.112 ruling 1b).
	sessionStartReconcileDeadlineExceeded sessionStartReconcileOutcome = "deadline_exceeded"
	// sessionStartReconcileDrainAckEscalated is the NAMED state a retained
	// drain-ack obligation enters once its obligation-scoped refusal count
	// crosses drainAckRefusalEscalationThreshold: the obligation (and its
	// legacy-exclusion fence) is retained, but re-examination drops from the
	// hot rate-limited cadence to drainAckEscalatedRetryInterval. Level-
	// triggered convergence is preserved — the row is still re-read on every
	// re-examination, so landed stamps or a died runtime resolve it — while an
	// unresolvable lease stops burning reconcile cycles (ga-f7v2ft.173).
	sessionStartReconcileDrainAckEscalated sessionStartReconcileOutcome = "drain_ack_escalated"
)

type sessionStartReconcileResult struct {
	Admission      sessionStartAdmission
	Outcome        sessionStartReconcileOutcome
	StartedAt      time.Time
	FinishedAt     time.Time
	LegacyFallback bool
	Err            error
	// DrainAckRefusals is the consecutive-refusal count that produced this
	// result, carried out so the runtime's observer can emit the throttled
	// diagnostic without the controller learning how to trace.
	DrainAckRefusals int
	// DrainAckCycleRefusals is the consecutive-refusal count accumulated
	// within the current deadline cycle (since the retained obligation's
	// deadline was stamped). The deadline-release line reports it so its
	// number is bounded by one cycle's re-examinations; the obligation's
	// cumulative count stays in DrainAckRefusals (ga-c9m4g).
	DrainAckCycleRefusals int
	// DrainAckEscalationCrossing marks the single bound check on which this
	// obligation's consecutive-refusal count first crossed
	// drainAckRefusalEscalationThreshold. The crossing is OBLIGATION-scoped
	// (ga-f7v2ft.191): the same wedged obligation announces its escalation
	// once and re-examines quietly afterward, while a genuinely new
	// obligation earns its own crossing when — and only when — its own streak
	// reaches the threshold.
	DrainAckEscalationCrossing bool
}

type sessionStartAuthoritativeSeedResult struct {
	SessionID             string
	PoolDrainAck          *routedWorkPoolDrainAckLease
	PoolDrainAckUncertain bool
	// PoolDrainAckUncertainToken carries the row's instance token alongside an
	// uncertain retention, read from the same row the seed judged
	// stop-pending. It scopes the obligation's refusal streak (ga-c9m4g); it
	// grants no stop authority.
	PoolDrainAckUncertainToken string
	Complete                   bool
	Err                        error
}

type sessionStartControllerOptions struct {
	Workers     int
	MaxDistinct int
	MaxRetries  int
	Reconcile   func(context.Context, sessionStartAdmission) error
	Observer    func(sessionStartReconcileResult)
	RateLimiter workqueue.TypedRateLimiter[string]
	Now         func() time.Time
	Stderr      io.Writer
}

// sessionStartController is a bounded, keyed workqueue for session-start
// reconciliation. The durable store remains authoritative: admissions are only
// hints naming which exact key to reread.
type sessionStartController struct {
	queue       workqueue.TypedRateLimitingInterface[string]
	workers     int
	maxDistinct int
	maxRetries  int
	reconcile   func(context.Context, sessionStartAdmission) error
	observer    func(sessionStartReconcileResult)
	now         func() time.Time
	stderr      io.Writer
	admissions  map[string]sessionStartAdmission
	// drainAckRefusalHistory is the OBLIGATION-scoped consecutive-refusal
	// streak for retained drain-acks, keyed by session ID. It deliberately
	// survives the deadline release (which deletes the admission and arms an
	// audit) so the release → re-detect → retry macro cycle cannot reset the
	// escalation bound for the SAME obligation; a re-admission carrying a
	// different instance token is a genuinely NEW obligation — a fresh drain
	// of a fresh incarnation — and starts a fresh streak (ga-f7v2ft.191). Any
	// resolution of the admission clears it (ga-f7v2ft.173).
	drainAckRefusalHistory    map[string]drainAckRefusalStreak
	nextVersion               uint64
	auditPending              bool
	seedOutstanding           map[string]struct{}
	inFlight                  map[string]uint64
	seedGeneration            uint64
	seedActive                bool
	seedCapacity              chan struct{}
	beforeMarkInFlightForTest func()

	mu        sync.Mutex
	started   bool
	accepting bool
	stopped   bool
	ctx       context.Context
	cancel    context.CancelFunc
	workerWG  sync.WaitGroup
	seedWG    sync.WaitGroup
	stopOnce  sync.Once
	stderrMu  sync.Mutex
}

func newSessionStartController(opts sessionStartControllerOptions) (*sessionStartController, error) {
	switch {
	case opts.Workers <= 0:
		return nil, fmt.Errorf("creating session-start controller: workers must be positive")
	case opts.MaxDistinct <= 0:
		return nil, fmt.Errorf("creating session-start controller: max distinct admissions must be positive")
	case opts.MaxRetries < 0:
		return nil, fmt.Errorf("creating session-start controller: max retries must not be negative")
	case opts.Reconcile == nil:
		return nil, fmt.Errorf("creating session-start controller: reconcile function is nil")
	}
	rateLimiter := opts.RateLimiter
	if rateLimiter == nil {
		rateLimiter = workqueue.DefaultTypedControllerRateLimiter[string]()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	return &sessionStartController{
		queue:                  workqueue.NewTypedRateLimitingQueue(rateLimiter),
		workers:                opts.Workers,
		maxDistinct:            opts.MaxDistinct,
		maxRetries:             opts.MaxRetries,
		reconcile:              opts.Reconcile,
		observer:               opts.Observer,
		now:                    now,
		stderr:                 stderr,
		admissions:             make(map[string]sessionStartAdmission, opts.MaxDistinct),
		drainAckRefusalHistory: make(map[string]drainAckRefusalStreak, opts.MaxDistinct),
		seedOutstanding:        make(map[string]struct{}),
		inFlight:               make(map[string]uint64, opts.MaxDistinct),
		seedCapacity:           make(chan struct{}, 1),
	}, nil
}

func (c *sessionStartController) Start(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("starting session-start controller: controller is nil")
	}
	if ctx == nil {
		return fmt.Errorf("starting session-start controller: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("starting session-start controller: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.stopped {
		return fmt.Errorf("starting session-start controller: controller is single-start")
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.started = true
	c.accepting = true
	c.workerWG.Add(c.workers)
	for range c.workers {
		go c.runWorker()
	}
	return nil
}

func (c *sessionStartController) Admit(id string, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting session start: controller is nil")
	}
	if err := validateSessionStartAdmission(id, source); err != nil {
		return "", err
	}

	outcome, _, err := c.admit(id, source, false, 0, nil, nil, false, "", nil, nil, nil, nil)
	return outcome, err
}

func (c *sessionStartController) AdmitPoolAllocation(lease routedWorkPoolStartLease) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting pool allocation: controller is nil")
	}
	if err := validateRoutedWorkPoolStartLease(lease); err != nil {
		return "", err
	}
	outcome, _, err := c.admit(lease.SessionID, sessionStartAdmissionInProcess, false, 0, &lease, nil, false, "", nil, nil, nil, nil)
	return outcome, err
}

// AdmitPoolDrainAck retains the narrow ownership proof for one agent-sourced
// pool drain acknowledgement until exact reconciliation has reread it.
func (c *sessionStartController) AdmitPoolDrainAck(lease routedWorkPoolDrainAckLease) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting pool drain acknowledgement: controller is nil")
	}
	if err := validateRoutedWorkPoolDrainAckLease(lease); err != nil {
		return "", err
	}
	outcome, _, err := c.admit(lease.SessionID, sessionStartAdmissionInProcess, false, 0, nil, &lease, false, "", nil, nil, nil, nil)
	return outcome, err
}

// AdmitWaitDependency retains a certified durable wait/session pair until the
// keyed worker has either committed its ready/start handoff or observed that
// the pair no longer matches. The queue remains keyed by the session because a
// provider start is the exclusive effect boundary.
func (c *sessionStartController) AdmitWaitDependency(lease sessionWaitDependencyStartLease) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting dependency wait start: controller is nil")
	}
	if err := validateSessionWaitDependencyStartLease(lease); err != nil {
		return "", err
	}
	outcome, _, err := c.admit(lease.SessionID, sessionStartAdmissionWaitDependency, false, 0, nil, nil, false, "", &lease, nil, nil, nil)
	return outcome, err
}

// The three wake-lease admissions take their SOURCE from the caller because the
// same certificate now arrives by two entry points: the CLI socket, and the
// detector sweep's D-WAKE routing seam (WD.10a), which admits under
// sessionStartAdmissionWakeFill so the parity join can tell a detected wake from
// an operator-driven one. The lease, not the source, is the ownership proof.
func (c *sessionStartController) AdmitConfiguredDependency(lease configuredDependencyStartLease, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting configured-dependency start: controller is nil")
	}
	if err := validateConfiguredDependencyStartLease(lease); err != nil {
		return "", err
	}
	outcome, _, err := c.admit(lease.SessionID, source, false, 0, nil, nil, false, "", nil, &lease, nil, nil)
	return outcome, err
}

func (c *sessionStartController) AdmitStrictDefaultPoolWake(lease strictDefaultPoolWakeStartLease, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting strict-default pool wake: controller is nil")
	}
	if err := validateStrictDefaultPoolWakeStartLease(lease); err != nil {
		return "", err
	}
	outcome, _, err := c.admit(lease.SessionID, source, false, 0, nil, nil, false, "", nil, nil, &lease, nil)
	return outcome, err
}

func (c *sessionStartController) AdmitConfiguredNamedWake(lease configuredNamedWakeStartLease, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	if c == nil {
		return "", fmt.Errorf("admitting configured named wake: controller is nil")
	}
	if err := validateConfiguredNamedWakeStartLease(lease); err != nil {
		return "", err
	}
	outcome, _, err := c.admit(lease.SessionID, source, false, 0, nil, nil, false, "", nil, nil, nil, &lease)
	return outcome, err
}

// poolDrainAckSupersedesPoolStart permits the one safe start-to-stop handoff:
// the same active pool incarnation acknowledges completion of the exact work
// that caused its start. A newer membership observation is allowed, but no
// identity, work, source, generation, or requester proof may change.
func poolDrainAckSupersedesPoolStart(start routedWorkPoolStartLease, drain routedWorkPoolDrainAckLease) bool {
	return start.SessionID == drain.SessionID &&
		start.InstanceToken == drain.InstanceToken &&
		start.ControllerGeneration == drain.ControllerGeneration &&
		start.PoolTarget == drain.PoolTarget &&
		start.WorkID == drain.WorkID &&
		start.SourceStore == drain.SourceStore &&
		start.MembershipRevision <= drain.MembershipRevision &&
		drain.RequesterSessionID == start.SessionID &&
		drain.RequesterInstanceToken == start.InstanceToken
}

// sessionStartAdmissionIsDemand reports whether a source names a DEMAND
// admission — the in-process event path or the socket command — as opposed to
// the anti-entropy census sweep or any detector-sweep key.
//
// This is the only source-shaped fact that survives coalescing. Which of the two
// demand keys is recorded does not: a socket admission folded onto a pending
// in_process one keeps the in_process source (see admit's coalescing rule, and
// TestSessionStartControllerPreservesInProcessAdmissionAcrossAntiEntropy), so
// whether a given commit trace reads "socket" or "in_process" depends on arrival
// order alone. Membership in the pair does survive, because nothing outside the
// pair can ever displace a member of it. ga-f7v2ft.125 ruled that arms must
// dispatch on the durable row and never on the source; this predicate is for the
// observers that legitimately ask the weaker question "did demand drive this,
// or a sweep" (ga-f7v2ft.142).
func sessionStartAdmissionIsDemand(source sessionStartAdmissionSource) bool {
	return source == sessionStartAdmissionInProcess || source == sessionStartAdmissionSocket
}

func (c *sessionStartController) admitAuthoritative(id string, censusGeneration uint64, poolDrainAck *routedWorkPoolDrainAckLease, poolDrainAckUncertain bool, poolDrainAckUncertainToken string) (sessionStartAdmissionOutcome, sessionStartAdmission, error) {
	return c.admit(id, sessionStartAdmissionAntiEntropy, true, censusGeneration, nil, poolDrainAck, poolDrainAckUncertain, poolDrainAckUncertainToken, nil, nil, nil, nil)
}

func (c *sessionStartController) admit(id string, source sessionStartAdmissionSource, authoritative bool, censusGeneration uint64, poolAllocation *routedWorkPoolStartLease, poolDrainAck *routedWorkPoolDrainAckLease, poolDrainAckUncertain bool, poolDrainAckUncertainToken string, waitDependency *sessionWaitDependencyStartLease, configuredDependency *configuredDependencyStartLease, strictDefaultPoolWake *strictDefaultPoolWakeStartLease, configuredNamedWake *configuredNamedWakeStartLease) (sessionStartAdmissionOutcome, sessionStartAdmission, error) {
	if err := validateSessionStartAdmission(id, source); err != nil {
		return "", sessionStartAdmission{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.accepting || c.stopped {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: controller is stopped", id)
	}
	if authoritative && (c.seedGeneration != censusGeneration || c.ctx.Err() != nil) {
		return sessionStartAdmissionStale, sessionStartAdmission{}, nil
	}
	previous, existed := c.admissions[id]
	if authoritative && !existed && len(c.seedOutstanding) >= c.authoritativeCapacity() {
		return sessionStartAdmissionOverflow, sessionStartAdmission{}, nil
	}
	if !existed && len(c.admissions) >= c.maxDistinct {
		if !authoritative {
			c.auditPending = true
		}
		return sessionStartAdmissionOverflow, sessionStartAdmission{}, nil
	}
	c.nextVersion++
	if c.nextVersion == 0 {
		c.auditPending = true
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: admission version exhausted", id)
	}
	admittedAt := c.now()
	if existed && (source == sessionStartAdmissionAntiEntropy ||
		(previous.Source == sessionStartAdmissionInProcess && source != sessionStartAdmissionInProcess)) {
		source = previous.Source
		admittedAt = previous.AdmittedAt
	}
	leaseCount := 0
	for _, present := range []bool{poolAllocation != nil, poolDrainAck != nil, waitDependency != nil, configuredDependency != nil, strictDefaultPoolWake != nil, configuredNamedWake != nil} {
		if present {
			leaseCount++
		}
	}
	if leaseCount > 1 {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: conflicting exact-start leases", id)
	}
	supersedesPoolStart := false
	if existed && poolAllocation != nil && previous.PoolDrainAck != nil {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained pool lease conflicts with new admission", id)
	}
	if existed && waitDependency != nil && (previous.PoolAllocation != nil || previous.PoolDrainAck != nil) {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained pool lease conflicts with dependency wait", id)
	}
	if existed && (poolAllocation != nil || poolDrainAck != nil) && previous.WaitDependency != nil {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained dependency wait conflicts with pool lease", id)
	}
	if existed && configuredDependency != nil && (previous.PoolAllocation != nil || previous.PoolDrainAck != nil || previous.WaitDependency != nil || previous.StrictDefaultPoolWake != nil) {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained exact-start lease conflicts with configured dependency", id)
	}
	if existed && (poolAllocation != nil || poolDrainAck != nil || waitDependency != nil || strictDefaultPoolWake != nil) && previous.ConfiguredDependency != nil {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained configured dependency conflicts with exact-start lease", id)
	}
	if existed && strictDefaultPoolWake != nil && (previous.PoolAllocation != nil || previous.PoolDrainAck != nil || previous.WaitDependency != nil) {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained exact-start lease conflicts with strict-default pool wake", id)
	}
	if existed && (poolAllocation != nil || poolDrainAck != nil || waitDependency != nil) && previous.StrictDefaultPoolWake != nil {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained strict-default pool wake conflicts with exact-start lease", id)
	}
	if existed && configuredNamedWake != nil && (previous.PoolAllocation != nil || previous.PoolDrainAck != nil || previous.WaitDependency != nil || previous.ConfiguredDependency != nil || previous.StrictDefaultPoolWake != nil) {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained exact-start lease conflicts with configured named wake", id)
	}
	if existed && (poolAllocation != nil || poolDrainAck != nil || waitDependency != nil || configuredDependency != nil || strictDefaultPoolWake != nil) && previous.ConfiguredNamedWake != nil {
		return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained configured named wake conflicts with exact-start lease", id)
	}
	if existed && poolDrainAck != nil && previous.PoolAllocation != nil {
		if !previous.PoolStartEntered || !poolDrainAckSupersedesPoolStart(*previous.PoolAllocation, *poolDrainAck) {
			return "", sessionStartAdmission{}, fmt.Errorf("admitting session start %q: retained pool lease conflicts with new admission", id)
		}
		supersedesPoolStart = true
	}
	poolStartEntered := false
	if poolAllocation == nil && existed && !supersedesPoolStart {
		poolAllocation = previous.PoolAllocation
		poolStartEntered = previous.PoolStartEntered
	} else if poolAllocation != nil && existed && previous.PoolAllocation != nil &&
		previous.PoolAllocation.SessionID == poolAllocation.SessionID &&
		previous.PoolAllocation.InstanceToken == poolAllocation.InstanceToken {
		poolStartEntered = previous.PoolStartEntered
	}
	if poolAllocation != nil {
		copied := *poolAllocation
		poolAllocation = &copied
	}
	if poolDrainAck == nil && existed {
		poolDrainAck = previous.PoolDrainAck
		if !poolDrainAckUncertain {
			poolDrainAckUncertain = previous.PoolDrainAckUncertain
			poolDrainAckUncertainToken = previous.PoolDrainAckUncertainToken
		}
	}
	if poolDrainAck != nil {
		copied := *poolDrainAck
		poolDrainAck = &copied
	}
	if waitDependency == nil && existed {
		waitDependency = previous.WaitDependency
	}
	if waitDependency != nil && existed && previous.WaitDependency != nil && sameWaitDependencyCertificate(*previous.WaitDependency, *waitDependency) {
		// Duplicate hints for the same durable observation keep the first minted
		// operation even when an in-memory index rebuild changes only routing
		// generation. A changed durable revision replaces the parked lease.
		copied := *waitDependency
		copied.Operation = previous.WaitDependency.Operation
		waitDependency = &copied
	}
	if waitDependency != nil {
		copied := *waitDependency
		copied.DepIDs = append([]string(nil), waitDependency.DepIDs...)
		waitDependency = &copied
	}
	configuredDependencyEntered := false
	if configuredDependency == nil && existed {
		configuredDependency = previous.ConfiguredDependency
		configuredDependencyEntered = previous.ConfiguredDependencyEntered
	} else if configuredDependency != nil && existed && previous.ConfiguredDependency != nil &&
		*configuredDependency == *previous.ConfiguredDependency {
		configuredDependencyEntered = previous.ConfiguredDependencyEntered
	}
	if configuredDependency != nil {
		copied := *configuredDependency
		configuredDependency = &copied
	}
	strictDefaultPoolWakeEntered := false
	if strictDefaultPoolWake == nil && existed {
		strictDefaultPoolWake = previous.StrictDefaultPoolWake
		strictDefaultPoolWakeEntered = previous.StrictDefaultPoolWakeEntered
	} else if strictDefaultPoolWake != nil && existed && previous.StrictDefaultPoolWake != nil &&
		*strictDefaultPoolWake == *previous.StrictDefaultPoolWake {
		strictDefaultPoolWakeEntered = previous.StrictDefaultPoolWakeEntered
	}
	if strictDefaultPoolWake != nil {
		copied := *strictDefaultPoolWake
		strictDefaultPoolWake = &copied
	}
	configuredNamedWakeEntered := false
	if configuredNamedWake == nil && existed {
		configuredNamedWake = previous.ConfiguredNamedWake
		configuredNamedWakeEntered = previous.ConfiguredNamedWakeEntered
	} else if configuredNamedWake != nil && existed && previous.ConfiguredNamedWake != nil &&
		*configuredNamedWake == *previous.ConfiguredNamedWake {
		configuredNamedWakeEntered = previous.ConfiguredNamedWakeEntered
	}
	if configuredNamedWake != nil {
		copied := *configuredNamedWake
		configuredNamedWake = &copied
	}
	// The drain-ack bound survives coalescing; its refusal counter does not. A
	// new version is a new admission for the escalation's purposes, but the
	// obligation's deadline belongs to the DRAIN, not to whichever hint most
	// recently landed on the key. The cycle-start count anchors to the same
	// deadline, so it rides with it.
	drainAckDeadline := time.Time{}
	drainAckCycleStartRefusals := 0
	if existed {
		drainAckDeadline = previous.DrainAckDeadline
		drainAckCycleStartRefusals = previous.DrainAckCycleStartRefusals
	}
	admission := sessionStartAdmission{
		SessionID:                    id,
		Source:                       source,
		Version:                      c.nextVersion,
		PoolAllocation:               poolAllocation,
		PoolDrainAck:                 poolDrainAck,
		PoolDrainAckUncertain:        poolDrainAckUncertain,
		PoolDrainAckUncertainToken:   poolDrainAckUncertainToken,
		WaitDependency:               waitDependency,
		ConfiguredDependency:         configuredDependency,
		ConfiguredDependencyEntered:  configuredDependencyEntered,
		StrictDefaultPoolWake:        strictDefaultPoolWake,
		StrictDefaultPoolWakeEntered: strictDefaultPoolWakeEntered,
		ConfiguredNamedWake:          configuredNamedWake,
		ConfiguredNamedWakeEntered:   configuredNamedWakeEntered,
		PoolStartEntered:             poolStartEntered,
		AdmittedAt:                   admittedAt,
		DrainAckDeadline:             drainAckDeadline,
		DrainAckCycleStartRefusals:   drainAckCycleStartRefusals,
	}
	if authoritative && admission.Source == sessionStartAdmissionAntiEntropy {
		admission.CensusGeneration = censusGeneration
	}
	c.admissions[id] = admission
	if authoritative && !existed && admission.Source == sessionStartAdmissionAntiEntropy {
		c.seedOutstanding[id] = struct{}{}
	}
	c.queue.Add(id)
	if existed {
		return sessionStartAdmissionCoalesced, admission, nil
	}
	return sessionStartAdmissionAccepted, admission, nil
}

func sameWaitDependencyCertificate(a, b sessionWaitDependencyStartLease) bool {
	return sameDurableWaitDependencyCertificate(a, b)
}

// StartAuthoritativeSeed starts at most one bounded producer. next distinguishes
// normal snapshot exhaustion from an incomplete or failed producer result. It is
// called without the controller lock.
func (c *sessionStartController) StartAuthoritativeSeed(next func(context.Context) sessionStartAuthoritativeSeedResult) error {
	if c == nil {
		return fmt.Errorf("starting authoritative session-start seed: controller is nil")
	}
	if next == nil {
		return fmt.Errorf("starting authoritative session-start seed: next is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.accepting || c.stopped {
		return fmt.Errorf("starting authoritative session-start seed: controller is stopped")
	}
	if c.seedActive {
		c.seedGeneration++
		c.auditPending = true
		c.signalSeedCapacityLocked()
		return nil
	}
	c.seedGeneration++
	if c.seedGeneration == 0 {
		c.auditPending = true
		return fmt.Errorf("starting authoritative session-start seed: generation exhausted")
	}
	generation := c.seedGeneration
	c.auditPending = false
	c.seedActive = true
	c.seedWG.Add(1)
	go c.runAuthoritativeSeed(generation, next)
	return nil
}

func (c *sessionStartController) runAuthoritativeSeed(generation uint64, next func(context.Context) sessionStartAuthoritativeSeedResult) {
	defer c.seedWG.Done()
	defer func() {
		c.mu.Lock()
		c.seedActive = false
		c.mu.Unlock()
	}()

	pendingID := ""
	var pendingDrainAck *routedWorkPoolDrainAckLease
	pendingDrainAckUncertain := false
	pendingDrainAckUncertainToken := ""
	for {
		if err := c.ctx.Err(); err != nil || !c.seedGenerationCurrent(generation) {
			return
		}
		if pendingID == "" {
			result := next(c.ctx)
			if result.Err != nil {
				c.failAuthoritativeSeed(generation)
				return
			}
			if result.Complete {
				if result.SessionID != "" {
					c.failAuthoritativeSeed(generation)
					return
				}
				c.publishCompleteAuthoritativeCensus(generation)
				return
			}
			if result.SessionID == "" {
				c.failAuthoritativeSeed(generation)
				return
			}
			pendingID = result.SessionID
			pendingDrainAck = result.PoolDrainAck
			pendingDrainAckUncertain = result.PoolDrainAckUncertain
			pendingDrainAckUncertainToken = result.PoolDrainAckUncertainToken
		}
		outcome, _, err := c.admitAuthoritative(pendingID, generation, pendingDrainAck, pendingDrainAckUncertain, pendingDrainAckUncertainToken)
		if err != nil {
			c.failAuthoritativeSeed(generation)
			return
		}
		switch outcome {
		case sessionStartAdmissionAccepted, sessionStartAdmissionCoalesced:
			pendingID = ""
			pendingDrainAck = nil
			pendingDrainAckUncertain = false
			pendingDrainAckUncertainToken = ""
		case sessionStartAdmissionOverflow:
			if !c.waitForSeedCapacity() {
				return
			}
		case sessionStartAdmissionStale:
			return
		}
	}
}

func (c *sessionStartController) failAuthoritativeSeed(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seedGeneration != generation || !c.accepting || c.stopped || c.ctx.Err() != nil {
		return
	}
	c.auditPending = true
}

// publishCompleteAuthoritativeCensus scans only the controller's bounded
// admission set. A key absent from this generation is marked culled only when
// no worker is already executing that exact admission; its slot remains until
// the workqueue delivers the retained key.
func (c *sessionStartController) publishCompleteAuthoritativeCensus(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seedGeneration != generation || !c.accepting || c.stopped || c.ctx.Err() != nil {
		return
	}
	for id, admission := range c.admissions {
		if admission.Source != sessionStartAdmissionAntiEntropy || admission.CensusGeneration == 0 || admission.CensusGeneration == generation ||
			c.inFlight[id] == admission.Version {
			continue
		}
		admission.Culled = true
		c.admissions[id] = admission
	}
}

func (c *sessionStartController) seedGenerationCurrent(generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seedGeneration == generation && c.accepting && !c.stopped
}

func (c *sessionStartController) authoritativeCapacity() int {
	capacity := min(c.workers, c.maxDistinct-1)
	if capacity < 1 {
		return 1
	}
	return capacity
}

func (c *sessionStartController) waitForSeedCapacity() bool {
	select {
	case <-c.ctx.Done():
		return false
	case <-c.seedCapacity:
		return true
	}
}

func (c *sessionStartController) signalSeedCapacityLocked() {
	select {
	case c.seedCapacity <- struct{}{}:
	default:
	}
}

func validateSessionStartAdmission(id string, source sessionStartAdmissionSource) error {
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("admitting session start: session id %q is not canonical", id)
	}
	if len(id) > sessionStartAdmissionMaxIDBytes {
		return fmt.Errorf("admitting session start: session id is %d bytes; maximum is %d", len(id), sessionStartAdmissionMaxIDBytes)
	}
	if !strings.ContainsRune(id, '-') {
		return fmt.Errorf("admitting session start: session id %q has no store prefix", id)
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("admitting session start: session id %q contains an invalid character", id)
	}
	switch source {
	case sessionStartAdmissionPendingCreate,
		sessionStartAdmissionExplicitWake,
		sessionStartAdmissionInProcess,
		sessionStartAdmissionSocket,
		sessionStartAdmissionAntiEntropy,
		sessionStartAdmissionWaitDependency,
		sessionStartAdmissionDeadline,
		sessionStartAdmissionOrphanClose,
		sessionStartAdmissionOrphanDrain,
		sessionStartAdmissionStaleCreate,
		sessionStartAdmissionConfigDrift,
		sessionStartAdmissionDuplicateNamed,
		sessionStartAdmissionSleepDrain,
		sessionStartAdmissionProgressStall,
		sessionStartAdmissionStrandedRepair,
		sessionStartAdmissionDrainAdvance,
		sessionStartAdmissionZombieMark,
		sessionStartAdmissionWakeFill:
		return nil
	default:
		return fmt.Errorf("admitting session start %q: unknown source %q", id, source)
	}
}

// RequestAudit records a level-triggered request for an authoritative census.
func (c *sessionStartController) RequestAudit() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.auditPending = true
	c.mu.Unlock()
}

// TakeAuditRequest returns and clears the current authoritative-audit request.
func (c *sessionStartController) TakeAuditRequest() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	requested := c.auditPending
	c.auditPending = false
	return requested
}

// Pending returns the number of distinct admitted keys, including keys
// currently being processed.
func (c *sessionStartController) Pending() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.admissions)
}

// ownsPoolAllocationStart requires the durable token to match before the first
// attempt enters. Once entered, pre-wake may rotate that token, so the retained
// admission remains the exclusion authority through retries until it terminates.
func (c *sessionStartController) ownsPoolAllocationStart(sessionID, instanceToken string) bool {
	instanceToken = strings.TrimSpace(instanceToken)
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	if !ok || admission.PoolAllocation == nil {
		return false
	}
	lease := admission.PoolAllocation
	return lease.SessionID == sessionID &&
		(admission.PoolStartEntered || (instanceToken != "" && lease.InstanceToken == instanceToken))
}

// ownsWaitDependencyStart reports whether a retained exact wait lease owns the
// session's start boundary. It intentionally ignores a caller-supplied token:
// the durable wait operation, not a legacy session projection, is the proof.
func (c *sessionStartController) ownsWaitDependencyStart(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.WaitDependency != nil
}

func (c *sessionStartController) ownsConfiguredDependencyStart(sessionID string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.ConfiguredDependency != nil
}

func (c *sessionStartController) ownsStrictDefaultPoolWakeStart(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.StrictDefaultPoolWake != nil
}

func (c *sessionStartController) ownsConfiguredNamedWakeStart(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.ConfiguredNamedWake != nil
}

func (c *sessionStartController) enterConfiguredDependencyStart(lease configuredDependencyStartLease) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[lease.SessionID]
	if !ok || admission.ConfiguredDependency == nil || *admission.ConfiguredDependency != lease {
		return false
	}
	admission.ConfiguredDependencyEntered = true
	c.admissions[lease.SessionID] = admission
	return true
}

func (c *sessionStartController) enterStrictDefaultPoolWakeStart(lease strictDefaultPoolWakeStartLease) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[lease.SessionID]
	if !ok || admission.StrictDefaultPoolWake == nil || *admission.StrictDefaultPoolWake != lease {
		return false
	}
	admission.StrictDefaultPoolWakeEntered = true
	c.admissions[lease.SessionID] = admission
	return true
}

func (c *sessionStartController) enterConfiguredNamedWakeStart(lease configuredNamedWakeStartLease) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[lease.SessionID]
	if !ok || admission.ConfiguredNamedWake == nil || *admission.ConfiguredNamedWake != lease {
		return false
	}
	admission.ConfiguredNamedWakeEntered = true
	c.admissions[lease.SessionID] = admission
	return true
}

// ownsWaitDependencyWait verifies the full durable wait identity retained by
// a keyed admission. A matching session alone is insufficient because a later
// wait registration must not inherit an earlier operation's exclusion.
func (c *sessionStartController) ownsWaitDependencyWait(wait sessionpkg.WaitInfo) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[wait.SessionID]
	if !ok || admission.WaitDependency == nil {
		return false
	}
	lease := admission.WaitDependency
	return wait.ID == lease.WaitID && wait.SessionID == lease.SessionID && wait.Kind == "deps" && wait.DepMode == lease.DepMode &&
		slices.Equal(wait.DepIDs, lease.DepIDs) && wait.RegisteredEpoch == lease.RegisteredEpoch &&
		(wait.State == waitStatePending || wait.State == waitStateReady && wait.ReadyOwner == string(sessionpkg.WaitReadyOwnerDependency) && wait.ReadyOperation == lease.Operation)
}

// ownsPoolDrainAckStop keeps legacy from entering a stop only while the exact
// drain acknowledgement it names is retained. Unlike a start lease, the
// runtime incarnation never rotates before the destructive stop effect.
func (c *sessionStartController) ownsPoolDrainAckStop(sessionID, instanceToken string) bool {
	instanceToken = strings.TrimSpace(instanceToken)
	if c == nil || sessionID == "" || instanceToken == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	if !ok || admission.PoolDrainAck == nil {
		return false
	}
	lease := admission.PoolDrainAck
	return lease.SessionID == sessionID && lease.InstanceToken == instanceToken
}

// ownsDeadlineStop reports whether the keyed controller currently holds a
// D-DEADLINE admission for this exact key. Legacy's idle-timeout and
// max-session-age arms consult it and yield: both writers fire off the same
// tracker on the same tick, so an acting D-DEADLINE beside a non-yielding
// legacy is a guaranteed double stop, not a race. The admission survives in the
// map from Admit until the handler succeeds or exhausts, so the yield covers
// the whole in-flight window.
func (c *sessionStartController) ownsDeadlineStop(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionDeadline
}

// ownsOrphanClose reports whether the keyed controller currently holds a
// D-ORPHAN close admission for this exact key. Legacy's CloseOrphan and
// CloseFailedCreate arms consult it and yield. This is a SIBLING of
// ownsDeadlineStop rather than a widening of it: each answers "is THIS family's
// effect in flight for this key", and a predicate that answered true for both
// families would make legacy's deadline arms stand down for rows the keyed
// orphan handler owns (and vice versa) — the same silent-disable trap WD.2
// recorded when it declined to reuse sessionStartLegacyExclusionPredicate.
// Retired at WE with the god function.
func (c *sessionStartController) ownsOrphanClose(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionOrphanClose
}

// ownsOrphanDrain reports whether the keyed controller currently holds a
// D-ORPHAN drain admission for this exact key. Legacy's Orphaned drain arm
// consults it and yields. It is a SIBLING of ownsOrphanClose, not a widening of
// it, for the reason WD.2 and WD.3 both recorded: each predicate answers "is
// THIS arm's effect in flight", and one predicate answering for both arms would
// make legacy's close arm stand down for rows the drain arm owns — and the drain
// arm's yield stand down for rows the close arm owns. Retired at WE with the god
// function.
func (c *sessionStartController) ownsOrphanDrain(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionOrphanDrain
}

// ownsStaleCreateRollback reports whether the keyed controller currently holds
// ANY admission for this exact key. Deliberately not gated on
// admission.Source == stale_create: the handler-dispatch seam guards on the
// DURABLE ROW, so any admission that reaches it on a row whose pending-create
// lease has expired runs the keyed rollback — and the controller coalesces
// admissions on a key while keeping the earlier source, which on this family is
// routinely the ordinary pending_create start. Gating the yield on the source
// would reproduce the ga-f7v2ft.125 hole on legacy's side. The other half of the
// yield — "and the row really is a rollback candidate" — is the caller's, and
// legacy already evaluates it before entering its rollback arm.
func (c *sessionStartController) ownsStaleCreateRollback(sessionID string) bool {
	return c.holdsAnyAdmission(sessionID)
}

// ownsStrandedRepair reports whether the keyed controller currently holds ANY
// admission for this exact key. Legacy's dead-pool stranded repair consults it
// and yields the destructive half of that arm — the unassign/reopen and the
// close — while the diagnostic above it keeps firing, because the marker the
// diagnostic stamps IS the keyed family's entry condition.
//
// It takes D-STALE-CREATE's any-admission shape rather than D-DEADLINE's
// source-gated one, for the reason WD.7 recorded and this family meets more
// often: the seam guards on the DURABLE ROW, and the controller coalesces
// admissions on a key while keeping the EARLIER source. A stranded pool member
// is routinely already held by a pool wake when the sweep finds it — the
// acked-member re-point residual (ga-f7v2ft.131) arrives exactly that way — so
// a source-gated yield would let the keyed handler repair through the coalesced
// admission while legacy raced it at the same work beads. The other half of the
// yield ("and the row really is a stranded-repair candidate") is the caller's:
// legacy evaluates pool-freeability, non-liveness, assigned work and the
// partial-store guard before it ever reaches the repair.
func (c *sessionStartController) ownsStrandedRepair(sessionID string) bool {
	return c.holdsAnyAdmission(sessionID)
}

// ownsZombieMark reports whether the keyed controller currently holds ANY
// admission for this exact key. Legacy's zombie-capture block consults it and
// stands down its whole effect — the markProviderTerminalError write, the
// SessionCrashed event and the crash telemetry — because the keyed handler
// already owns that key.
//
// It takes the source-blind shape of ownsStaleCreateRollback and
// ownsStrandedRepair rather than D-DEADLINE's source-gated one, and this family
// meets that case constantly: a zombie row is awake and desired, so it is
// routinely already held by an ordinary wake, drift or deadline admission when
// the sweep finds it, and the controller coalesces on the key while keeping the
// EARLIER source. A source-gated yield would let the keyed handler mark through
// the coalesced admission while legacy raced the same write and fired a second
// SessionCrashed for one incarnation.
func (c *sessionStartController) ownsZombieMark(sessionID string) bool {
	return c.holdsAnyAdmission(sessionID)
}

// holdsAnyAdmission is the source-blind half of the two yields above. It exists
// so the families that deliberately answer on ANY in-flight admission share one
// spelling of that decision instead of two copies that can drift apart.
func (c *sessionStartController) holdsAnyAdmission(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.admissions[sessionID]
	return ok
}

// ownsConfigDriftConverge reports whether the keyed controller currently holds
// ANY admission for this exact key. It takes WD.7's ownership semantics rather
// than WD.2's, and the choice is deliberate on both halves.
//
// Not source-gated, because the handler-dispatch seam guards on the DURABLE ROW
// (seam rule 1) and the controller coalesces admissions on a key while keeping
// the earlier source. Config drift is the family where that bites hardest: a
// config edit drifts the whole fleet at once, so a drift key routinely lands on
// a key that already carries an in_process or anti_entropy admission. Every one
// of those runs the keyed converge ladder, so a source-gated yield would leave
// legacy converging the same row at the same moment — the ga-f7v2ft.125 hole,
// on legacy's side.
//
// The predicate's other half — "and this arm is really keyed's" — is the
// CALLER's, and it is answered twice at each legacy site: the site re-derives
// the drift key it is about to act on, and the yield is installed at the
// CONVERGENCE effects only, with the deferral arms carrying their own bridge.
// Retired at WE with the god function.
func (c *sessionStartController) ownsConfigDriftConverge(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.admissions[sessionID]
	return ok
}

// ownsConfigDriftDefer reports whether the keyed controller currently holds an
// admission that will run the D-DRIFT ladder's A6 half for this exact key. It
// answers identically to ownsConfigDriftConverge, and that is not an accident to
// be collapsed: the family's converge and defer arms ride ONE detected
// condition and ONE admission source because the fact that forks them —
// attachment — is provider I/O the detector may not pay, so an admitted key is
// an admission to the whole ladder. What is genuinely separate is the YIELD:
// each half's legacy counterpart stands down through its own option, which is
// what let the two halves cross in two slices without ever leaving an attached
// session undefended.
func (c *sessionStartController) ownsConfigDriftDefer(sessionID string) bool {
	return c.ownsConfigDriftConverge(sessionID)
}

// ownsDuplicateNamedRetire reports whether the keyed controller currently holds
// a D-DUP admission for this exact key. Legacy's Phase-0b duplicate retire
// consults it and yields that row. Like the deadline seam and unlike the start
// seam, this is not a race to lose: both writers compute the same duplicate set
// from the same durable rows on the same tick, so an un-yielding legacy stops
// the loser's runtime a second time and races a second re-point at the same work
// beads. The admission survives in the map from Admit until the handler succeeds
// or exhausts, so the yield covers the whole in-flight window.
func (c *sessionStartController) ownsDuplicateNamedRetire(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionDuplicateNamed
}

// ownsSleepDrain reports whether the keyed controller currently holds a D-SLEEP
// admission for this exact key. Legacy's awake-scan no-wake arm consults it and
// yields. It is a SIBLING of ownsOrphanDrain rather than a widening of it, for
// the reason WD.2, WD.3 and WD.4 all recorded: each predicate answers "is THIS
// family's effect in flight for this key", and one predicate serving both drain
// families would make legacy's orphan drain stand down for sleep-owned rows and
// legacy's sleep drain stand down for orphan-owned ones. Retired at WE with the
// god function.
func (c *sessionStartController) ownsSleepDrain(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionSleepDrain
}

// ownsDrainAdvance reports whether the keyed controller currently holds a
// D-DRAIN admission for this exact key. Legacy's forward-pass acknowledgement
// block and its drain-advance scan both consult it and yield. It is a
// source-gated SIBLING of ownsSleepDrain and ownsOrphanDrain, not a widening of
// either: the two drain-BEGIN families and this drain-ADVANCE family each answer
// "is THIS family's effect in flight for this key", and one predicate serving
// them all would make each begin arm's legacy counterpart stand down for rows
// the advance owns.
//
// Source-gating is also what keeps the yield NARROW enough to be safe. An agent
// may acknowledge a drain on a row that carries no tracker intent at all; such a
// row is never routed under this source, so legacy keeps its acknowledgement
// block for it. Retired at WE with the god function.
func (c *sessionStartController) ownsDrainAdvance(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionDrainAdvance
}

// ownsProgressStallRecycle reports whether the keyed controller currently holds
// a D-STALL admission for this exact key. Legacy's progress-stall arms consult
// it and skip their restart_requested write. It is a source-gated SIBLING of
// ownsDeadlineStop and ownsDuplicateNamedRetire — not the stale-create form,
// which accepts any admission — because the legacy arm it stands down is a
// destructive recycle and the seam's own guard already re-derives the
// condition. Like the deadline seam this is not a race to lose: both writers
// evaluate the same ladder over the same durable row on the same tick, so an
// un-yielding legacy sets restart_requested behind the keyed handler's back and
// its restart block kills the replacement incarnation the keyed reset just
// committed. Retired at WE with the god function.
func (c *sessionStartController) ownsProgressStallRecycle(sessionID string) bool {
	if c == nil || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[sessionID]
	return ok && admission.Source == sessionStartAdmissionProgressStall
}

// YieldPoolDrainAck releases a retained agent drain acknowledgement only when
// the same durable lease still owns the key. An async pre-stop rollback must
// not erase a newer admission for a replacement runtime incarnation.
func (c *sessionStartController) YieldPoolDrainAck(lease routedWorkPoolDrainAckLease) bool {
	if c == nil || validateRoutedWorkPoolDrainAckLease(lease) != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[lease.SessionID]
	if !ok || admission.PoolDrainAck == nil || *admission.PoolDrainAck != lease {
		return false
	}
	delete(c.admissions, lease.SessionID)
	c.releaseAuthoritativeSlotLocked(lease.SessionID)
	c.queue.Forget(lease.SessionID)
	return true
}

func (c *sessionStartController) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		c.mu.Lock()
		started := c.started
		c.accepting = false
		c.stopped = true
		cancel := c.cancel
		c.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		c.seedWG.Wait()
		if started {
			c.queue.ShutDownWithDrain()
			c.workerWG.Wait()
		} else {
			c.queue.ShutDown()
		}

		c.mu.Lock()
		clear(c.admissions)
		clear(c.seedOutstanding)
		clear(c.inFlight)
		c.mu.Unlock()
	})
}

func (c *sessionStartController) runWorker() {
	defer c.workerWG.Done()
	for {
		key, shutdown := c.queue.Get()
		if shutdown {
			return
		}
		func() {
			defer c.queue.Done(key)
			c.reconcileKey(key)
		}()
	}
}

func (c *sessionStartController) reconcileKey(key string) {
	admission, ok := c.readAdmission(key)
	if !ok {
		c.queue.Forget(key)
		return
	}
	if admission.Culled {
		c.queue.Forget(key)
		c.deleteAdmissionIfVersion(key, admission.Version)
		return
	}
	if err := c.ctx.Err(); err != nil {
		finishedAt := c.now()
		c.queue.Forget(key)
		c.deleteAdmissionIfVersion(key, admission.Version)
		c.observe(sessionStartReconcileResult{
			Admission:  admission,
			Outcome:    sessionStartReconcileCanceled,
			StartedAt:  finishedAt,
			FinishedAt: finishedAt,
			Err:        err,
		})
		return
	}
	if c.beforeMarkInFlightForTest != nil {
		c.beforeMarkInFlightForTest()
	}
	if !c.markInFlightIfVersion(key, admission.Version) {
		c.queue.Forget(key)
		c.deleteAdmissionIfVersion(key, admission.Version)
		return
	}
	defer c.clearInFlightIfVersion(key, admission.Version)
	startedAt := c.now()
	err := c.callReconcile(admission)
	legacyFallback := errors.Is(err, errSessionStartLegacyFallbackRequired)
	legacyFallbackErr := error(nil)
	if legacyFallback {
		if err.Error() != errSessionStartLegacyFallbackRequired.Error() {
			legacyFallbackErr = err
		}
		err = nil
	}
	finishedAt := c.now()
	result := sessionStartReconcileResult{
		Admission:      admission,
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
		LegacyFallback: legacyFallback,
		Err:            errors.Join(err, legacyFallbackErr),
	}

	if c.ctx.Err() != nil {
		c.queue.Forget(key)
		c.deleteAdmissionIfVersion(key, admission.Version)
		result.Outcome = sessionStartReconcileCanceled
		result.Err = c.ctx.Err()
		c.observe(result)
		return
	}
	if err == nil && !legacyFallback && (c.deleteAdmissionIfCoalescedStrictDefaultPoolWakeCompleted(key, admission) ||
		c.deleteAdmissionIfCoalescedConfiguredNamedWakeCompleted(key, admission)) {
		c.queue.Forget(key)
		result.Outcome = sessionStartReconcileSucceeded
		c.observe(result)
		return
	}
	if !c.admissionVersionCurrent(key, admission.Version) {
		c.queue.Forget(key)
		result.Outcome = sessionStartReconcileSuperseded
		c.observe(result)
		return
	}
	if err == nil {
		c.queue.Forget(key)
		c.deleteAdmissionIfVersion(key, admission.Version)
		result.Outcome = sessionStartReconcileSucceeded
		c.observe(result)
		return
	}
	if admission.WaitDependency != nil || admission.ConfiguredDependency != nil || admission.StrictDefaultPoolWake != nil || admission.ConfiguredNamedWake != nil {
		// A retained handoff witness is never exhausted. Forget this queued
		// attempt while retaining its lease; the next exact event or audit
		// admission redrives it without deleting its only ownership proof.
		c.queue.Forget(key)
		result.Outcome = sessionStartReconcileRetrying
		c.observe(result)
		return
	}
	// A retained drain-ack obligation bypasses maxRetries on purpose: a drain-ack
	// is a durable obligation that must never be dropped. It is bounded instead
	// by the DRAIN's own ack-or-timeout deadline — on expiry the admission is
	// deleted, the retained lease dropped and an audit armed, so level-triggered
	// re-detection re-owns the row instead of being fenced out of it forever
	// (ga-f7v2ft.112 ruling 1b).
	if admission.PoolDrainAck != nil || admission.PoolDrainAckUncertain || errors.Is(err, errSessionStartPoolDrainAckPending) {
		expired, refusals, cycleRefusals, crossing := c.boundRetainedDrainAck(key, admission.Version)
		result.DrainAckRefusals = refusals
		result.DrainAckCycleRefusals = cycleRefusals
		result.DrainAckEscalationCrossing = crossing
		if expired {
			c.queue.Forget(key)
			c.releaseAdmission(key, admission.Version)
			result.Outcome = sessionStartReconcileDeadlineExceeded
			c.observe(result)
			return
		}
		if refusals >= drainAckRefusalEscalationThreshold {
			// The named escalated state: the obligation and its fence are
			// retained, but re-examination leaves the hot rate-limited cadence.
			// Forget resets the limiter so a later resolution does not inherit
			// escalation-era backoff.
			c.queue.Forget(key)
			c.queue.AddAfter(key, drainAckEscalatedRetryInterval)
			result.Outcome = sessionStartReconcileDrainAckEscalated
			c.observe(result)
			return
		}
		c.queue.AddRateLimited(key)
		result.Outcome = sessionStartReconcileRetrying
		c.observe(result)
		return
	}
	if c.queue.NumRequeues(key) < c.maxRetries {
		c.queue.AddRateLimited(key)
		result.Outcome = sessionStartReconcileRetrying
		c.observe(result)
		return
	}

	c.queue.Forget(key)
	c.releaseAdmission(key, admission.Version)
	result.Outcome = sessionStartReconcileExhausted
	c.observe(result)
}

// releaseAdmission drops one admission version and arms an authoritative audit
// so the released key is re-detected rather than forgotten. Exhaustion and the
// drain-ack deadline release share it: both give the key back, and the audit is
// what makes that safe.
func (c *sessionStartController) releaseAdmission(key string, version uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, exists := c.admissions[key]; exists && current.Version == version {
		delete(c.admissions, key)
		c.releaseAuthoritativeSlotLocked(key)
		c.auditPending = true
	}
}

// drainAckAdmissionBudget is how long one retained drain-ack obligation may hold
// its ownership fence. It is defaultDrainTimeout because that IS the drain's
// contract: every drain is bounded by ack-or-timeout, and an acknowledgement
// runs the same clock the tracker's own deadline runs on.
const drainAckAdmissionBudget = defaultDrainTimeout

// drainAckRefusalDiagnosticInterval throttles the consecutive-refusal
// escalation. Repeated (false, nil) refusals are indistinguishable from
// transient ones by construction, so the controller never classifies them — it
// reports them periodically and keeps retrying until the deadline bound fires.
const drainAckRefusalDiagnosticInterval = 8

// drainAckRefusalEscalationThreshold is where a retained drain-ack obligation
// stops riding the hot rate-limited retry and enters the named escalated
// state. Three diagnostic intervals of consecutive refusals is well past any
// healthy async-stop window and past the point where another hot retry could
// classify anything new — from here only external evidence (the row changing,
// the runtime dying) resolves the lease, and the slow cadence still observes
// both (ga-f7v2ft.173).
const drainAckRefusalEscalationThreshold = 3 * drainAckRefusalDiagnosticInterval

// drainAckEscalatedRetryInterval is the escalated obligation's re-examination
// cadence: the drain's own deadline budget, because that is the drain
// contract's native clock and each re-examination re-proves provenance and
// liveness from scratch.
const drainAckEscalatedRetryInterval = drainAckAdmissionBudget

// drainAckRefusalStreak is one drain-ack obligation's consecutive-refusal
// history. InstanceToken names the obligation — the incarnation whose drain
// the acknowledgement completes — so the streak survives the deadline
// release's re-detection of the SAME obligation while a fresh incarnation's
// drain starts a fresh streak (ga-f7v2ft.191). An empty token (the
// PoolDrainAckUncertain retention, which could not reconstruct its lease)
// matches whatever streak the session carries: an uncertain re-seed of a
// wedged drain is the same obligation, not a new one. EscalationLogged makes
// the >= threshold crossing announce itself exactly once per obligation.
type drainAckRefusalStreak struct {
	InstanceToken    string
	Count            int
	EscalationLogged bool
}

// boundRetainedDrainAck stamps the drain's own deadline on first retention and
// counts the consecutive refusal. The count is OBLIGATION-scoped
// (drainAckRefusalHistory): it survives version coalescing AND the deadline
// release, so the release → audit → re-detect macro cycle cannot reset the
// escalation bound for the same obligation — while a re-admission carrying a
// different instance token starts a genuinely new obligation's streak at one.
// The obligation identity comes from the acknowledgement lease when one was
// reconstructable, else from the row token the seed stamped on its uncertain
// retention (ga-c9m4g: the lease-less class was otherwise unscopable). It
// reports whether the obligation has outlived its deadline bound, the streak's
// count, the count within the current deadline cycle, and whether this check
// is the streak's single escalation crossing.
func (c *sessionStartController) boundRetainedDrainAck(key string, version uint64) (bool, int, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.admissions[key]
	if !ok || current.Version != version {
		return false, 0, 0, false
	}
	token := ""
	if current.PoolDrainAck != nil {
		token = current.PoolDrainAck.InstanceToken
	}
	if token == "" {
		token = current.PoolDrainAckUncertainToken
	}
	streak := c.drainAckRefusalHistory[key]
	if token != "" && streak.InstanceToken != "" && streak.InstanceToken != token {
		streak = drainAckRefusalStreak{}
		// The inherited deadline (and its cycle anchor) belonged to the
		// previous obligation; a new obligation runs its own drain clock.
		current.DrainAckDeadline = time.Time{}
		current.DrainAckCycleStartRefusals = 0
	}
	if token != "" && streak.InstanceToken == "" {
		streak.InstanceToken = token
	}
	if current.DrainAckDeadline.IsZero() {
		current.DrainAckDeadline = c.now().Add(drainAckAdmissionBudget)
		current.DrainAckCycleStartRefusals = streak.Count
	}
	streak.Count++
	crossing := !streak.EscalationLogged && streak.Count >= drainAckRefusalEscalationThreshold
	if crossing {
		streak.EscalationLogged = true
	}
	c.drainAckRefusalHistory[key] = streak
	current.DrainAckRefusals = streak.Count
	c.admissions[key] = current
	cycleRefusals := streak.Count - current.DrainAckCycleStartRefusals
	return !c.now().Before(current.DrainAckDeadline), streak.Count, cycleRefusals, crossing
}

func (c *sessionStartController) readAdmission(key string) (sessionStartAdmission, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	admission, ok := c.admissions[key]
	return admission, ok
}

func (c *sessionStartController) admissionVersionCurrent(key string, version uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.admissions[key]
	return ok && current.Version == version
}

func (c *sessionStartController) markInFlightIfVersion(key string, version uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.admissions[key]
	if !ok || current.Version != version || current.Culled {
		return false
	}
	if current.PoolAllocation != nil && !current.PoolStartEntered {
		current.PoolStartEntered = true
		c.admissions[key] = current
	}
	c.inFlight[key] = version
	return true
}

func (c *sessionStartController) clearInFlightIfVersion(key string, version uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[key] == version {
		delete(c.inFlight, key)
	}
}

func (c *sessionStartController) deleteAdmissionIfVersion(key string, version uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.admissions[key]; ok && current.Version == version {
		delete(c.admissions, key)
		// Any resolution of the admission ends the obligation's refusal
		// streak; only the deadline release (releaseAdmission) keeps it, so
		// the audit's re-detection continues the count (ga-f7v2ft.173).
		delete(c.drainAckRefusalHistory, key)
		c.releaseAuthoritativeSlotLocked(key)
	}
}

func (c *sessionStartController) deleteAdmissionIfCoalescedStrictDefaultPoolWakeCompleted(key string, completed sessionStartAdmission) bool {
	if completed.StrictDefaultPoolWake == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.admissions[key]
	if !ok || current.Version <= completed.Version || !current.StrictDefaultPoolWakeEntered ||
		current.StrictDefaultPoolWake == nil || *current.StrictDefaultPoolWake != *completed.StrictDefaultPoolWake {
		return false
	}
	delete(c.admissions, key)
	c.releaseAuthoritativeSlotLocked(key)
	return true
}

func (c *sessionStartController) deleteAdmissionIfCoalescedConfiguredNamedWakeCompleted(key string, completed sessionStartAdmission) bool {
	if completed.ConfiguredNamedWake == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.admissions[key]
	if !ok || current.Version <= completed.Version || !current.ConfiguredNamedWakeEntered ||
		current.ConfiguredNamedWake == nil || *current.ConfiguredNamedWake != *completed.ConfiguredNamedWake {
		return false
	}
	delete(c.admissions, key)
	c.releaseAuthoritativeSlotLocked(key)
	return true
}

func (c *sessionStartController) releaseAuthoritativeSlotLocked(key string) {
	delete(c.seedOutstanding, key)
	c.signalSeedCapacityLocked()
}

func (c *sessionStartController) callReconcile(admission sessionStartAdmission) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("session-start reconcile panicked for %s: %v", admission.SessionID, recovered)
			c.writeDiagnostic("%v\n%s\n", err, debug.Stack())
		}
	}()
	return c.reconcile(c.ctx, admission)
}

func (c *sessionStartController) observe(result sessionStartReconcileResult) {
	if c.observer == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			c.writeDiagnostic("session-start result observer panicked for %s: %v\n%s\n", result.Admission.SessionID, recovered, debug.Stack())
		}
	}()
	c.observer(result)
}

func (c *sessionStartController) writeDiagnostic(format string, args ...any) {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	fmt.Fprintf(c.stderr, format, args...) //nolint:errcheck // controller diagnostics must not kill reconciliation
}
