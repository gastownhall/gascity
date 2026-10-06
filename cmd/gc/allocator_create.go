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
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// The allocator's create effects (CONTRACT §7, C1.9, C1.10): the only writer
// of new session rows under v2. The planner records an in-flight create entry
// and mints the plan's instance token at submit; an effect runs legacy's
// fenced create from an effect-local view
// (createPoolSessionBeadWithGuardedAliasUsingLock) and posts one settlement
// (CONTRACT v5 P1, P5, C1). The settlement is its only output: it mutates no
// planner state. Effects write only the new row
// and never probe a provider: the pass decided singleton occupancy from the
// observation cache (C7.3).
//
// Unwired in this slice: C4b submits the plans from the planner and runs them
// on the session executor. Named-session creates run on the same executor
// (allocator_create_named.go).

// createEffectParallelism bounds concurrent create effects, as legacy bounds
// its planned creates (POOL-053, C1.10).
const createEffectParallelism = poolRealizeParallelism

// createPlan is one fresh pool or dependency-floor row, or one configured
// named session (Named), the allocator admitted under the create entry
// EntryID. The planner mints Token at submit and records it in the entry, so
// the row's token and the entry's cannot diverge (S-8); Seq is the entry's
// submit, which the settlement echoes; ConfigRev is the revision the plan was
// decided under.
type createPlan struct {
	EntryID           string
	Seq               uint64
	Token             string
	ConfigRev         string
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
// in-flight entry and of its refusal record (AM-N8).
func (p createPlan) identity() createIdentity {
	if p.Named != nil {
		return createIdentity{Template: p.Named.Template, QualifiedInstance: p.Named.Identity, Named: true}
	}
	return createIdentity{Template: p.Template, QualifiedInstance: p.QualifiedInstance, Slot: p.Slot}
}

// createIdentity is the identity a create plan materializes. Create
// backoff records key on it (AM-N8): a row key does not exist until the
// create lands.
type createIdentity struct {
	Template          string
	QualifiedInstance string
	// Slot is the plan's pool slot. It is not part of the key; agentIn reads
	// it to re-derive the identity from config.
	Slot int
	// Named marks a configured named session's create: QualifiedInstance is
	// its identity, and Template its backing template.
	Named bool
}

func (c createIdentity) key() string {
	if c.Named {
		return "named:" + c.QualifiedInstance
	}
	return c.Template + "/" + c.QualifiedInstance
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
// closed store fails the effect as a no-write failure, at the write too: a
// closed store refuses before it writes (createWriteRefused).
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
	// settle posts an effect's settlement to the planner's queue, which
	// applies it to its in-flight map and backoff table (P1). It is
	// required, it must not block, and it must be safe for concurrent use:
	// effects call it from their own goroutines.
	settle func(createSettlement)
	// withLocks takes the city identifier locks; nil means
	// session.WithCitySessionIdentifierLocks.
	withLocks poolSessionIdentifierLockFunc
	// verify is worktree.Verify unless a test injects one.
	verify func(worktree.Spec) (worktree.Report, error)
	now    func() time.Time
	stderr io.Writer
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

// newCreateEffects builds the executor. It refuses a host without settle or
// the city path: with no city path the identifier locks would fence this
// process only (C7.1 tier 2).
func newCreateEffects(h createEffectHost) (*createEffects, error) {
	switch {
	case h.settle == nil:
		return nil, errors.New("create effects: no settlement sink")
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

// submit queues pass's plans. It refuses them once shutdown began: they
// never run and never settle.
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
// its refusal's cause (ineligible:create-refused:<cause>).
const (
	createStageStalePlan = "stale-plan" // the plan's template or identity left config
	createStageWorktree  = "worktree"   // the plan's worktree evidence failed verification
	createStagePrepare   = "prepare"    // transport, tmux alias, identifiers
	createStageLock      = "lock"       // the city identifier locks
	createStageFence     = "fence"      // the locked re-census and availability checks
	createStageFenceRead = "fence-read" // a pool create's locked failure that proves no name taken
	createStagePanic     = "panic"      // a panic before the write
	createStageResolve   = "resolve"    // a named create's read-only template resolution
)

// createProgress is how far one effect got: the stage a no-write failure
// names, whether the row write began, and on which row when known; the
// closed row a named create reopens; and the worktree verdict, when the plan
// carried evidence.
type createProgress struct {
	stage    string
	writing  bool
	rowID    string
	retarget string
	work     *workVerdict
}

// run is one create effect. Every return, panic included, settles it once
// (P3, P6). A panic before the row write wrote nothing; from the write on,
// the row may exist, so it settles as ambiguous. A plan without its token
// refuses before anything: a minted token would never match the entry's.
func (x *createEffects) run(job createJob) {
	p := job.plan
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
		x.settle(p, prog, info, err)
	}()
	if p.Token == "" {
		err = fmt.Errorf("create plan %s has no instance token", p.EntryID)
		return
	}
	if p.Named != nil {
		info, err = x.createNamed(job.pass, p, &prog)
		return
	}
	cfgAgent, err := p.identity().agentIn(job.pass.cfg)
	if err != nil {
		prog.stage = createStageStalePlan
		return
	}
	prog.stage = createStageWorktree
	metadata, err := x.verifiedMetadata(p, &prog)
	if err != nil {
		return
	}
	prog.stage = createStagePrepare
	view := x.view(job.pass, p.Token)
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
// #34). It reports the verdict in the settlement: on a failure, which writes
// nothing, the planner stops binding the work while the same evidence stands
// (C6.5(a)); on a success, it forgets an earlier refusal.
func (x *createEffects) verifiedMetadata(p createPlan, prog *createProgress) (map[string]string, error) {
	if p.WorktreeSpec == nil {
		return p.Metadata, nil
	}
	report, err := x.host.verify(*p.WorktreeSpec)
	prog.work = &workVerdict{BeadID: p.WorktreeSpec.BeadID, Fingerprint: specFingerprint(*p.WorktreeSpec), Refused: err != nil}
	if err != nil {
		return nil, fmt.Errorf("%w: verification failed: %w", errPoolTriggerWorktreeEvidence, err)
	}
	metadata := make(map[string]string, len(p.Metadata)+2)
	maps.Copy(metadata, p.Metadata)
	if report.Path != "" {
		metadata[beadmeta.WorkDirMetadataKey] = report.Path
		metadata[beadmeta.LegacyWorkDirMetadataKey] = report.Path
	}
	return metadata, nil
}

// view is the effect-local view of the guarded create: no runtime probe, and
// no primary snapshot, since the in-flight entry represents the row until the
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

// createSettlement is one create effect's settlement. The planner applies it
// to its in-flight map (createSettlement.settlement), then to its backoff
// table (P1): a landed create resets Identity's record; a
// non-empty Stage refuses Identity with cause Stage under ConfigRev (AM-N8);
// Work refuses or resets the work item's record (C6.5(a)).
type createSettlement struct {
	EntryID   string
	Seq       uint64
	Identity  string // createIdentity.key
	Token     string
	ConfigRev string
	// Stage is the cause of a no-write failure's refusal, empty for a landed
	// or ambiguous create and for failed worktree evidence, which throttles
	// the work item rather than the slot. A pool create's cause is "fence"
	// only when the locked checks proved its name taken; any other failure
	// under the locks (a failed live re-census or alias query, a refused
	// write) is "fence-read", which stalls the request where "fence" moves it
	// to the next slot (F3).
	Stage string
	RowID string
	// Landed: the row was written. Ambiguous: the write call failed after it
	// may have landed, so the row may exist (P5).
	Landed    bool
	Ambiguous bool
	// RetargetRowID is the closed row a named create reopens (AM-N2).
	RetargetRowID string
	Work          *workVerdict
	Err           error
	At            time.Time
}

// workVerdict is a plan's worktree verification: Fingerprint is the
// evidence's (specFingerprint).
type workVerdict struct {
	BeadID      string
	Fingerprint string
	Refused     bool
}

// settle builds the effect's settlement and posts it. A create commits with
// its row ID and token. An error from the write itself is ambiguous, since
// the row may exist: it settles with the token (and the row ID, when known).
// A row write the store refused (createWriteRefused), and any
// other error, wrote nothing. A panic in the sink or the log is recovered, so
// the worker lives on.
func (x *createEffects) settle(p createPlan, prog createProgress, info session.Info, err error) {
	defer func() {
		if r := recover(); r != nil {
			x.logf("allocator: settling create %s: panic: %v\n", p.EntryID, r)
		}
	}()
	s := createSettlement{
		EntryID: p.EntryID, Seq: p.Seq, Identity: p.identity().key(), Token: p.Token, ConfigRev: p.ConfigRev,
		RetargetRowID: prog.retarget, Work: prog.work, Err: err, At: x.host.now(),
	}
	var written poolCreateWriteError
	switch {
	case err == nil:
		s.Landed, s.RowID = true, info.ID
	case errors.As(err, &written) && (written.landed || !createWriteRefused(err)):
		s.Ambiguous, s.RowID = true, written.rowID
	case prog.stage == createStageWorktree:
	case prog.stage == createStageFence && p.Named == nil && !errors.Is(err, errPoolSessionNameUnavailable):
		s.Stage = createStageFenceRead
	default:
		s.Stage = prog.stage
	}
	x.host.settle(s)
	if err == nil {
		return
	}
	subject := p.Template
	if p.Named != nil {
		subject = p.Named.Identity
	}
	x.logf("allocator: create %s for %q: %v\n", p.EntryID, subject, err)
}

// createWriteRefused reports a write error that proves the store wrote
// nothing: a lost revision fence, a store that cannot fence, a backend gate
// refusal, a not-found (bd's classification of a code-less not-found,
// bdstore_conditional.go), a closed store (SQLite's ensureOpen and native
// Dolt's acquireStorage refuse before any write; no other store returns
// it), or a SQLite write whose busy retries ran out uncommitted. The
// connection class, where the write may have committed, is none of these.
func createWriteRefused(err error) bool {
	return beads.IsPreconditionFailed(err) || beads.IsConditionalWriteUnsupported(err) ||
		beads.IsGateRefusal(err) || errors.Is(err, beads.ErrNotFound) ||
		errors.Is(err, beads.ErrStoreClosed) || errors.Is(err, beads.ErrSQLiteBusyExhausted)
}

// logf reports to stderr. A panicking writer is ignored: it must not kill a
// worker.
func (x *createEffects) logf(format string, args ...any) {
	defer func() { _ = recover() }()
	fmt.Fprintf(x.host.stderr, format, args...) //nolint:errcheck
}
