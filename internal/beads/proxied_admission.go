package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// ProviderOps is the ENTIRE bd-verb surface admission may reach for.
//
// Two verbs, both of them provider-owned lifecycle operations gc already runs
// elsewhere. The narrowness is the point: bd owns the proxy and its Dolt child,
// so admission's escalation ladder must be expressible as "ask bd to make its
// proxy healthy" and nothing else. An interface with a third method would be an
// invitation to reach for a bd read, and a bd read is the fork this whole lane
// exists to remove.
//
// Neither verb may spawn anything gc owns. Ping is bd's own `ping`, which
// adopts or restarts bd's proxy; Recover is the provider's `recover`. gc never
// execs dolt, and never starts a proxy itself.
type ProviderOps interface {
	// Ping asks bd to make the scope's proxy healthy, and reports whether it
	// could.
	Ping(ctx context.Context, scopeRoot string) error
	// Recover asks bd to retire and re-establish a proxy that is listening but
	// not answering.
	Recover(ctx context.Context, scopeRoot string) error
}

// GenerationSet is a process-local set of proxy generations, with a TTL.
//
// Two of them bound the escalation ladder across the many opens a single gc
// command performs: one records generations a ping has already proven healthy,
// so a later open in the same process does not buy the same answer twice, and
// one records generations a recover has already been spent on, so a proxy that
// stays sick is declared a zombie instead of being recovered in a loop.
//
// It is a set of generation STRINGS rather than of PoolKeys because the
// question is about a proxy process, and one proxy legitimately serves several
// databases — a rig sharing its city's proxy root differs from the city in the
// database alone, and recovering for the rig would otherwise look unrecovered
// to the city.
//
// # Why the entries expire (council A-F5)
//
// They did not, and the set is process-lifetime. On a one-shot command that is
// the same thing; on a controller or an api server it is not, and the
// difference is an operator-visible fault.
//
// A long-running gc pings a scope at boot, which records that generation as one
// a rung has been spent on. Two hours later that generation's Dolt child is
// OOM-killed: the endpoint accepts and never greets, the ladder walks its three
// probes, reaches the ping rung, finds the generation already "spent" — and
// falls straight through to the RECOVER rung. Recover is the provider script's
// `provider_owned_retire_local_dolt` (`bd dolt stop`) plus `bd ping`, and the
// script's own comment says that for a city root this takes down the one proxy
// and Dolt child serving hq and every other rig, under live agents. A plain
// `bd ping` — which the script documents as blocking until the Dolt child
// reports ready — would have fixed it without cycling anything.
//
// The design's memo (3.3 step 2) exists to dedupe the many opens of ONE
// command. generationMemoTTL is that window made explicit: long enough that no
// single command or burst of opens buys the same answer twice, short enough
// that an hours-old ping is not mistaken for a rung this incident has spent.
type GenerationSet struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	now  func() time.Time
}

// generationMemoTTL is how long a spent rung stays spent. See GenerationSet.
const generationMemoTTL = 5 * time.Minute

// NewGenerationSet returns an empty set with the default TTL.
func NewGenerationSet() *GenerationSet {
	return &GenerationSet{seen: map[string]time.Time{}, ttl: generationMemoTTL, now: time.Now}
}

// Add records a generation and reports whether it was NEW — absent, or recorded
// longer ago than the TTL — which is how a caller spends an escalation rung
// exactly once per incident rather than once per process.
func (s *GenerationSet) Add(generation string) bool {
	if s == nil || generation == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	now := s.clock()
	if at, ok := s.seen[generation]; ok && !s.expired(at, now) {
		return false
	}
	s.seen[generation] = now
	return true
}

// Release drops a generation, so the rung it stood for may be spent again.
//
// It is what keeps a rung that did not actually buy anything from counting: a
// provider verb that failed for a reason of gc's own — the lifecycle semaphore,
// the op budget — learned nothing about the proxy, and recording it would poison
// the scope for the rest of the TTL on the strength of gc's own contention.
func (s *GenerationSet) Release(generation string) {
	if s == nil || generation == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, generation)
}

// Has reports whether a generation is in the set and has not expired.
func (s *GenerationSet) Has(generation string) bool {
	if s == nil || generation == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.seen[generation]
	return ok && !s.expired(at, s.clock())
}

func (s *GenerationSet) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *GenerationSet) expired(at, now time.Time) bool {
	ttl := s.ttl
	if ttl <= 0 {
		ttl = generationMemoTTL
	}
	return now.Sub(at) >= ttl
}

// The process-local defaults, used when a caller supplies no sets. They are
// package-level because "once per process" is exactly the scope the design
// asks for, and a per-call set would silently turn "recover once per
// generation" into "recover on every open".
var (
	defaultObservedGenerations  = NewGenerationSet()
	defaultRecoveredGenerations = NewGenerationSet()
)

// Pin is an admission pass: proof that a specific proxy generation was found
// healthy, serving the named database, at cursors equal to this binary's.
//
// Every field is unexported and there is no exported constructor, so the only
// way to hold a non-zero Pin is to have called Admit and had it succeed. That
// is deliberate: the proxied opener takes a Pin, so a caller cannot reach the
// opener without passing the gate, and "somebody built the env map by hand"
// stops being a reachable state rather than a reviewed one.
type Pin struct {
	admitted bool
	key      proxyendpoint.PoolKey
	// scopeRoot is the WORKSPACE this pin admitted, which is not root: one bd
	// proxy root legitimately serves several scopes (a rig sharing its city's
	// proxy differs from the city in the database alone). The pin memo is keyed
	// on it, so anything that must invalidate a memoized pass — the mutation
	// bracket's generation check, the guard tick — needs it back out.
	scopeRoot string
	root      string
	database  string
	idle      proxyendpoint.IdlePolicy
	cursors   proxyendpoint.Cursors
	evidence  proxyendpoint.Evidence
	// head is the database's HEAD commit hash as the admitting probe session
	// saw it, or "" for a pin served from the memo (see withoutHead). A fresh
	// probe always carries one: it is read on the session's first statement,
	// and a session that cannot read it is not served.
	//
	// It is not admission evidence: no decision above is made from it, and a
	// pin with no head is a perfectly good pin. It is carried so the OPENER can
	// re-read the same value once the library open has returned and see whether
	// the open moved HEAD. See ProxiedHeadUnmoved.
	head string
}

// Admitted reports whether this is a real pass rather than the zero value.
func (p Pin) Admitted() bool { return p.admitted }

// PoolKey is the generation-and-database identity this pin admitted.
func (p Pin) PoolKey() proxyendpoint.PoolKey { return p.key }

// Root is bd's proxy root the record was read from.
func (p Pin) Root() string { return p.root }

// ScopeRoot is the workspace this pin admitted. See the field comment for why it
// is not the same thing as Root.
func (p Pin) ScopeRoot() string { return p.scopeRoot }

// Database is the Dolt database the pin admitted.
func (p Pin) Database() string { return p.database }

// Port is the proxy's loopback data port.
func (p Pin) Port() int { return p.key.Port }

// Generation renders the proxy process generation.
func (p Pin) Generation() string { return p.key.Generation() }

// IdlePolicy is the resolved idle rule of the proxy this pin admitted.
func (p Pin) IdlePolicy() proxyendpoint.IdlePolicy { return p.idle }

// Cursors are the migration cursors the probe read straight off disk.
func (p Pin) Cursors() proxyendpoint.Cursors { return p.cursors }

// Evidence is the strongest liveness proof the inspection obtained.
func (p Pin) Evidence() proxyendpoint.Evidence { return p.evidence }

// Head is the database's HEAD commit hash at admission time, or "" when it was
// not observed. See the field comment: it is an observation the opener re-reads
// after the library open, not a fact admission decided on.
func (p Pin) Head() string { return p.head }

// withoutHead returns the pin with its HEAD observation dropped.
//
// A memoized pin is served for up to proxiedPinMemoTTL, and any bd client can
// commit to that database in the meantime, so the hash it carries stops being a
// statement about what THIS open did the moment it is reused. Dropping it makes
// the post-open comparison decline to conclude rather than accuse another
// process's ordinary write, which is the one way a belt-and-braces check can do
// damage.
//
// What it gives up, stated: an open served from the memo is NOT checked, for up
// to the memo's TTL. That is bounded rather than open-ended because the writes in
// question are reconciles — the fresh, checked open at the head of a memo window
// performs them, and the memoized opens behind it find nothing left to do — and
// because a head_moved verdict forgets the memo, so the open after an incident
// probes fresh and is checked again.
func (p Pin) withoutHead() Pin {
	p.head = ""
	return p
}

// Report projects the pin onto the factory's diagnostic shape, so the opener
// does not restate facts admission already established.
func (p Pin) Report() ProxiedOpenReport {
	return ProxiedOpenReport{
		Endpoint: ProxiedEndpointStamp{
			Port:       p.key.Port,
			PID:        p.key.PID,
			Generation: p.key.Generation(),
		},
		Evidence:   p.evidence.String(),
		IdlePolicy: p.idle.String(),
		Cursors:    p.cursors,
	}
}

// AdmissionInput is everything Admit needs, with every effect that is not a
// plain file read injected.
//
// The file reads are NOT injected, on purpose. ProviderRoot, ReadOwnership,
// ReadSidecar and Inspect are the parity contract with bd: they resolve the
// same root bd resolves and decode the record bd wrote, and a test that stubbed
// them would prove gc agrees with a fake. The three effects that are injected
// are the ones a test cannot afford to perform — a process table, a TCP
// session, and a bd fork — and the clock.
type AdmissionInput struct {
	// ScopeRoot is the workspace whose proxy is being admitted.
	ScopeRoot string
	// Database is the Dolt database to admit. It is required: the cursors are
	// DATABASE()-scoped, so a probe with none selected reports served with both
	// cursors at zero, which would pass the gate against nothing at all.
	Database string

	// ProcessTable answers the liveness half of the inspection.
	ProcessTable proxyendpoint.ProcessTable
	// Probe runs one session against the proxy's data port.
	Probe func(ctx context.Context, ep proxyendpoint.Endpoint, database string) proxyendpoint.ProbeResult
	// Ops is the bd-verb surface. A nil Ops means the ladder cannot escalate,
	// which is a legitimate configuration (a caller that wants admission to be
	// read-only); the affected verdicts simply come back unescalated.
	Ops ProviderOps

	// LongLived says the store will be held for the process lifetime. It
	// decides two things: whether a finite idle policy is admissible at all,
	// and whether a draining proxy is waited out or refused immediately.
	LongLived bool

	// Observed records generations a ping has already proven healthy, and
	// Recovered records generations a recover has already been spent on. Nil
	// uses the process-local defaults.
	Observed  *GenerationSet
	Recovered *GenerationSet

	// Now and Sleep are the clock. Sleep must return the context's error when
	// the budget expires rather than sleeping through it.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// SkipMemo bypasses the process-local pin memo for this call. The guard
	// tick sets it: a tick that re-admitted out of the memo it populated would
	// be asserting that nothing changed by reading its own answer.
	SkipMemo bool
}

const (
	// admissionNoGreetingAttempts is how many probe sessions a silent endpoint
	// gets before gc spends a bd verb on it.
	admissionNoGreetingAttempts = 3
	// admissionNoGreetingSpacing spaces those attempts so the ladder spans at
	// least two seconds. A proxy mid-restart accepts and stays silent for a
	// beat, and escalating inside that beat would fork bd for a proxy that was
	// about to answer.
	admissionNoGreetingSpacing = time.Second
	// admissionDrainPoll is the drain loop's cadence for the cheap half: two
	// file reads, no socket.
	admissionDrainPoll = 250 * time.Millisecond
	// admissionDrainProbesEvery is how many polls pass between re-probes of the
	// data port. Eight polls is two seconds, which is the probe's own session
	// budget — so the loop never has more than one probe's worth of staleness
	// and never costs bd's idle watcher more than one accepted connection per
	// two seconds of waiting.
	admissionDrainProbesEvery = 8
	// admissionDrainCeiling caps the drain wait however long the caller's
	// context is. A proxy that has been draining for a minute is not draining.
	admissionDrainCeiling = 60 * time.Second
)

// Admit decides whether gc may open the linked library against the database bd
// serves for this scope, and pins the generation it may open against.
//
// The order is the order the evidence gets more expensive, and it matters:
//
//  1. resolve bd's proxy root the way bd resolves it;
//  2. read the ownership record, then the sidecar — file reads, no dial;
//  3. inspect: decode, validate, and ask the process table. A record that fails
//     validation (a foreign root_id, a pre-schema-2 document) is refused HERE,
//     with no dial ever spent on it;
//  4. resolve the idle policy, because a finite-idle proxy cannot host a
//     long-lived handle whatever its health;
//  5. probe, once, for a live endpoint;
//  6. gate the probe's raw on-disk cursors against this binary's pinned pair.
//
// Step 6 runs BEFORE the library open and not after, and that is the whole
// reason the cursors come from the probe rather than from a library read: the
// linked library runs CheckForwardDrift at every open, so a mismatch discovered
// after the open would be the library's untyped error — on a database gc had
// already connected to and might already have migrated. Discovered here it is
// gc's typed verdict, and nothing was opened.
func Admit(ctx context.Context, in AdmissionInput) (Pin, error) {
	in = in.withDefaults()
	if in.ScopeRoot == "" {
		return Pin{}, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord, "admission was given no scope root", nil)
	}
	if in.Database == "" {
		// The cursors are DATABASE()-scoped. A probe with none selected reads
		// zeros and calls them evidence.
		return Pin{}, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord, "admission was given no database name", nil)
	}

	root, err := proxyendpoint.ProviderRoot(in.ScopeRoot)
	if err != nil {
		return Pin{}, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord, "resolving bd's proxy root", err)
	}
	beadsDir := filepath.Join(in.ScopeRoot, ".beads")

	if !in.SkipMemo {
		if pin, ok := lookupProxiedPin(in.ScopeRoot, in.Database, in.LongLived, root, beadsDir, in.Now()); ok {
			// The HEAD observation does not survive the memo: see withoutHead.
			return pin.withoutHead(), nil
		}
	}

	// The ladder re-runs admission after each escalation rung. The rung
	// counters (Observed, Recovered) are what bound it, not this number; the
	// cap exists so a pathological record that keeps changing under us cannot
	// spin.
	const maxRungs = 4
	var lastErr error
	for attempt := 0; attempt < maxRungs; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Pin{}, NewNonTerminalProxiedVerdictError(ProxiedVerdictBudgetExhausted,
				"admission budget expired", ctxErr)
		}
		pin, retry, admitErr := admitOnce(ctx, in, root, beadsDir)
		if admitErr == nil {
			// The scope admitted, so whatever incident the absent-record rung
			// was spent on is OVER. Releasing it is what makes that rung "once
			// per incident" rather than once per process (council A-F6): a
			// missing record has no generation to key on, so without this the
			// SECOND `bd dolt stop` in a long-running process was never
			// recovered.
			in.Observed.Release(absentRecordPingKey(in.ScopeRoot))
			if !in.SkipMemo {
				storeProxiedPin(in.ScopeRoot, in.Database, in.LongLived, root, beadsDir, pin, in.Now())
			}
			return pin, nil
		}
		lastErr = admitErr
		if !retry {
			return Pin{}, admitErr
		}
	}
	return Pin{}, lastErr
}

// admitOnce is one pass of the ladder. retry reports that an escalation rung
// was spent and the caller should re-read everything from disk: a rung that
// worked changed the very record the decision was made from.
func admitOnce(ctx context.Context, in AdmissionInput, root, beadsDir string) (Pin, bool, error) {
	own, err := proxyendpoint.ReadOwnership(root)
	switch {
	case errors.Is(err, proxyendpoint.ErrNoProxy):
		// bd removes the record on an orderly exit, so an absent one is a
		// stopped proxy rather than a fault. This is one of the four states
		// worth a bd verb.
		return in.escalateWithPing(ctx, "", "no proxy record", err)
	case err != nil:
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord, "reading the ownership record", err)
	case own.Kind != proxyendpoint.RecordKind:
		// The dolt-backend record bd writes beside the proxy's own. Reading it
		// as the proxy record would dial bd's Dolt child directly.
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord,
			fmt.Sprintf("ownership record kind is %q, want %q", own.Kind, proxyendpoint.RecordKind), nil)
	}

	sidecar, err := proxyendpoint.ReadSidecar(beadsDir)
	if err != nil {
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord, "reading the proxied sidecar", err)
	}

	ep := proxyendpoint.Inspect(root, in.ProcessTable)
	key := proxyendpoint.NewPoolKey(ep.Record, in.Database)
	switch ep.Verdict {
	case proxyendpoint.VerdictLive:
		// Fall through to the idle rule and the probe.
	case proxyendpoint.VerdictLegacySchema:
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictLegacySchema,
			"the proxy record predates the schema that carries a birth token, so no generation can be established", ep.Err)
	case proxyendpoint.VerdictNotOurs, proxyendpoint.VerdictForeignProcess:
		// Decided from the record and the process table alone. No dial is ever
		// spent on a record that does not validate for this root: that is the
		// whole protection against a copied proxy.pid pointing gc at somebody
		// else's database.
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictNotOurs, ep.Verdict.String(), ep.Err)
	case proxyendpoint.VerdictMalformed:
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictNoOwnershipRecord, "malformed proxy record", ep.Err)
	case proxyendpoint.VerdictDead, proxyendpoint.VerdictBirthMismatch:
		return in.escalateWithPing(ctx, key.Generation(), ep.Verdict.String(), ep.Err)
	default:
		// VerdictUndetermined: a read the proof depends on failed. gc declines
		// rather than guesses, and spends nothing: a ping cannot make an argv
		// readable, and a dial on an unproven record is the one thing this
		// package refuses to do. Non-terminal, so the next open re-inspects.
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictProxyGone,
			"proxy liveness undetermined: "+ep.Verdict.String(), ep.Err)
	}

	idle := proxyendpoint.ResolveIdlePolicy(sidecar, ep.Liveness.IdlePolicy)
	if idle.Kind == proxyendpoint.IdleFinite && in.LongLived {
		// The deliberate, doctor-visible PR2 deviation. bd retires a
		// finite-idle proxy AND its Dolt child after a quiet window, so a
		// handle gc held across one would be pinned to a process bd has
		// decided to stop. The design's answer is a store re-opened per
		// reconcile pass; PR2's is to keep BdStore for such a scope and say so
		// in the payload.
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictIdlePolicyFinite,
			"long-lived native open refused: proxy idle policy is "+idle.String(), nil)
	}

	probe := in.Probe(ctx, ep, in.Database)
	switch probe.Outcome {
	case proxyendpoint.ProbeServed:
		// The gate reads the probe's REALITY as well as its cursors: the raw
		// ignored cursor is not the number the linked library acts on, and a
		// gate that compared it admitted databases the library would migrate.
		// See CursorsMatchPinned (council A-F2).
		ok, lane, dir := CursorsMatchPinned(probe.Cursors, probe.Reality)
		if !ok {
			detail := fmt.Sprintf("database %s, this binary pins main=%d ignored=%d",
				probe.Cursors, SchemaCursorMain, SchemaCursorIgnored)
			if clamp := probe.Reality.String(); clamp != "" {
				detail += "; " + clamp
			}
			return Pin{}, false, NewSchemaSkewVerdictError(lane, dir, detail)
		}
		return Pin{
			admitted:  true,
			key:       key,
			scopeRoot: in.ScopeRoot,
			root:      root,
			database:  in.Database,
			idle:      idle,
			cursors:   probe.Cursors,
			evidence:  ep.Liveness.Evidence,
			head:      probe.Head,
		}, false, nil

	case proxyendpoint.ProbeRefused:
		return in.drain(ctx, ep, root, key)

	case proxyendpoint.ProbeAcceptedNoGreeting:
		return in.escalateZombie(ctx, root, ep, key)

	default:
		// ProbeUnknown. IsIndeterminate is consulted before anything else,
		// because the probe's own deadline and the caller's cancellation are
		// facts about US: neither is evidence about a proxy, and classifying
		// one as an endpoint state is how a loaded box demotes a healthy city.
		if proxyendpoint.IsIndeterminate(probe.Err) {
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBudgetExhausted,
				"the probe's own clock ended the session; nothing was learned about the endpoint", probe.Err)
		}
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictBackendUnreachable, "probe outcome unknown", probe.Err)
	}
}

// drain handles a data port the kernel refused.
//
// Refused on a record that is STILL live and still the same generation is a
// proxy on its way down: the supervisor has closed its listener and has not yet
// removed the record. A long-lived open waits it out, because the alternative
// is demoting a controller store for a two-second shutdown. A one-shot does
// not: the command in front of it would rather run on BdStore now.
//
// Expiry returns draining NON-TERMINAL, and that is B's trap made safe. The
// budget running out says nothing about the proxy, so a terminal verdict here
// would permanently demote a handle over a slow box.
func (in AdmissionInput) drain(ctx context.Context, ep proxyendpoint.Endpoint, root string, key proxyendpoint.PoolKey) (Pin, bool, error) {
	if !in.LongLived {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
			"data port refused; a one-shot open takes the bd front door rather than waiting", nil)
	}
	deadline := in.Now().Add(admissionDrainCeiling)
	for poll := 1; in.Now().Before(deadline); poll++ {
		if err := in.Sleep(ctx, admissionDrainPoll); err != nil {
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
				"the drain wait ran out of budget", err)
		}
		current, err := proxyendpoint.Read(root)
		if err != nil || !proxyendpoint.NewPoolKey(current, in.Database).SameGeneration(key) {
			// The record is gone or the generation moved: the drain finished.
			// Re-run from the top rather than probing a generation nobody has
			// validated.
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
				"the draining generation was replaced; re-admitting", err)
		}
		if poll%admissionDrainProbesEvery != 0 {
			continue
		}
		// Re-probe the PORT, not just the record (council A-F7). ECONNREFUSED
		// on a live same-generation record is the signature of a proxy on its
		// way down AND of one on its way up: the supervisor writes the record
		// at start, binds its listener afterwards, and its Dolt child can
		// cold-start for tens of seconds. In the starting case the generation
		// never moves, so a loop that watched only the record burned the whole
		// 60s ceiling and then refused — a controller boot that caught that
		// window blocked for a minute and fell to BdStore for the process.
		switch probe := in.Probe(ctx, ep, in.Database); probe.Outcome {
		case proxyendpoint.ProbeRefused:
			// Still down, or still coming up. Keep waiting.
		default:
			// Anything else is a changed answer, and the endpoint is no longer
			// this pass's evidence: re-run from the top, where a served probe
			// meets the cursor gate and a silent one meets the no-greeting
			// ladder.
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
				"the data port answered "+probe.Outcome.String()+" during the drain wait; re-admitting", probe.Err)
		}
	}
	return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
		"the proxy is still refusing after the drain ceiling", nil)
}

// escalateZombie handles an endpoint that accepts a connection and then never
// greets.
//
// The ladder is deliberately slow to spend anything. A proxy mid-restart
// accepts and stays silent for a beat, so the first rung is simply asking
// again, three times across at least two seconds. Only then does gc fork bd,
// and only once per generation does it ask for a recover. A second silent pass
// after a recover has already been spent on this generation is terminal:
// bd has been asked to fix it and has not, and gc's remaining options are all
// somebody else's to exercise.
func (in AdmissionInput) escalateZombie(ctx context.Context, root string, ep proxyendpoint.Endpoint, key proxyendpoint.PoolKey) (Pin, bool, error) {
	for attempt := 1; attempt < admissionNoGreetingAttempts; attempt++ {
		if err := in.Sleep(ctx, admissionNoGreetingSpacing); err != nil {
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the no-greeting ladder ran out of budget", err)
		}
		probe := in.Probe(ctx, ep, in.Database)
		if probe.Outcome != proxyendpoint.ProbeAcceptedNoGreeting {
			// Something changed. Re-run from the top: the record may have moved
			// under us, and this pass's endpoint is no longer the evidence.
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the endpoint's answer changed during the no-greeting ladder", probe.Err)
		}
	}

	generation := key.Generation()
	if in.Ops == nil {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the endpoint accepts and never greets, and admission has no provider ops to escalate with", ep.Err)
	}

	// One ping per generation. The observed set is what makes "a later open in
	// the same process skips its ping" true: a generation gc has already asked
	// bd about does not get asked again just because a second scope opened.
	if in.Observed.Add(generation) {
		err := in.Ops.Ping(ctx, in.ScopeRoot)
		switch {
		case err == nil:
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"pinged the provider; re-admitting", nil)
		case !in.sameGeneration(root, key):
			// The ping failed but the generation moved anyway, which is bd
			// replacing its proxy. Re-admit against whatever is there now.
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the generation moved during the ping; re-admitting", err)
		default:
			// The ping failed and nothing moved. That is not evidence that bd
			// cannot fix this proxy — the failure is as likely to be gc's own
			// lifecycle semaphore or op budget — so it does NOT cascade into a
			// `bd dolt stop` in the same pass (council A-F5). The rung is
			// released so a later open may ask again, and the answer is
			// non-terminal so the next open re-admits from the top.
			in.Observed.Release(generation)
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the endpoint accepts and never greets, and the provider ping failed; "+
					"not escalating to a recover on a failure that says nothing about the proxy", err)
		}
	}

	// One recover per generation, ever.
	if in.Recovered.Add(generation) {
		if err := in.Ops.Recover(ctx, in.ScopeRoot); err != nil {
			return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictProxyZombie,
				"the endpoint accepts and never greets, and the provider could not recover it", err)
		}
		return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"recovered the provider; re-admitting", nil)
	}

	return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictProxyZombie,
		"the endpoint still accepts and never greets after a recover was already spent on generation "+generation, ep.Err)
}

// escalateWithPing spends the single ping a stopped or stale record is worth.
//
// This is the "absent/dead" half of the four states the design says may cost a
// bd fork. It is ONE ping: bd either adopts or restarts its proxy, and the
// caller re-admits against whatever bd produced. It never spawns anything —
// asking bd to start bd's proxy is the only lifecycle move gc has.
func (in AdmissionInput) escalateWithPing(ctx context.Context, generation, detail string, cause error) (Pin, bool, error) {
	const verdict = ProxiedVerdictProxyGone
	if in.Ops == nil {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(verdict,
			detail+"; admission has no provider ops to escalate with", cause)
	}
	// An absent record has no generation to key on, so it is keyed on the scope
	// instead: the question "have we already asked bd about this scope's
	// missing proxy" has the same shape and the same answer.
	//
	// It is bounded by the INCIDENT, not by the process (council A-F6). The
	// design's rule is once per generation, and a missing generation was being
	// treated as a permanent one: the first open of a scope whose proxy is
	// stopped spent the ping, and every later open in that process returned
	// proxy_gone and spent nothing — for ever, whether the first ping had
	// succeeded or failed. Two things bound it now: Admit releases this key the
	// moment the scope admits again, because an incident that ended is not a
	// rung this one has spent, and the ledger's own TTL expires it anyway.
	key := generation
	if key == "" {
		key = absentRecordPingKey(in.ScopeRoot)
	}
	if !in.Observed.Add(key) {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(verdict,
			detail+"; the provider was already pinged for this generation", cause)
	}
	if err := in.Ops.Ping(ctx, in.ScopeRoot); err != nil {
		// The rung bought nothing, so it is not spent. A ping that failed on
		// gc's own lifecycle semaphore must not poison the scope for the next
		// open, which may well find the semaphore free.
		in.Observed.Release(key)
		return Pin{}, false, NewNonTerminalProxiedVerdictError(verdict,
			detail+"; the provider ping failed", err)
	}
	return Pin{}, true, NewNonTerminalProxiedVerdictError(verdict, detail+"; pinged the provider, re-admitting", cause)
}

// absentRecordPingKey is the ledger key for a scope with no proxy record at
// all. It is namespaced so it can never collide with a real generation, which
// is a {pid, birth} pair.
func absentRecordPingKey(scopeRoot string) string { return "scope:" + scopeRoot }

// sameGeneration re-reads the record and reports whether it still names the
// generation the caller was working with.
func (in AdmissionInput) sameGeneration(root string, key proxyendpoint.PoolKey) bool {
	current, err := proxyendpoint.Read(root)
	if err != nil {
		return false
	}
	return proxyendpoint.NewPoolKey(current, in.Database).SameGeneration(key)
}

func (in AdmissionInput) withDefaults() AdmissionInput {
	if in.ProcessTable.Alive == nil {
		in.ProcessTable = proxyendpoint.DefaultProcessTable()
	}
	if in.Probe == nil {
		in.Probe = proxyendpoint.ProbeEndpoint
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.Sleep == nil {
		in.Sleep = sleepWithContext
	}
	if in.Observed == nil {
		in.Observed = defaultObservedGenerations
	}
	if in.Recovered == nil {
		in.Recovered = defaultRecoveredGenerations
	}
	return in
}

// sleepWithContext waits, and reports the context's error instead of sleeping
// through an expired budget.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// The process-local pin memo.
//
// It exists because a single gc command opens a scope many times — `gc doctor`
// alone opens 17+ — and admission's cheapest healthy path still costs one
// probe SESSION against bd's proxy. Seventeen sessions per run is seventeen
// accepted TCP connections bd's idle watcher counts, for an answer that cannot
// have changed.
//
// It is keyed on the two files the answer is derived from, not on time alone: a
// proxy.pid or a sidecar that changed invalidates the entry immediately,
// whatever the TTL says. The TTL is the guard interval, so the memo can never
// hold an answer longer than the tick that would have re-checked it.
//
// It is ALSO keyed on the lane — LongLived — and that is council C-F2. Admit
// consults the memo before admitOnce, and the finite-idle refusal (the Q1
// deviation) lives inside admitOnce, after it. With a lane-blind key a one-shot
// open of an operator-initialized finite-idle scope admitted, memoized its pass,
// and a LONG-LIVED open of the same scope inside the TTL got that pass back —
// so gc held a resident native handle across a window in which bd retires the
// proxy and its Dolt child, which is the exact outcome Q1 says PR2 refuses.
// Production takes that path: cmd/gc/main.go's openStoreAtForCity is
// longLived=false and cmd/gc/api_state.go is LongLived=true, in one binary.
//
// Keying on the lane rather than moving the idle rule ahead of the memo is the
// smaller change and the more honest one: "may this scope be served natively"
// is a DIFFERENT question for a handle held for 40ms and one held for the
// process lifetime, and a memo keyed on less than the question is a memo that
// answers a question nobody asked.
//
// # What the stamp CANNOT see, and what bounds it (council A-F3)
//
// A migration writes neither proxy.pid nor the sidecar. The cursors are read
// from the database, so no file fingerprint can invalidate an entry when
// somebody runs `bd migrate` (or a newer bd opens the database) inside the TTL:
// the memo hands back the stale pin, with the stale cursors, and the schema
// gate does not run for that open.
//
// Re-reading the cursors on a memo HIT is not the fix. The cursor read IS the
// probe session, and one probe session is exactly what a memo MISS costs on the
// healthy path — so a memo that re-probed would cost what it saves and delete
// its own reason to exist (17 accepted TCP connections per `gc doctor` run, on
// a proxy whose idle watcher cannot arm while one is open).
//
// Three things bound the exposure instead, and they are stated here so a reader
// does not have to reconstruct them:
//
//  1. proxiedPinMemoTTL caps the entry at proxiedPinMemoMaxTTL however long the
//     operator's guard interval is. GC_BEADS_PROXIED_GUARD_INTERVAL has a floor
//     but no ceiling, so without the cap an operator asking for a quieter tick
//     was also asking the memo to trust a schema answer for that long.
//  2. The library runs CheckForwardDrift at EVERY open, so a database that
//     moved ahead of this binary is refused by the library rather than served —
//     as the library's untyped error rather than gc's typed verdict, which is
//     the part that is genuinely worse and is the residual here.
//  3. The guard tick re-reads the cursors every interval and, on drift, now
//     forgets the memo as well as standing the handle down — so the next open
//     in the process re-derives instead of reading an answer a tick has already
//     contradicted.
var proxiedPinMemo = struct {
	mu      sync.Mutex
	entries map[string]proxiedPinMemoEntry
}{entries: map[string]proxiedPinMemoEntry{}}

type proxiedPinMemoEntry struct {
	pin     Pin
	stamp   string
	expires time.Time
}

func proxiedPinMemoKey(scopeRoot, database string, longLived bool) string {
	lane := "one-shot"
	if longLived {
		lane = "long-lived"
	}
	return scopeRoot + "\x00" + database + "\x00" + lane
}

// proxiedPinStamp fingerprints the two files admission's answer depends on.
// An unreadable file yields a stamp nothing matches, so the memo misses and
// admission re-derives rather than trusting a stale pass.
func proxiedPinStamp(root, beadsDir string, now time.Time) string {
	stamp := func(path string) string {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Sprintf("%s:absent:%d", path, now.UnixNano())
		}
		return fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
	}
	return stamp(proxyendpoint.PIDPath(root)) + "|" + stamp(proxyendpoint.SidecarPath(beadsDir))
}

func lookupProxiedPin(scopeRoot, database string, longLived bool, root, beadsDir string, now time.Time) (Pin, bool) {
	proxiedPinMemo.mu.Lock()
	defer proxiedPinMemo.mu.Unlock()
	entry, ok := proxiedPinMemo.entries[proxiedPinMemoKey(scopeRoot, database, longLived)]
	if !ok || now.After(entry.expires) {
		return Pin{}, false
	}
	if entry.stamp != proxiedPinStamp(root, beadsDir, now) {
		return Pin{}, false
	}
	return entry.pin, true
}

// proxiedPinMemoMaxTTL is the ceiling on how long a memoized admission pass may
// be trusted, whatever the guard interval says.
//
// The TTL is the guard interval because the memo must never hold an answer
// longer than the tick that would have re-checked it. That reasoning is sound
// for everything the stamp CAN see and silent about the one thing it cannot: a
// migration. GC_BEADS_PROXIED_GUARD_INTERVAL has a floor and no ceiling, so
// `GC_BEADS_PROXIED_GUARD_INTERVAL=1h` — a reasonable thing for an operator to
// ask of a ticker — also asked the memo to trust a schema answer for an hour.
const proxiedPinMemoMaxTTL = 15 * time.Second

// proxiedPinMemoTTL is how long an entry is trusted: the guard interval, capped.
func proxiedPinMemoTTL() time.Duration {
	if interval := proxiedGuardInterval(); interval < proxiedPinMemoMaxTTL {
		return interval
	}
	return proxiedPinMemoMaxTTL
}

func storeProxiedPin(scopeRoot, database string, longLived bool, root, beadsDir string, pin Pin, now time.Time) {
	proxiedPinMemo.mu.Lock()
	defer proxiedPinMemo.mu.Unlock()
	proxiedPinMemo.entries[proxiedPinMemoKey(scopeRoot, database, longLived)] = proxiedPinMemoEntry{
		pin:     pin,
		stamp:   proxiedPinStamp(root, beadsDir, now),
		expires: now.Add(proxiedPinMemoTTL()),
	}
}

// ForgetProxiedPin drops a scope's memoized admission passes — BOTH lanes.
//
// The guard tick calls it on a generation change: the memo's whole contract is
// that the answer cannot have changed, and a tick that just proved otherwise
// must not leave the contradiction in place for the next open to read. The
// generation moving invalidates the one-shot answer and the long-lived answer
// alike, so forgetting only the caller's own lane would leave the other half of
// the contradiction behind.
func ForgetProxiedPin(scopeRoot, database string) {
	proxiedPinMemo.mu.Lock()
	defer proxiedPinMemo.mu.Unlock()
	for _, longLived := range []bool{false, true} {
		delete(proxiedPinMemo.entries, proxiedPinMemoKey(scopeRoot, database, longLived))
	}
}
