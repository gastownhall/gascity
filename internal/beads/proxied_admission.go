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
	"github.com/gastownhall/gascity/internal/pathutil"
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
	// could. A failure bd ITSELF reported — the verb ran to completion and
	// exited non-zero — must be marked with ProviderReportedFailure; every
	// other failure (gc's lifecycle semaphore, the op budget, an environment
	// gc could not build) must not be. The no-greeting ladder escalates on the
	// first and never on the second.
	Ping(ctx context.Context, scopeRoot string) error
	// Recover asks bd to retire and re-establish a proxy that is listening but
	// not answering. Its failures carry the same mark on the same terms as
	// Ping's, and the mark decides more here: only a marked failure may end
	// the lane for the handle (proxy_zombie is terminal); every other failure
	// holds the recover rung for failedRecoverBackoff and is non-terminal.
	//
	// generation is the proxy generation admission saw as a zombie. Recover
	// may wait for whatever serializes provider verbs (gc's per-city
	// lifecycle slot), and another recover of the same zombie — the health
	// loop's — may have replaced it by then with a healthy proxy. So once it
	// holds that serialization, Recover must re-read the scope's proxy record
	// and run nothing unless it still names generation; otherwise it returns
	// an error wrapping ErrRecoverTargetMoved (round4 review F3). A record it
	// cannot read counts as moved: `bd dolt stop` is never aimed at a proxy
	// gc cannot name. And it must run nothing, returning an error wrapping
	// ErrRecoverAlreadyIssued, if this process has already issued the
	// recover on generation (round5 recheck M1; see GenerationSet.IssueStop).
	Recover(ctx context.Context, scopeRoot, generation string) error
}

// ErrRecoverTargetMoved is what errors.Is finds on a ProviderOps.Recover that
// ran nothing because, by the time it could run, the scope's proxy was no
// longer the generation admission asked it to recover. See ProviderOps.Recover.
var ErrRecoverTargetMoved = errors.New("the proxy generation the recover was aimed at is no longer current")

// ErrRecoverAlreadyIssued is what errors.Is finds on a ProviderOps.Recover
// that ran nothing because this process has already issued the provider's
// recover — its `bd dolt stop` — on that generation (round5 recheck M1). The
// invariant it enforces is absolute: per proxy generation, at most one `bd
// dolt stop` is ever issued by this process. A recover that was cut short
// after its script started may have stopped the proxy or may not have, and a
// second one is never the way to find out. See GenerationSet.IssueStop.
var ErrRecoverAlreadyIssued = errors.New("this process has already issued the provider recover on this proxy generation")

// ErrProviderReportedFailure is what errors.Is finds on a ProviderOps failure
// the PROVIDER reported: bd ran the verb to completion and said it could not
// do what it was asked. See ProviderReportedFailure.
var ErrProviderReportedFailure = errors.New("the provider ran the verb and reported failure")

// ProviderReportedFailure marks err as a failure the provider itself reported,
// leaving its text unchanged. A nil err stays nil.
//
// The distinction is the one piece of evidence the no-greeting ladder turns on
// (design v2 3.4, F13a/F22). SIGTERM a proxied scope's Dolt child and the
// child exits 0, so bd's supervisor never notices and the proxy lives on as a
// zombie: its data port accepts and never greets. `bd ping` adopts that proxy
// (the control port still answers), its SELECT 1 through the proxy fails, and
// it exits 1. That exit is bd saying its ping cannot make this proxy healthy,
// and the recover rung — `bd dolt stop` plus a ping — is what the design spends
// on it. A ping that failed on gc's side never got that far and says nothing
// about the proxy (council A-F5), so it stays unmarked and holds the rung for
// failedPingBackoff instead.
func ProviderReportedFailure(err error) error {
	if err == nil {
		return nil
	}
	return providerReportedFailure{err: err}
}

// providerReportedFailure is the mark ProviderReportedFailure puts on an error.
type providerReportedFailure struct{ err error }

func (e providerReportedFailure) Error() string { return e.err.Error() }

func (e providerReportedFailure) Unwrap() error { return e.err }

// Is matches ErrProviderReportedFailure, so the mark survives any %w wrapping
// between the provider op and the ladder.
func (e providerReportedFailure) Is(target error) bool { return target == ErrProviderReportedFailure }

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
	seen map[string]generationEntry
	// stops records the generations this process has issued a provider
	// recover (`bd dolt stop`) on. It never expires and nothing releases it:
	// see IssueStop.
	stops map[string]struct{}
	ttl   time.Duration
	now   func() time.Time
}

// generationEntry is one spent rung: when it stops counting, whether the verb
// that spent it FAILED (see Backoff), and whether that verb is still RUNNING
// (see begin).
type generationEntry struct {
	expires time.Time
	failed  bool
	// inFlight marks a rung whose verb has started and not yet returned. It
	// does not expire: the verb's own budget bounds it, and settle — which the
	// spender defers — ends it whichever way the verb came back.
	inFlight bool
	// settledAt is when settle ended an in-flight claim: when the verb's
	// answer landed. Zero for an entry Add wrote (no verb ran under it) and
	// for a failure arm's Backoff. See answeredSince.
	settledAt time.Time
}

// live reports whether the entry still holds its rung at now.
func (e generationEntry) live(now time.Time) bool {
	return e.inFlight || now.Before(e.expires)
}

// rungState is one generation's standing in a ledger, read under one lock so
// a caller never combines two answers from two different moments.
type rungState struct {
	// inFlight: a verb was started on the rung and has not returned.
	inFlight bool
	// backoff is how much longer a FAILED verb holds the rung; zero when the
	// last verb did not fail or its backoff has run out.
	backoff time.Duration
	// spent: a verb was spent on the rung, returned without failing, and the
	// entry has not expired.
	spent bool
}

// generationMemoTTL is how long a spent rung stays spent. See GenerationSet.
const generationMemoTTL = 5 * time.Minute

// failedPingBackoff is how long a provider ping that FAILED keeps its rung
// before a later open may fork bd for it again. See Backoff.
const failedPingBackoff = 30 * time.Second

// failedRecoverBackoff is the same bound for a provider recover that failed
// on gc's side or with an outcome gc cannot read (round4 recheck M1). It is
// its own name because it is its own rung: the recover ledger is a different
// GenerationSet, and a later reader tuning one must not silently tune both.
const failedRecoverBackoff = failedPingBackoff

// NewGenerationSet returns an empty set with the default TTL.
func NewGenerationSet() *GenerationSet {
	return &GenerationSet{seen: map[string]generationEntry{}, ttl: generationMemoTTL, now: time.Now}
}

// Add records a generation and reports whether it was NEW — absent, or recorded
// longer ago than its TTL — which is how a caller spends an escalation rung
// exactly once per incident rather than once per process. A rung under a failed
// verb's backoff is not new either.
func (s *GenerationSet) Add(generation string) bool {
	if s == nil || generation == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]generationEntry{}
	}
	now := s.clock()
	if entry, ok := s.seen[generation]; ok && entry.live(now) {
		return false
	}
	s.seen[generation] = generationEntry{expires: now.Add(s.memoTTL())}
	return true
}

// begin is Add for a rung whose verb is about to RUN: it claims the rung
// exactly as Add does, and marks it in flight until settle (round4 review F1).
//
// Add records a success-shaped entry before the verb has run, which is fine
// for a ledger nothing reads as a verdict. The recover ledger is read as one:
// "the recover was spent on this generation and it is still silent" is what
// makes proxy_zombie terminal. Recorded at Add, that sentence was true of a
// recover still queued on the lifecycle slot or still running, so a second
// ladder over the same generation — a rig on the city's proxy root, a second
// open of the city — reached it while the recover that would fix the proxy
// was in flight, and stood its long-lived handle down for the process.
func (s *GenerationSet) begin(generation string) bool {
	if s == nil || generation == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]generationEntry{}
	}
	if entry, ok := s.seen[generation]; ok && entry.live(s.clock()) {
		return false
	}
	s.seen[generation] = generationEntry{inFlight: true}
	return true
}

// settle ends a rung begin put in flight: it is spent from now, for the TTL.
//
// A spender defers it, so the rung stops being in flight however the verb
// came back. It changes nothing a later write already decided: a Backoff (the
// verb failed on gc's side) or a Release replaced the in-flight entry, and
// that answer stands.
func (s *GenerationSet) settle(generation string) {
	if s == nil || generation == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.seen[generation]; ok && entry.inFlight {
		now := s.clock()
		s.seen[generation] = generationEntry{expires: now.Add(s.memoTTL()), settledAt: now}
	}
}

// answeredSince reports whether a verb begin claimed on generation was
// answered (settled) strictly after t (round5 recheck M1).
//
// The no-greeting ladder spends the recover on its own evidence — three
// probes that found the endpoint silent. That evidence is stale if another
// ladder's ping of the same generation came back after this ladder's last
// probe began: a slow-to-greet proxy that bd's ping has just found healthy
// looks exactly like a zombie to a probe that ran before the ping returned.
func (s *GenerationSet) answeredSince(generation string, t time.Time) bool {
	if s == nil || generation == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.seen[generation]
	return ok && !entry.inFlight && !entry.settledAt.IsZero() && entry.settledAt.After(t)
}

// IssueStop records that this process is about to issue the provider recover
// — `bd dolt stop` then `bd ping` — on generation, and reports false if it
// already has (round5 recheck M1).
//
// It is the last gate before the script runs, and it is absolute: per proxy
// generation, at most one `bd dolt stop` is ever issued by this process. The
// recover rung above it is not enough on its own. That rung is RETRYABLE by
// design after a recover gc's own budget cut short (round4 recheck M1), and a
// recover cut short after its script started may already have stopped the
// proxy — or may have been killed before the stop landed. Either way a second
// stop of the same generation is not the way to find out: a stop that took
// moves the generation, and bd's own ping brings the scope back; a stop that
// did not is left to the ping rung and to the city's health loop.
//
// Nothing releases or expires an entry. A generation is a {pid, birth} pair
// that is never reused, so the set grows by one per recover this process ever
// issues.
func (s *GenerationSet) IssueStop(generation string) bool {
	if s == nil || generation == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stops == nil {
		s.stops = map[string]struct{}{}
	}
	if _, issued := s.stops[generation]; issued {
		return false
	}
	s.stops[generation] = struct{}{}
	return true
}

// rung reports a generation's standing in one read. See rungState.
func (s *GenerationSet) rung(generation string) rungState {
	if s == nil || generation == "" {
		return rungState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.seen[generation]
	if !ok {
		return rungState{}
	}
	if entry.inFlight {
		return rungState{inFlight: true}
	}
	remaining := entry.expires.Sub(s.clock())
	if remaining <= 0 {
		return rungState{}
	}
	if entry.failed {
		return rungState{backoff: remaining}
	}
	return rungState{spent: true}
}

// Release drops a generation, so the rung it stood for may be spent again at
// once.
//
// It is for an incident that is OVER — Admit releases the absent-record key the
// moment the scope admits (council A-F6). It is NOT for a verb that failed: see
// Backoff: a failed ping used to be released, and that removed the bound
// entirely.
func (s *GenerationSet) Release(generation string) {
	if s == nil || generation == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, generation)
}

// Backoff records that the verb spent on a generation FAILED, and keeps its rung
// for d rather than for the full TTL (council pr2 D-F9).
//
// A provider verb can fail for a reason of gc's own — the lifecycle semaphore,
// the op budget — which says nothing about the proxy, so the rung must not stay
// spent for the whole TTL on the strength of gc's own contention (council
// A-F5). But releasing it outright, which is what this replaced, removed the
// bound entirely: a refusal is never memoized, so every later open re-walked
// the ladder and re-forked `bd ping` — `gc doctor`'s seventeen opens of one
// scope cost seventeen forks where main cost one — and the failure the ping is
// most likely to have hit, lifecycle-semaphore contention on a loaded box, is
// made worse by every extra bd child. A short backoff keeps the ask
// retryable and makes it bounded.
//
// While it holds, the rung is neither spendable (Add reports false) nor spent
// successfully: BackingOff tells the ladder to decline without escalating,
// because a ping that failed on gc's side is no evidence that bd has been
// asked and could not help. A ping bd itself reported as failed never gets
// here from the no-greeting ladder: that is the evidence, and the ladder
// spends the recover on it (see escalateZombie).
//
// The recover ledger uses it the same way (round4 recheck M1): a recover gc's
// own budget or cancellation cut short, or one whose outcome gc cannot read,
// holds the recover rung for failedRecoverBackoff instead of spending it, so
// it is neither re-forked on every open nor read as "bd was asked to recover
// and could not" — which is the only reading that may end the lane.
func (s *GenerationSet) Backoff(generation string, d time.Duration) {
	if s == nil || generation == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]generationEntry{}
	}
	s.seen[generation] = generationEntry{expires: s.clock().Add(d), failed: true}
}

// BackingOff reports whether a generation's last verb failed and its backoff
// has not yet run out, and how long remains.
func (s *GenerationSet) BackingOff(generation string) (time.Duration, bool) {
	if s == nil || generation == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.seen[generation]
	if !ok || !entry.failed {
		return 0, false
	}
	remaining := entry.expires.Sub(s.clock())
	return remaining, remaining > 0
}

// Has reports whether a generation is in the set and has not expired.
func (s *GenerationSet) Has(generation string) bool {
	if s == nil || generation == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.seen[generation]
	return ok && entry.live(s.clock())
}

func (s *GenerationSet) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

func (s *GenerationSet) memoTTL() time.Duration {
	if s.ttl <= 0 {
		return generationMemoTTL
	}
	return s.ttl
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
	// the open moved HEAD. See ProxiedOpenUnmoved.
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
	// CityRoot is the city's workspace root. A scope that is NOT the city but
	// whose proxy root IS the city's — the `gc beads city migrate-proxied`
	// shape, where one proxy and one Dolt child serve hq and every rig —
	// cannot run the recover the ladder's last rung exists for: the provider
	// script degrades such a scope's recover to a ping, because cycling the
	// shared pair is the city scope's. Such a scope never spends the recover
	// rung (round4 recheck M2; see leaveRecoverToCity). Empty treats every
	// scope as owning its root.
	CityRoot string
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

	// ProbeOnce caps this call at ONE probe session and no waiting: a single
	// pass, no drain wait on a refused port, no no-greeting ladder on a silent
	// one, and no re-run after a changed answer. Every one of those comes back
	// as the same non-terminal verdict the ladder would have started from.
	//
	// It is the guard tick's recovery shape (council pr2 D-F5). The ladder's
	// waits exist for a caller who needs an answer NOW; a background tick
	// does not, because the tick itself is the retry — it runs again one
	// interval later. Without the cap a recovery against a proxy that accepts
	// and stays silent cost up to nine probe sessions and ~6s of sleeps per
	// tick (three outer passes x the three-attempt ladder), and one against a
	// refusing port ran the whole 60s drain ceiling, forever, on a timer —
	// each session an accepted connection bd's idle watcher counts, on a proxy
	// bd may be trying to retire.
	ProbeOnce bool
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
// library's own open-time checks see only the MAIN lane (see "What does NOT
// bound it" on proxiedPinMemo), so an ignored-lane mismatch discovered after
// the open is one the open has already migrated, and a main-lane one is the
// library's untyped error. Discovered here it is gc's typed verdict, and nothing
// was opened.
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
	maxRungs := 4
	if in.ProbeOnce {
		// One pass: a retry is a second probe session, and the caller's next
		// tick is the retry.
		maxRungs = 1
	}
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
		//
		// And it reads it only when a session EVALUATED it (council pr2
		// D-F11). An unevaluated reality's zero value says "nothing was
		// missing", which is the raw-cursor comparison A-F2 removed, so it is
		// refused — non-terminally, because it is a fact about the session
		// that produced the result rather than about the database.
		if !probe.Reality.Checked() {
			return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictSchemaUnverified,
				"the probe reported the database served but its session never evaluated the ignored lane's "+
					"cursor reality, so the gate cannot tell which cursor the linked library will act on; "+
					"in production only the probe session marks a reality evaluated", nil)
		}
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
	if in.ProbeOnce {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictDraining,
			"data port refused; a background recovery does not wait out a drain, its next tick asks again", nil)
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
		probe := in.Probe(ctx, ep, in.Database)
		switch {
		case probe.Outcome == proxyendpoint.ProbeRefused:
			// Still down, or still coming up. Keep waiting.
		case probe.Outcome == proxyendpoint.ProbeUnknown && proxyendpoint.IsIndeterminate(probe.Err):
			// The probe's own clock (or the caller's) ended the session, which
			// says nothing about the endpoint — the discipline every other
			// probe consumer in this file applies. It is NOT a changed answer
			// (council pr2 D-F10): ending the drain on it meant a draining
			// proxy on a loaded box — exactly where two-second sessions run
			// out — stopped being waited out at the first re-probe, re-ran the
			// pass, met the same indeterminate probe and returned
			// budget_exhausted. Keep waiting; the record check above and the
			// ceiling still bound the loop, and the caller's cancellation
			// still ends it through Sleep.
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
//
// The recover is reached two ways, and both are "bd has been asked and could
// not": the ping bd ran reported failure (ProviderReportedFailure — the real
// zombie's shape, whose `bd ping` exits 1, so it falls through in the SAME
// pass), or the ping succeeded and the generation it left is still silent (a
// later pass finds the rung spent). A ping that failed on gc's side reaches
// neither: it holds the rung for failedPingBackoff (council A-F5, D-F9).
//
// Only a recover bd itself refused is terminal (round4 recheck M1); see
// recoverFailed for every other way a recover can fail. And only a scope that
// can cycle its proxy root spends the recover at all: a rig on the city's
// root leaves it to the city scope (round4 recheck M2, leaveRecoverToCity).
// "Spent" means the recover has RETURNED: a ladder that reaches the rung
// while another open's recover of the same generation is still queued or
// running answers non-terminal and runs nothing (round4 review F1).
func (in AdmissionInput) escalateZombie(ctx context.Context, root string, ep proxyendpoint.Endpoint, key proxyendpoint.PoolKey) (Pin, bool, error) {
	if in.ProbeOnce {
		// The first rung is "ask again", and a background tick asks again by
		// running again. It could not spend the escalation rungs anyway.
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the endpoint accepts and never greets; a background recovery does not walk the no-greeting ladder, its next tick asks again", ep.Err)
	}
	// lastProbe is when this ladder's newest evidence of silence was taken,
	// on the ledger's clock (see GenerationSet.answeredSince).
	var lastProbe time.Time
	for attempt := 1; attempt < admissionNoGreetingAttempts; attempt++ {
		if err := in.Sleep(ctx, admissionNoGreetingSpacing); err != nil {
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the no-greeting ladder ran out of budget", err)
		}
		lastProbe = in.Observed.clock()
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

	// A ping that failed recently is not re-forked on every open, and is not
	// escalated on either (council pr2 D-F9): see GenerationSet.Backoff.
	if remaining, backing := in.Observed.BackingOff(generation); backing {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			fmt.Sprintf("the endpoint accepts and never greets, and the provider ping for this generation failed; "+
				"not re-forking bd for another %s, and not escalating to a recover on a failure that says nothing about the proxy",
				remaining.Round(time.Second)), ep.Err)
	}

	// One ping per generation. The observed set is what makes "a later open in
	// the same process skips its ping" true: a generation gc has already asked
	// bd about does not get asked again just because a second scope opened.
	//
	// The claim is in flight until bd answers (round5 recheck M1), and the
	// answer is written in ONE step once the arm below has decided it: a
	// success or bd's own refusal settles the rung, a failure of gc's own
	// replaces the claim with a backoff. Before this the claim was Add's
	// success-shaped entry from the moment the ping was forked, so a second
	// ladder on the same generation — a second long-lived open of the city,
	// a city ladder while a shared-root rig's ping held the lifecycle slot —
	// read the ping as answered and spent the recover, `bd dolt stop`, while
	// the only ping was still running. If that ping then failed on gc's side
	// (which must never lead to a stop: council A-F5, D-F9) or came back
	// healthy (a slow-to-greet proxy, generation unchanged), the stop had
	// already been issued on it.
	if in.Observed.begin(generation) {
		// A panic in the verb must not strand the claim in flight for ever;
		// on every ordinary path the rung has already been written by then
		// and this is a no-op.
		defer in.Observed.settle(generation)
		err := in.Ops.Ping(ctx, in.ScopeRoot)
		switch {
		case err == nil:
			in.Observed.settle(generation)
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"pinged the provider; re-admitting", nil)
		case !in.sameGeneration(root, key):
			// The ping failed but the generation moved anyway, which is bd
			// replacing its proxy — or the record could not be read at all,
			// which sameGeneration also reports as "moved". Re-admit against
			// whatever is there now, and hold THIS generation's rung for the
			// backoff first (council pr2 E-S6). Settled as a success, a record
			// read that merely failed (EMFILE, EIO, a torn read) while the
			// generation stayed current let the next open find the ping
			// "spent", no backoff, and escalate straight to a recover — a
			// `bd dolt stop` with no successful ping on this generation, the
			// cascade D-F9 says a failed ping never causes. If the generation
			// really moved, the backoff sits on a key nobody asks about again.
			in.Observed.Backoff(generation, failedPingBackoff)
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the generation moved during the ping; re-admitting", err)
		case errors.Is(err, ErrProviderReportedFailure):
			// bd RAN its ping and said it could not make this proxy healthy,
			// and nothing moved. That is the zombie's own signature, not gc's
			// contention: a Dolt child that exited 0 leaves bd's proxy up and
			// silent, `bd ping` adopts it, its SELECT 1 fails and it exits 1
			// (design v2 3.4, F13a/F22). It is exactly the evidence the
			// recover rung exists for, so it falls through to it in this pass,
			// once per generation. The ping rung stays spent: bd has answered
			// it.
			//
			// This arm is round3 review completeness: before it, EVERY failed
			// ping took the backoff arm below, so a real zombie — whose ping
			// can only fail — was pinged once per backoff window for ever and
			// never recovered.
			in.Observed.settle(generation)
		default:
			// The ping failed on gc's side — the lifecycle semaphore, the op
			// budget, an environment gc could not build — and nothing moved.
			// bd never ran to an answer, so this is not evidence that bd
			// cannot fix this proxy, and it does NOT cascade into a `bd dolt
			// stop` in the same pass (council A-F5), nor in a later one while
			// the backoff holds. The rung is held for failedPingBackoff rather
			// than released, so a later open asks again once it runs out and
			// not on every open before then (council pr2 D-F9). The answer is
			// non-terminal.
			in.Observed.Backoff(generation, failedPingBackoff)
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the endpoint accepts and never greets, and the provider ping failed before bd could answer; "+
					"not escalating to a recover on a failure that says nothing about the proxy", err)
		}
	} else {
		// The ping rung was already claimed. Nothing below it may run until
		// bd has answered that ping, and only on evidence taken after it did.
		ping := in.Observed.rung(generation)
		switch {
		case ping.inFlight:
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the endpoint accepts and never greets, and another open in this process is running the provider "+
					"ping for this generation now; not escalating to a recover before bd has answered it", ep.Err)
		case ping.backoff > 0:
			// The ping failed on gc's side between the two reads.
			return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the endpoint accepts and never greets, and the provider ping for this generation failed before bd "+
					"could answer; not escalating to a recover on a failure that says nothing about the proxy", ep.Err)
		case !ping.spent:
			// Released or expired between the two reads: nothing was asked.
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the ping rung for this generation was released while this pass read it; re-admitting", ep.Err)
		case in.Observed.answeredSince(generation, lastProbe):
			// bd answered another open's ping after this ladder's last probe
			// began, so this ladder's silence predates bd's answer. A proxy
			// that was merely slow to greet looks exactly like a zombie to
			// those probes; re-read and re-probe rather than stop it.
			return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"bd answered this generation's ping after this ladder's probes; re-admitting on fresh evidence", ep.Err)
		}
	}

	// The recover rung is spent by the scope that can run it (round4 recheck
	// M2). The ledger is process-global and keyed on the generation, which a
	// rig sharing the city's proxy root shares with the city.
	if in.sharesCityProxyRoot(root) {
		return Pin{}, false, in.leaveRecoverToCity(generation, ep)
	}

	// One recover per generation, ever. The claim is in flight until the verb
	// returns (round4 review F1), so no other ladder reads it as spent early.
	if in.Recovered.begin(generation) {
		return in.spendRecover(ctx, root, key)
	}

	// The rung was already claimed. Which way decides the answer, and all of
	// it comes from one read of the ledger.
	rung := in.Recovered.rung(generation)
	switch {
	case rung.inFlight:
		// Another open in this process is running this generation's recover
		// now — a second long-lived handle on the city, or the pass of an
		// admitWith loop that reached the rung while the recover it is
		// waiting for is queued on the lifecycle slot. bd has not answered
		// it yet, so "a recover was already spent" is not true of it, and a
		// second recover of the same proxy is exactly what the ledger exists
		// to prevent. Once it returns the generation has moved (and this
		// scope re-admits) or its answer is on the rung.
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the endpoint accepts and never greets, and another open in this process is running the provider "+
				"recover for this generation now; not running a second one, and not ending the lane before bd has answered it", ep.Err)
	case rung.backoff > 0:
		// A recover that failed on gc's side holds its rung for a backoff
		// (round4 recheck M1). While it holds, this generation has had no
		// recover bd answered, so the terminal line below — "a recover was
		// already spent" — is not true of it yet.
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			fmt.Sprintf("the endpoint accepts and never greets, and the provider recover for this generation failed "+
				"before bd could answer; not re-forking it for another %s", rung.backoff.Round(time.Second)), ep.Err)
	case rung.spent:
		return Pin{}, false, NewProxiedVerdictError(ProxiedVerdictProxyZombie,
			"the endpoint still accepts and never greets after a recover was already spent on generation "+generation, ep.Err)
	default:
		// The claim that refused this one expired or was released between
		// the two reads. Nothing was spent by this pass; ask again.
		return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the recover rung for this generation was released while this pass read it; re-admitting", ep.Err)
	}
}

// spendRecover runs the recover this ladder has just claimed, and ends the
// claim however the verb comes back: settle, deferred, turns an in-flight rung
// into a spent one unless recoverFailed has already put a backoff on it.
func (in AdmissionInput) spendRecover(ctx context.Context, root string, key proxyendpoint.PoolKey) (Pin, bool, error) {
	generation := key.Generation()
	defer in.Recovered.settle(generation)
	err := in.Ops.Recover(ctx, in.ScopeRoot, generation)
	if errors.Is(err, ErrRecoverTargetMoved) {
		// Nothing ran: the zombie was replaced while the recover waited for
		// the lifecycle slot, usually by the health loop's own recover of it
		// (round4 review F3). Before this check the queued recover ran `bd
		// dolt stop` on whatever held the port by then — a healthy proxy
		// nobody had seen as a zombie — and cold-started hq and every rig a
		// second time. No recover was spent on this generation, so its rung
		// is released rather than settled, and this scope re-admits against
		// what is there now.
		in.Recovered.Release(generation)
		return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the generation moved while the provider recover waited to run; it ran nothing, re-admitting", err)
	}
	if errors.Is(err, ErrRecoverAlreadyIssued) {
		// Nothing ran: this process already issued the recover on this
		// generation, and it was cut short on gc's side (round5 recheck M1).
		// A second `bd dolt stop` of one generation is never issued, so the
		// rung is held for the backoff — no fork on every open — and the
		// answer is non-terminal: nothing here is bd's answer either.
		in.Recovered.Backoff(generation, failedRecoverBackoff)
		return Pin{}, false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the endpoint accepts and never greets, and this process has already issued the provider recover on "+
				"this generation; not issuing a second `bd dolt stop`", err)
	}
	if err != nil {
		retry, verdict := in.recoverFailed(ctx, root, key, err)
		return Pin{}, retry, verdict
	}
	return Pin{}, true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
		"recovered the provider; re-admitting", nil)
}

// sharesCityProxyRoot reports whether this scope is not the city but resolves
// the city's proxy root.
//
// It is the provider script's own test (gc-beads-bd.sh,
// provider_owned_scope_shares_city_proxy_root: a scope other than
// GC_CITY_PATH whose physical proxy root is the city's), answered with the
// resolver the generation under escalation was read from — so "shares" here
// means exactly "shares the generation the city scope would recover". A city
// root gc cannot resolve answers false, which is the script's answer too.
func (in AdmissionInput) sharesCityProxyRoot(root string) bool {
	if in.CityRoot == "" || pathutil.SamePath(in.ScopeRoot, in.CityRoot) {
		return false
	}
	cityRoot, err := proxyendpoint.ProviderRoot(in.CityRoot)
	if err != nil || cityRoot == "" {
		return false
	}
	return pathutil.SamePath(root, cityRoot)
}

// leaveRecoverToCity is the no-greeting ladder's last rung for a scope that
// shares the city's proxy root, and so cannot run the recover (round4 recheck
// M2).
//
// The recover ledger is process-global and keyed on the generation, and a rig
// on the city's root has the city's generation. The rig's own recover op is
// `bd ping` alone — the script will not `bd dolt stop` the pair serving hq and
// every rig from a rig — so a rig whose ladder reached the rung first spent the
// generation's one recover on a ping bd had just refused, took a terminal
// proxy_zombie for it, and the city's ladder then found the rung spent and went
// terminal with no verb at all: `bd dolt stop` never ran, the zombie was never
// recovered, and both long-lived handles were demoted for the process. Whether
// that happened depended on which reader hit the dead pool first.
//
// So this scope neither spends the rung nor forks its degraded recover (a
// second `bd ping` straight after the one bd just refused learns nothing), and
// answers non-terminal: the recover that cycles the shared proxy is the city
// scope's, spent by the city's own ladder, and once it has run the generation
// moves and this scope re-admits. Its answer turns terminal on exactly the
// city's terms and no sooner — the city's recover has been SPENT on this
// generation and has RETURNED (not merely held for a backoff after a gc-side
// failure, and not still queued or running: round4 review F1) and the
// generation is still silent.
//
// The in-flight case is the ordinary one, not a corner: a rig on a later pass
// of its admitWith loop reaches this rung while the city's recover is queued
// on the lifecycle slot or running. What can no longer happen is the order
// this comment used to describe as intended — the city claiming the recover
// while the rig's `bd ping` still held the slot. That spent the stop before
// bd had answered the generation's only ping; the city's ladder now waits for
// that answer (round5 recheck M1, escalateZombie's ping rung).
func (in AdmissionInput) leaveRecoverToCity(generation string, ep proxyendpoint.Endpoint) error {
	rung := in.Recovered.rung(generation)
	if rung.spent {
		return NewProxiedVerdictError(ProxiedVerdictProxyZombie,
			"the endpoint still accepts and never greets after the city scope's recover was already spent on generation "+
				generation+", whose proxy root this scope shares", ep.Err)
	}
	if rung.inFlight {
		return NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
			"the endpoint accepts and never greets, and the city scope's recover of this generation, whose proxy root "+
				"this scope shares, is running now; not ending the lane before bd has answered it", ep.Err)
	}
	return NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
		"the endpoint accepts and never greets, and this scope shares the city's proxy root: its own recover would only "+
			"ping, which bd has already answered for this generation, and the recover that cycles the shared proxy is "+
			"the city scope's to spend", ep.Err)
}

// recoverFailed classifies a provider recover that did not succeed (round4
// recheck M1).
//
// proxy_zombie is terminal: the handle that takes it stands down for the rest
// of the process and every read goes through bd. The design earns that only
// when bd has been asked to recover and could not (v2 3.4: "if the recovered
// generation is again a zombie"), so ONLY a determinate refusal from the
// sanctioned verb is terminal: the script ran to completion inside gc's
// budget and bd answered with a status of its own (ProviderReportedFailure,
// and gc's context still live, and nothing about the error a timeout) on a
// generation that is still current. A refusal after which the record names a
// new generation re-admits instead: that generation is the recovered one, and
// only its own ladder can say it is again a zombie (round4 review F2).
//
// Everything else is non-terminal, because none of it is bd's answer:
//
//   - gc's own clock or cancellation. The read path's reopen runs this ladder
//     under the read's retry budget (10s), and a recover is `bd dolt stop`
//     followed by a `bd ping` that cold-starts Dolt in 30-45s on a real data
//     dir: the runner SIGKILLs it mid-start at gc's deadline. A marked
//     failure that lands after gc's context ended is counted here too — the
//     outcome raced gc's own clock, and "undetermined is never a pass" cuts
//     the same way for a demotion.
//   - a timeout of any spelling (IsIndeterminate) — the op's own budget, or
//     the in-process lifecycle slot another recover of the same zombie holds
//     (the controller's health loop), which waits out as a deadline.
//   - an unmarked failure: an environment or ownership refusal before the
//     script ran, a child killed by a signal, the script's "not needed"
//     status. bd never answered.
//
// Each holds the recover rung for failedRecoverBackoff rather than spending it
// (see GenerationSet.Backoff): the next open inside the window spends no verb,
// and the one after it may ask again once gc's contention has cleared. Before
// this, every one of them was a terminal proxy_zombie, and a long-lived handle
// whose read budget ran out mid-recover was demoted for the process.
func (in AdmissionInput) recoverFailed(ctx context.Context, root string, key proxyendpoint.PoolKey, err error) (retry bool, verdict error) {
	generation := key.Generation()
	ctxErr := ctx.Err()
	indeterminate := ctxErr != nil || proxyendpoint.IsIndeterminate(err)
	if !indeterminate && errors.Is(err, ErrProviderReportedFailure) {
		if !in.sameGeneration(root, key) {
			// bd refused, but its `bd dolt stop` ran and a new proxy wrote
			// its record before the ping inside the recover gave up — the
			// shape of a Dolt cold start longer than the proxy's own
			// serverReadyTimeout. That refusal is about the start, not about
			// the generation it left, which nobody has probed. The design
			// re-runs 3.3 on the recovered generation and ends the lane only
			// if IT is again a zombie (round4 review F2), so re-admit, as the
			// ping arm does when the generation moves under a failed ping.
			// This generation's rung stays spent (settle): if the record was
			// merely unreadable and the generation has not moved, the next
			// pass finds the recover spent and answers terminal.
			return true, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
				"the provider recover reported failure but the generation moved; re-admitting against the one it left", err)
		}
		return false, NewProxiedVerdictError(ProxiedVerdictProxyZombie,
			"the endpoint accepts and never greets, and bd ran the provider recover and reported it could not recover it", err)
	}
	in.Recovered.Backoff(generation, failedRecoverBackoff)
	if indeterminate {
		if ctxErr != nil && !errors.Is(err, ctxErr) {
			// A failure that raced gc's clock keeps its own text and gains
			// the clock's, so the operator sees both.
			err = errors.Join(err, ctxErr)
		}
		return false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBudgetExhausted,
			"the endpoint accepts and never greets, and the provider recover was cut short by gc's own clock or "+
				"cancellation; that says nothing about whether bd can recover this proxy, so the lane is not ended on it", err)
	}
	return false, NewNonTerminalProxiedVerdictError(ProxiedVerdictBackendUnreachable,
		"the endpoint accepts and never greets, and the provider recover failed before bd could answer; "+
			"not ending the lane on a failure that says nothing about the proxy", err)
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
	// succeeded or failed. Three things bound it now: Admit releases this key
	// the moment the scope admits again, because an incident that ended is not
	// a rung this one has spent; a FAILED ping holds it only for
	// failedPingBackoff (council pr2 D-F9 — it used to release it, which let
	// every open re-fork); and the ledger's own TTL expires a successful one.
	key := generation
	if key == "" {
		key = absentRecordPingKey(in.ScopeRoot)
	}
	if remaining, backing := in.Observed.BackingOff(key); backing {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(verdict,
			fmt.Sprintf("%s; the provider ping failed and is not re-forked for another %s",
				detail, remaining.Round(time.Second)), cause)
	}
	if !in.Observed.Add(key) {
		return Pin{}, false, NewNonTerminalProxiedVerdictError(verdict,
			detail+"; the provider was already pinged for this generation", cause)
	}
	if err := in.Ops.Ping(ctx, in.ScopeRoot); err != nil {
		// The rung bought nothing, so it must not stay spent for the whole
		// TTL: a ping that failed on gc's own lifecycle semaphore must not
		// poison the scope. But it is held for failedPingBackoff rather than
		// released, because a release let every later open re-fork the ping
		// with no bound at all (council pr2 D-F9).
		in.Observed.Backoff(key, failedPingBackoff)
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
// Two things bound the exposure instead, and they are stated here so a reader
// does not have to reconstruct them:
//
//  1. proxiedPinMemoTTL caps the entry at proxiedPinMemoMaxTTL however long the
//     operator's guard interval is. GC_BEADS_PROXIED_GUARD_INTERVAL has a floor
//     but no ceiling, so without the cap an operator asking for a quieter tick
//     was also asking the memo to trust a schema answer for that long.
//  2. The guard tick re-reads the cursors every interval and, on drift, now
//     forgets the memo as well as standing the handle down — so the next open
//     in the process re-derives instead of reading an answer a tick has already
//     contradicted. That is a long-lived store's bound only; a one-shot has no
//     guard, and the TTL cap is all it gets.
//
// What does NOT bound it, corrected (council pr2 D-F7). This block used to
// list a third bound: "the library runs CheckForwardDrift at EVERY open, so a
// database that moved ahead of this binary is refused by the library". That is
// true of exactly one lane and one direction, and it was cited as if it covered
// the lane A-F2 exists for. At beads v1.3.0:
//
//   - CheckForwardDrift (store.go:1991) is checkSchemaSkew over CurrentVersion,
//     which is mainSource.currentVersion (schema.go:133-162, :519-521): the
//     MAIN lane, refused only when AHEAD.
//   - A main lane BEHIND is refused by a different check, the shared-store
//     migrate gate (store.go:2939, remote_migrate_gate.go:517-557), which again
//     reads the main lane only.
//   - NOTHING in the library's open consults the IGNORED lane before
//     migrating it. So an ignored lane that moves inside the memo window — a
//     sentinel that vanishes, dropping the library's effective cursor 26 -> 11,
//     is the case that matters — is checked by nobody on a memo hit, and the
//     open replays ignored 0012-0025 against bd's database. Inside the window
//     that hazard is bounded by the TTL cap and the tick above, and by nothing
//     the library does. The post-open observation (ProxiedOpenUnmoved) does
//     not reach it either, and "a memoized pin carries no hash" — the reason
//     this line used to give — understated why (council pr2 E-S4): the replay
//     writes only the dolt_ignore'd plane, which is never committed, so HEAD
//     would not move even if the pin carried one; and the observation's
//     ignored-plane half cannot tell a replay that restored what it found
//     missing from an untouched plane, because the plane has no history and
//     the replay records its cursor with INSERT IGNORE. Carrying the hash
//     into memo hits would close nothing.
//
// One thing the old sentence feared that does NOT happen: bd's documented
// escape hatch BD_IGNORE_SCHEMA_SKEW=1 is read with os.Getenv inside the open,
// but the proxied window withholds the whole BD_ namespace for the duration of
// OpenBestAvailable (openNativeStorageProxied), so an operator's exported hatch
// cannot switch the AHEAD refusal off in-process
// (TestProxiedHermeticOpenScrubsUnlocksAndProjectsAuthorPair pins the unset).
// TestForwardDriftSeesOnlyTheMainLane pins the three library facts above.
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
