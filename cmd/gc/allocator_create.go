package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The allocator's create effects (CONTRACT §7, C1.9, C1.10): the only writer
// of new session rows under v2. The pass decides and reserves a create entry
// in the intent ledger; an effect then issues the entry, runs legacy's fenced
// create from an effect-local view (createPoolSessionBeadWithGuardedAliasUsingLock),
// and settles the entry with the marker the census will show (AM1). Effects
// write only the new row and never probe a provider: the pass decided
// singleton occupancy from the observation cache (C7.3).
//
// Unwired in this slice: P3-5b reserves the entries and builds the plans,
// P3-7 submits them after each pass and shuts the executor down with the
// runtime. Named-session creates run on the same executor
// (allocator_create_named.go).

// createEffectParallelism bounds concurrent create effects, as legacy bounds
// its planned creates (POOL-053, C1.10).
const createEffectParallelism = poolRealizeParallelism

// createPlan is one fresh pool or dependency-floor row, or one configured
// named session (Named), the allocator admitted under the create entry
// EntryID. The entry, not the plan, carries the instance token: the effect
// reads it when it issues the entry, so the row's token and the ledger
// marker cannot diverge.
type createPlan struct {
	EntryID           string
	Template          string
	QualifiedInstance string
	Slot              int
	// Metadata is the trigger and provenance from poolTriggerMetadata. Under
	// planOnly a request whose work dir needs worktree.Verify has no
	// gc.work_dir here and carries WorktreeSpec instead; the effect verifies
	// the spec and stamps the work dir before it writes the row.
	Metadata     map[string]string
	WorktreeSpec *worktree.Spec
	// Named is set for a configured named session's create or reopen; the
	// pool fields above are then unused.
	Named *namedCreatePlan
}

// identity is the create identity the plan materializes: the key of its
// create veto (AM-N8).
func (p createPlan) identity() createIdentity {
	if p.Named != nil {
		return createIdentity{Template: p.Named.Template, QualifiedInstance: p.Named.Identity, Named: true}
	}
	return createIdentity{Template: p.Template, QualifiedInstance: p.QualifiedInstance, Slot: p.Slot}
}

// agentIn returns the agent cfg configures for c: c's template has one, and
// it derives c's instance and pool slot from c's slot, as the planner does
// (poolDesiredRequestIdentity). Legacy creates with the plan's slot, so the
// slot must equal the pool slot (a canonical singleton's are both 0). A named
// identity's agent is its configured named session's backing agent.
func (c createIdentity) agentIn(cfg *config.City) (*config.Agent, error) {
	if c.Named {
		spec, ok := findNamedSessionSpec(cfg, "", c.QualifiedInstance)
		if !ok {
			return nil, fmt.Errorf("named session %q is not configured", c.QualifiedInstance)
		}
		return spec.Agent, nil
	}
	cfgAgent := findAgentByTemplate(cfg, c.Template)
	if cfgAgent == nil {
		return nil, fmt.Errorf("pool template %q has no configured agent", c.Template)
	}
	if _, qualifiedInstance, poolSlot := poolDesiredRequestIdentity(cfgAgent, c.Slot); qualifiedInstance != c.QualifiedInstance || poolSlot != c.Slot {
		return nil, fmt.Errorf("create identity %q slot %d is not template %q's (instance %q, pool slot %d)",
			c.QualifiedInstance, c.Slot, c.Template, qualifiedInstance, poolSlot)
	}
	return cfgAgent, nil
}

// createIdentityConfigured is PruneCreateVetoes' predicate for cfg.
func createIdentityConfigured(cfg *config.City) func(createIdentity) bool {
	return func(c createIdentity) bool {
		_, err := c.agentIn(cfg)
		return err == nil
	}
}

// createPlanOf adapts a planner create plan (selectOrPlanPoolSessionBead, or
// the dependency floor's) to the create entry entryID.
func createPlanOf(entryID, template string, p poolSessionCreatePlan) createPlan {
	return createPlan{
		EntryID:           entryID,
		Template:          template,
		QualifiedInstance: p.qualifiedInstance,
		Slot:              p.slot,
		Metadata:          p.metadata,
		WorktreeSpec:      p.worktreeSpec,
	}
}

// createPass is what one allocator pass hands its creates: the config, the
// provider and the stores of the environment it decided under, and the
// census rows it planned against. The pass must not change it after submit.
//
// planning holds census rows only (C7.2), never the planning reservations
// of uncleared creates: a create's own reservation carries its own
// identifiers, so the fenced check would refuse the create, or drop its
// alias, on its own name. Creates in flight fence each other through the
// identifier locks and the live re-census instead.
//
// An effect writes through the stores it was handed. After a store swap, a
// closed store fails the effect: before the write as a no-write failure, or
// at the write itself, which settles as ambiguous (a closed store and a lost
// write look alike), so lag repair decides.
type createPass struct {
	cfg *config.City
	// sp answers transport capability checks only; no effect probes it.
	sp                runtime.Provider
	store             beads.Store
	rigStores         map[string]beads.Store
	suspendedRigPaths map[string]bool
	planning          []session.Info
}

// createEffectHost is what the executor needs from the v2 runtime.
type createEffectHost struct {
	cityPath string
	cityName string
	lookPath config.LookPathFunc
	ledger   *intentLedger
	// enqueue wakes the allocator on every settle (C5.12) and the new row's
	// session key on a commit.
	enqueue v2Enqueuer
	// withLocks takes the city identifier locks; nil means
	// session.WithCitySessionIdentifierLocks.
	withLocks poolSessionIdentifierLockFunc
	// verify is worktree.Verify unless a test injects one.
	verify func(worktree.Spec) (worktree.Report, error)
	// verdicts is the worktree verdict cache the planner reads. It is
	// required: a private cache would hide every verdict from the planner.
	verdicts *worktreeVerdicts
	now      func() time.Time
	stderr   io.Writer
}

// createEffects is the executor: at most createEffectParallelism effects run
// at once, on workers that exit when the queue drains.
type createEffects struct {
	host createEffectHost

	mu      sync.Mutex
	queue   []createJob
	workers int
	stopped bool
	wg      sync.WaitGroup
}

type createJob struct {
	pass *createPass
	plan createPlan
}

// newCreateEffects builds the executor. It refuses a host without the
// ledger, the shared verdict cache or the city path: with no city path the
// identifier locks would fence this process only (C7.1 tier 2).
func newCreateEffects(h createEffectHost) (*createEffects, error) {
	switch {
	case h.ledger == nil:
		return nil, errors.New("create effects: no intent ledger")
	case h.verdicts == nil:
		return nil, errors.New("create effects: no worktree verdict cache")
	case strings.TrimSpace(h.cityPath) == "":
		return nil, errors.New("create effects: no city path for the identifier locks")
	}
	if h.withLocks == nil {
		h.withLocks = session.WithCitySessionIdentifierLocks
	}
	if h.verify == nil {
		h.verify = worktree.Verify
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.stderr == nil {
		h.stderr = io.Discard
	}
	return &createEffects{host: h}, nil
}

// submit queues pass's plans. It refuses them once shutdown began: their
// entries stay reserved, never issued.
func (x *createEffects) submit(pass *createPass, plans ...createPlan) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.stopped {
		return false
	}
	for _, p := range plans {
		x.queue = append(x.queue, createJob{pass: pass, plan: p})
	}
	for range plans {
		if x.workers >= createEffectParallelism {
			break
		}
		x.workers++
		x.wg.Add(1)
		go x.work()
	}
	return true
}

// shutdown stops admission, drops the queued plans, and waits for the
// effects in flight until ctx ends (C1.8). It holds no lock while it waits.
// A create that lands after shutdown began is durable, and the next process
// counts its row as in flight (C5.14).
func (x *createEffects) shutdown(ctx context.Context) error {
	x.mu.Lock()
	x.stopped, x.queue = true, nil
	x.mu.Unlock()
	done := make(chan struct{})
	go func() {
		x.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (x *createEffects) work() {
	defer x.wg.Done()
	for {
		x.mu.Lock()
		if x.stopped || len(x.queue) == 0 {
			x.workers--
			x.mu.Unlock()
			return
		}
		job := x.queue[0]
		x.queue = x.queue[1:]
		x.mu.Unlock()
		x.run(job)
	}
}

// Create effect stages. A no-write failure names the stage it failed in as
// its create veto's cause (ineligible:create-refused:<cause>).
const (
	createStageStalePlan = "stale-plan" // the plan's template or identity left config
	createStageWorktree  = "worktree"   // the plan's worktree evidence failed verification
	createStagePrepare   = "prepare"    // transport, tmux alias, identifiers
	createStageLock      = "lock"       // the city identifier locks
	createStageFence     = "fence"      // the locked re-census and availability checks
	createStagePanic     = "panic"      // a panic before the write
	createStageResolve   = "resolve"    // a named create's read-only template resolution
)

// createProgress is how far one effect got: the stage a no-write failure
// names, and whether the row write began, and on which row when known.
type createProgress struct {
	stage   string
	writing bool
	rowID   string
}

// run is one create effect. It performs no effect unless it wins the
// reserved → issued CAS (C5.1); after that every return, panic included,
// settles the entry (C5.6, C1.7). A panic before the row write wrote
// nothing; from the write on, the row may exist, so it settles as ambiguous.
func (x *createEffects) run(job createJob) {
	p := job.plan
	token, ok := x.host.ledger.IssueCreate(p.EntryID)
	if !ok {
		return // released first: the allocator re-decides
	}
	var (
		info session.Info
		err  error
		prog = createProgress{stage: createStagePrepare}
	)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("create effect panic: %v", r)
			if prog.writing {
				err = poolCreateWriteError{err: err, rowID: prog.rowID}
			} else {
				prog.stage = createStagePanic
			}
		}
		x.settle(p, token, prog.stage, info, err)
	}()
	if p.Named != nil {
		info, err = x.createNamed(job.pass, p, token, &prog)
		return
	}
	cfgAgent, err := p.identity().agentIn(job.pass.cfg)
	if err != nil {
		prog.stage = createStageStalePlan
		return
	}
	prog.stage = createStageWorktree
	metadata, err := x.verifiedMetadata(p)
	if err != nil {
		return
	}
	prog.stage = createStagePrepare
	view := x.view(job.pass, token)
	view.beforeWrite = func(id string) { prog.writing, prog.rowID = true, id }
	locks := func(cityPath string, identifiers []string, fn func() error) error {
		prog.stage = createStageLock
		return x.host.withLocks(cityPath, identifiers, func() error {
			prog.stage = createStageFence
			return fn()
		})
	}
	info, err = createPoolSessionBeadWithGuardedAliasUsingLock(view, cfgAgent, p.Template, p.QualifiedInstance, p.Slot, metadata, locks)
}

// verifiedMetadata verifies the plan's worktree evidence and stamps the
// verified work dir as poolTriggerMetadata does outside planOnly (POOL-055,
// #34). A failure writes nothing and is remembered, so the planner stops
// binding the work while its evidence stands.
func (x *createEffects) verifiedMetadata(p createPlan) (map[string]string, error) {
	if p.WorktreeSpec == nil {
		return p.Metadata, nil
	}
	report, err := x.host.verify(*p.WorktreeSpec)
	if err != nil {
		x.host.verdicts.record(*p.WorktreeSpec, x.host.now())
		return nil, fmt.Errorf("%w: verification failed: %w", errPoolTriggerWorktreeEvidence, err)
	}
	x.host.verdicts.verified(p.WorktreeSpec.BeadID)
	metadata := make(map[string]string, len(p.Metadata)+2)
	maps.Copy(metadata, p.Metadata)
	if report.Path != "" {
		metadata[beadmeta.WorkDirMetadataKey] = report.Path
		metadata[beadmeta.LegacyWorkDirMetadataKey] = report.Path
	}
	return metadata, nil
}

// view is the effect-local view of the guarded create: no runtime probe, and
// no primary snapshot, since the ledger entry represents the row until the
// census shows it.
func (x *createEffects) view(pass *createPass, token string) poolCreateView {
	cfg := pass.cfg
	return poolCreateView{
		cityPath:          x.host.cityPath,
		city:              cfg,
		store:             pass.store,
		rigStores:         pass.rigStores,
		suspendedRigPaths: pass.suspendedRigPaths,
		planning:          pass.planning,
		validateTransport: func(cfgAgent *config.Agent, qualifiedName string) error {
			return validateAgentSessionTransport(&cfg.Workspace, cfg.Providers, x.host.lookPath, pass.sp, cfgAgent, qualifiedName)
		},
		tmuxAlias: func(cfgAgent *config.Agent) (string, error) {
			return resolveTmuxAliasForAgentIn(x.host.cityPath, x.host.cityName, cfg.Rigs, cfgAgent)
		},
		startedAt:     func() time.Time { return x.host.now().UTC() },
		instanceToken: token,
	}
}

// settle records the effect's outcome and wakes the allocator (C5.12). A
// create commits with its row ID and token, and resets its identity's create
// veto. An error from the write itself is ambiguous, since the row may
// exist: it commits with the token (and the row ID, when known) as its
// marker, and lag repair decides (C5.4, C5.15); an ambiguous named reopen
// also wakes its row's session key, since a Tx reopen raises no event on the
// binding. A write the store refused (createWriteRefused), and any other
// error, wrote nothing: the entry fails, its clear refunds, and its identity
// gets a create veto (AM-N8), except for failed worktree evidence, which the
// verdict cache throttles per work item rather than per slot.
//
// The ledger moves first. A panic after it (a wake or log sink) is
// recovered: the entry is settled, and the worker lives on.
func (x *createEffects) settle(p createPlan, token, stage string, info session.Info, err error) {
	defer func() {
		if r := recover(); r != nil {
			x.logf("allocator: settling create %s: panic: %v\n", p.EntryID, r)
		}
	}()
	var (
		written poolCreateWriteError
		keys    []reconcilekey.Key
	)
	switch {
	case err == nil:
		x.host.ledger.CommitCreate(p.EntryID, p.identity(), ledgerMarker{RowID: info.ID, InstanceToken: token})
		x.wake(reconcilekey.Session(info.ID))
		return
	case errors.As(err, &written) && !createWriteRefused(err):
		x.host.ledger.Commit(p.EntryID, ledgerMarker{RowID: written.rowID, InstanceToken: token})
		if p.Named != nil && written.rowID != "" {
			keys = append(keys, reconcilekey.Session(written.rowID))
		}
	case stage == createStageWorktree:
		x.host.ledger.Fail(p.EntryID, false, ledgerMarker{})
	default:
		x.host.ledger.FailCreate(p.EntryID, p.identity(), stage)
	}
	subject := p.Template
	if p.Named != nil {
		subject = p.Named.Identity
	}
	x.logf("allocator: create %s for %q: %v\n", p.EntryID, subject, err)
	x.wake(keys...)
}

// createWriteRefused reports a write error that proves the store wrote
// nothing: a lost revision fence, a store that cannot fence, a backend gate
// refusal, or a not-found (bd's classification of a code-less not-found,
// bdstore_conditional.go). The connection class, where the write may have
// committed, is none of these.
func createWriteRefused(err error) bool {
	return beads.IsPreconditionFailed(err) || beads.IsConditionalWriteUnsupported(err) ||
		beads.IsGateRefusal(err) || errors.Is(err, beads.ErrNotFound)
}

// logf reports to stderr. A panicking writer is ignored: it must not kill a
// worker.
func (x *createEffects) logf(format string, args ...any) {
	defer func() { _ = recover() }()
	fmt.Fprintf(x.host.stderr, format, args...) //nolint:errcheck
}

func (x *createEffects) wake(keys ...reconcilekey.Key) {
	if x.host.enqueue != nil {
		x.host.enqueue("create", append([]reconcilekey.Key{reconcilekey.Allocator()}, keys...)...)
	}
}

// worktreeVerdicts remembers, per work bead, the worktree evidence a create
// effect failed to verify (C6.2, C6.5a, POOL-054/055, #34). The planner skips
// binding a work bead while its verdict stands, so a bad work item is not
// created and refused on every pass. A verdict stands for C5.11's backoff
// (10s doubling per consecutive failure of the same evidence, capped at 5m),
// so evidence repaired out of band is retried. New evidence for the bead
// (another generation, path or branch) is verified afresh, a successful
// verify drops the verdict, and prune drops beads no longer in demand.
type worktreeVerdicts struct {
	mu     sync.Mutex
	failed map[string]worktreeVerdict
}

type worktreeVerdict struct {
	spec        worktree.Spec
	until       time.Time
	consecutive int
}

func (v *worktreeVerdicts) record(spec worktree.Spec, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failed == nil {
		v.failed = make(map[string]worktreeVerdict)
	}
	f := v.failed[spec.BeadID]
	if f.spec != spec {
		f = worktreeVerdict{spec: spec}
	}
	if !f.until.After(now) {
		f.consecutive++
	}
	f.until = now.Add(vetoBackoff(f.consecutive))
	v.failed[spec.BeadID] = f
}

// verified drops beadID's verdict: its evidence verified.
func (v *worktreeVerdicts) verified(beadID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.failed, beadID)
}

// refuses reports whether spec is evidence that failed verification and
// whose verdict still stands at now.
func (v *worktreeVerdicts) refuses(spec worktree.Spec, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	f, ok := v.failed[spec.BeadID]
	return ok && f.spec == spec && f.until.After(now)
}

// prune drops the verdict of every bead not in demand, so the cache stays
// bounded by the current demand set. The planner calls it each pass.
func (v *worktreeVerdicts) prune(demand map[string]bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for beadID := range v.failed {
		if !demand[beadID] {
			delete(v.failed, beadID)
		}
	}
}
