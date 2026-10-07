package main

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/poolplan"
	"github.com/gastownhall/gascity/internal/session"
)

// The planner's admission (CONTRACT v5 P2-P4): one pure function over the
// intents a pass proposed, which replaces the grants and the intent ledger.
// Every input is passed in; it performs no I/O, reads no clock and iterates
// no map for its output (TestDecideIsPure). Admitted effects still re-check
// at their own boundary (R2). A row with an effect in flight is never
// proposed (R5). Unwired: C2c1 calls it from the pass.

// Intent kinds. An in-flight entry and its settlement carry the same names.
const (
	intentStart           = inflightStart       // bringUp (S1): may launch
	intentAdopt           = "adopt"             // S1's adopt: commits a live runtime, never launches
	intentCreate          = inflightCreate      // C1; a named create may reopen its closed row (C2)
	intentRekey           = "rekey"             // S4
	intentZombie          = "zombie"            // A12's classification (S5)
	intentDrainBegin      = "drain-begin"       // D1, D2
	intentDrainBeginFresh = "drain-begin-fresh" // a begin that reads fresh legs (F4) or re-proves idleness
	intentSignal          = "signal"            // D2
	intentSignalFresh     = "signal-fresh"      // a signal that reads fresh legs or revalidates claims
	intentDrainCancel     = "drain-cancel"      // A19
	intentDrainVoid       = "drain-void"        // A19
	intentStop            = "stop"              // the stop verb with its finalize (D3, D4)
	intentClose           = "close"             // A21 (C3)
	intentRollback        = "rollback"          // A10 (C3)
	intentRowMetadata     = "row-metadata"      // A7 (M1)
	intentBaseline        = "baseline"          // A8 (M2)
	intentRowHeal         = "row-heal"          // A6's heals and markers, and other row writes
)

// capClass is the cap an intent counts against (P4); none is per endpoint.
type capClass uint8

const (
	capStarts    capClass = iota + 1 // max_wakes_per_tick bring-up rows, tokens, the endpoint gate
	capCreates                       // createsInFlightCap, and a start for the row admissible now
	capProbing                       // probe_concurrency
	capRowWrites                     // probe_concurrency, counted apart
)

// kindSpec is all admission knows of a kind: a later kind is one line.
type kindSpec struct {
	class     capClass
	bootGated bool // destructive: deferred while the boot gate is closed (P2)
	tokens    int  // debited on admission, never refunded (I9)
}

var intentKinds = map[string]kindSpec{
	intentStart:           {class: capStarts, tokens: 1},
	intentAdopt:           {class: capProbing},
	intentCreate:          {class: capCreates},
	intentRekey:           {class: capProbing},
	intentZombie:          {class: capProbing, bootGated: true},
	intentDrainBegin:      {class: capRowWrites, bootGated: true},
	intentDrainBeginFresh: {class: capProbing, bootGated: true},
	intentSignal:          {class: capRowWrites, bootGated: true},
	intentSignalFresh:     {class: capProbing, bootGated: true},
	intentDrainCancel:     {class: capRowWrites},
	intentDrainVoid:       {class: capRowWrites},
	intentStop:            {class: capProbing, bootGated: true},
	intentClose:           {class: capProbing, bootGated: true},
	intentRollback:        {class: capProbing, bootGated: true},
	intentRowMetadata:     {class: capRowWrites},
	intentBaseline:        {class: capRowWrites},
	intentRowHeal:         {class: capRowWrites},
}

// createsInFlightCap bounds running creates (P4, C1); the rest are P3's
// flat deadlines.
const (
	createsInFlightCap     = 8
	startDeadlineSlack     = 10 * time.Second
	providerEffectDeadline = 60 * time.Second
	rowWriteDeadline       = 30 * time.Second
)

// deadline is the effect's deadline (P3), by class alone with no exception:
// a start startup_timeout + 10s, a row write 30s, every other effect 60s.
func (k kindSpec) deadline(startupTimeout time.Duration) time.Duration {
	switch k.class {
	case capStarts:
		return startupTimeout + startDeadlineSlack
	case capRowWrites:
		return rowWriteDeadline
	}
	return providerEffectDeadline
}

// Deferral causes: every deferred intent carries one for the trace (R6).
const (
	causeUnknownKind    = "unknown-kind"
	causeRowRepeated    = "row-repeated" // a second intent for one row in a pass (R5)
	causeBootGate       = "boot-gate"
	causeRowBackoff     = "backoff"
	causeSwapPause      = "swap-pause"
	causeAwaitingBudget = "awaiting-budget"
	causeCityCap        = "city-cap"
	causeEndpointGate   = "endpoint-gate"
	causeCreateCap      = "create-cap"
	causeFairShare      = "fair-share"
	causeProbeCap       = "probe-cap"
	causeRowWriteCap    = "row-write-cap"
	// causeFinalizePrefix prefixes the cause of a refusal the stop verb's
	// finalize records (C6b2): a finalize waits on its own backoff only.
	causeFinalizePrefix = "finalize/"
)

// intent is one effect the pass proposes for admission.
type intent struct {
	Kind     string
	Key      rowKey      // the row; zero for a create
	Reason   string      // the arm's reason, for the trace
	Endpoint endpointKey // config-only; a start or create is gated on it
	Rank     time.Time   // orders starts oldest first: wakeFairnessTime (START-009)
	// Create is a create's plan; Floor marks a pool create that satisfies a
	// min_active_sessions floor, for the fair share.
	Create createPlan
	Floor  bool
	Basis  rowBasis // the incarnation the pass saw; the effect's CAS re-checks it
	// Finalize marks the stop verb proposed for a row whose runtime reads
	// gone: it confirms and finalizes, and stops nothing (A4, D3).
	Finalize bool
	// Patch is a row write's patch and Event what it records once it lands.
	// The row-write effect re-decides on the fresh row and writes the
	// re-decided intent's (R2).
	Patch session.MetadataPatch
	Event *events.Event
	// Deadline is set on admission (P3); Cause on deferral.
	Deadline time.Time
	Cause    string
}

func (it intent) named() bool { return it.Create.Named != nil }

// finalizesOnly is the stop verb's finalize, the one intent a row backoff
// does not defer (P4), unless the finalize's own refusal recorded it.
func (it intent) finalizesOnly(r backoffRecord) bool {
	return it.Kind == intentStop && it.Finalize && !strings.HasPrefix(r.Cause, causeFinalizePrefix)
}

// admitInput is everything admit reads.
type admitInput struct {
	Now time.Time
	Cfg *config.City // the caps, the bucket's patrol interval, startup_timeout
	// Bucket and FairSeed are planner state, as the last pass left them.
	Bucket   bucketState
	FairSeed uint64
	InFlight inflightView
	BringUp  []bringUpRow // sessionCensus.BringUp
	// Endpoints are the breaker readings: the empty key is closed, and a key
	// without a reading admits nothing.
	Endpoints map[endpointKey]endpointView
	Backoff   map[string]backoffRecord
	// Paused is a provider swap in progress (P7); BootOpen the boot gate (P2).
	Paused   bool
	BootOpen bool
}

// admitResult is admit's output: the intents to submit, the rest with their
// causes, and the planner state for the next pass. NextToken, when a start
// waited for a token, is when the bucket next holds one: a pass to schedule.
type admitResult struct {
	Admitted  []intent
	Deferred  []intent
	Bucket    bucketState
	FairSeed  uint64
	NextToken time.Time
}

// admission is one admit's working state.
type admission struct {
	in       admitInput
	capacity int
	probeCap int
	startup  time.Duration
	interval time.Duration
	bucket   bucketState
	seen     map[rowKey]bool // rows with an intent this pass
	starved  bool            // a start waited for a token
	// inFlight is the city in-flight count, and counted the rows holding a
	// slot in it.
	inFlight    int
	counted     map[rowKey]bool
	outstanding map[endpointKey]int
	// parkedOn is the pending-create rows per endpoint: behind a probe gate
	// they wait for the probe, so a create would only park another.
	parkedOn map[endpointKey]int
	running  map[capClass]int
	// createTokens are the tokens admitted creates hold for their rows'
	// first starts this pass; they debit nothing.
	createTokens int
	demands      []poolplan.Demand
	budget       *poolplan.CreateBudget
	budgetSet    bool
	poolAdmitted bool
}

// admit is the pass's admission (P4). In order: starts, in LRU rank with
// probe rotation inside each endpoint; adopts; named creates, then pool
// creates in fair-share order; the other probing effects; row writes. Each
// intent is checked against the boot gate, its row's backoff, the swap
// pause, then its class's caps.
func admit(in admitInput, intents []intent) admitResult {
	cfg := in.Cfg
	if cfg == nil {
		cfg = &config.City{}
	}
	a := &admission{
		in:       in,
		capacity: cfg.Daemon.MaxWakesPerTickOrDefault(),
		probeCap: cfg.Daemon.ProbeConcurrencyOrDefault(),
		startup:  cfg.Session.StartupTimeoutDuration(),
		interval: cfg.Daemon.PatrolIntervalDuration(),
		seen:     make(map[rowKey]bool),
		parkedOn: make(map[endpointKey]int),
		running:  make(map[capClass]int),
	}
	if a.startup <= 0 {
		a.startup = 60 * time.Second // config's default startup_timeout
	}
	a.bucket = in.Bucket.refill(in.Now, a.capacity, a.interval)
	a.inFlight, a.counted = cityInFlight(in.InFlight, in.BringUp, a.gate)
	a.outstanding = endpointOutstanding(in.InFlight)
	for _, e := range in.InFlight.Entries {
		spec, ok := intentKinds[e.Kind]
		if !ok {
			spec.class = capProbing // a running kind this table does not know
		}
		if !e.Ambiguous {
			a.running[spec.class]++
		}
	}
	for _, r := range in.BringUp {
		if r.PendingCreate {
			a.parkedOn[r.Endpoint]++
		}
	}
	res := admitResult{}
	for _, it := range a.order(intents) {
		if it.Cause = a.admitOne(&it); it.Cause != "" {
			res.Deferred = append(res.Deferred, it)
			continue
		}
		res.Admitted = append(res.Admitted, it)
	}
	res.Bucket, res.FairSeed = a.bucket, in.FairSeed
	if a.starved {
		res.NextToken = a.bucket.nextToken(a.capacity, a.interval)
	}
	if a.poolAdmitted {
		// The seed advances only when a fair-share create was admitted, so
		// passes without tokens do not decide the rotation.
		res.FairSeed++
	}
	return res
}

// order sorts intents into admission order, stably, and gathers the pool
// creates' fair-share demand.
func (a *admission) order(intents []intent) []intent {
	tier := func(it intent) int {
		switch spec, ok := intentKinds[it.Kind]; {
		case !ok:
			return 6
		case spec.class == capStarts:
			return 0
		case it.Kind == intentAdopt:
			return 1
		case spec.class == capCreates && it.named():
			return 2
		case spec.class == capCreates:
			return 3
		case spec.class == capProbing:
			return 4
		}
		return 5
	}
	out := slices.Clone(intents)
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if tx, ty := tier(x), tier(y); tx != ty || tx != 0 {
			return tx < ty
		}
		if !x.Rank.Equal(y.Rank) {
			return x.Rank.Before(y.Rank)
		}
		return x.Key.Leg < y.Key.Leg || (x.Key.Leg == y.Key.Leg && x.Key.ID < y.Key.ID)
	})
	starts := 0
	for starts < len(out) && tier(out[starts]) == 0 {
		starts++
	}
	rotateProbeSlots(out[:starts], func(it intent) endpointKey { return it.Endpoint },
		func(k endpointKey, it intent) int { return a.in.Endpoints[k].Refusals[it.Key.ID] })
	for _, it := range out[starts:] {
		if tier(it) != 3 {
			continue
		}
		i := slices.IndexFunc(a.demands, func(d poolplan.Demand) bool { return d.Template == it.Create.Template })
		if i < 0 {
			i, a.demands = len(a.demands), append(a.demands, poolplan.Demand{Template: it.Create.Template})
		}
		a.demands[i].FreshCreates++
		a.demands[i].HasFloor = a.demands[i].HasFloor || it.Floor
	}
	return out
}

// admitOne admits it, counting it and setting its deadline, or returns the
// cause it is deferred for.
func (a *admission) admitOne(it *intent) string {
	if it.Key.ID != "" {
		if a.seen[it.Key] {
			return causeRowRepeated
		}
		a.seen[it.Key] = true
	}
	spec, ok := intentKinds[it.Kind]
	backoff := a.in.Backoff[rowBackoffKey(it.Key)]
	switch {
	case !ok:
		return causeUnknownKind
	case spec.bootGated && !a.in.BootOpen:
		return causeBootGate
	case it.Key.ID != "" && !it.finalizesOnly(backoff) && backoff.live(a.in.Now):
		return causeRowBackoff
	case a.in.Paused && (spec.class == capStarts || spec.class == capCreates):
		return causeSwapPause
	}
	switch spec.class {
	case capStarts:
		held, cause := a.startCause(it.Key, it.Endpoint, a.bucket.Tokens, 0)
		if a.starved = a.starved || cause == causeAwaitingBudget; cause != "" {
			return cause
		}
		a.countBringUp(it.Key, it.Endpoint, held)
	case capCreates:
		if cause := a.createCause(*it); cause != "" {
			return cause
		}
		a.countBringUp(rowKey{}, it.Endpoint, false)
		a.createTokens++
	case capProbing:
		if a.running[capProbing] >= a.probeCap {
			return causeProbeCap
		}
	case capRowWrites:
		if a.running[capRowWrites] >= a.probeCap {
			return causeRowWriteCap
		}
	}
	a.bucket.Tokens -= spec.tokens
	a.running[spec.class]++
	it.Deadline = a.in.Now.Add(spec.deadline(a.startup))
	return ""
}

// createCause is why a create may not be admitted, or "": the create cap,
// a start for its row not admissible now (behind a probe gate the
// endpoint's pending rows count as outstanding), or, for a pool create,
// its template's fair share of the tokens the named creates left.
func (a *admission) createCause(it intent) string {
	left := a.bucket.Tokens - a.createTokens
	if a.running[capCreates] >= createsInFlightCap {
		return causeCreateCap
	}
	if _, cause := a.startCause(rowKey{}, it.Endpoint, left, a.parkedOn[it.Endpoint]); cause != "" {
		return cause
	}
	if it.named() {
		return ""
	}
	if !a.budgetSet {
		// The fair share divides what the create cap leaves too, or the cap
		// would hand every slot to the first template.
		a.budget, a.budgetSet = poolplan.NewCreateBudget(min(left, createsInFlightCap-a.running[capCreates])), true
		a.budget.ConfigureFairShare(a.demands, a.in.FairSeed)
	}
	if !a.budget.TryClaim(it.Create.Template) {
		return causeFairShare
	}
	a.poolAdmitted = true
	return ""
}

// startCause is admitStart over the pass's counts: tokens; the city cap
// over the bring-up rows, or, for a row already counted, over the running
// starts, since its start takes no further slot (P4) and a cap lowered by a
// reload must not wedge it; and the endpoint's gate with its outstanding
// effects plus extra.
func (a *admission) startCause(k rowKey, ep endpointKey, tokens, extra int) (held bool, cause string) {
	held = k.ID != "" && a.counted[k]
	inFlight := a.inFlight
	if held {
		inFlight = a.running[capStarts]
	}
	gate, _ := a.gate(ep)
	return held, admitStart(bucketState{Tokens: tokens}, inFlight, a.capacity, gate, a.outstanding[ep]+extra)
}

// countBringUp counts an admitted start or create: outstanding on its
// endpoint, and in the city in-flight count unless its row already holds a
// slot there.
func (a *admission) countBringUp(k rowKey, ep endpointKey, held bool) {
	a.outstanding[ep]++
	if held {
		return
	}
	a.inFlight++
	if k.ID != "" {
		a.counted[k] = true
	}
}

// gate is k's breaker reading and whether one exists: the empty key is
// closed, and any other key without a reading reads shut.
func (a *admission) gate(k endpointKey) (endpointGate, bool) {
	if k == "" {
		return gateClosed, true
	}
	v, ok := a.in.Endpoints[k]
	return v.Gate, ok
}
