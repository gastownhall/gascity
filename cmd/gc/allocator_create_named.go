package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The named-canonical create kind (P3-6b; POOL-042, the sync create arm): a
// configured named session with no canonical row gets its closed canonical
// row reopened, or a fresh row, with legacy's metadata (AM7). The effect runs
// on the session executor and settles like a pool create, with these
// differences:
//
//   - it resolves the template read-only, serialized (AM-N3), and resolution
//     gates the reopen as legacy's skipped spec does;
//   - it takes the city flock over exactly {identity, session_name}, the lock
//     set of legacy sync and the CLI, once, and fails closed on a lock error
//     (AM-N1);
//   - inside the lock it reads the sessions store live once (the identity's
//     rows, alias and session-name availability) and writes once: a reopen
//     conditional on the revision it read (AM-N4), or a create, each after a
//     last check of its context (an effect abandoned at its deadline never
//     writes); a store with no conditional writer refuses the reopen (v5 C2);
//   - a reopen reports the row it retargets in its settlement (AM-N2) and
//     keeps the row's instance_token and generation;
//   - the row's state comes from the plan, decided from the observation cache
//     (AM-N6): the effect never probes the provider and writes no file, except
//     that once an AdoptLive write lands and the locks are released, it gives
//     the row and the runtime one identity (adoptLiveIdentity, LL5).

// namedCreatePlan is one configured named session to materialize (POOL-042,
// P3-6b §3.1).
type namedCreatePlan struct {
	Identity    string // configured_named_identity, and the alias
	SessionName string // config.NamedSessionRuntimeName
	Template    string // namedSessionBackingTemplate(spec)
	Mode        string
	BoundStepID string // computeNamedSessionDemand's work bead (POOL-039)
	// AdoptLive replaces legacy's provider probe: an alive, unattributed
	// runtime holds SessionName, so the row is created active (AM-N6).
	AdoptLive bool
}

// templateResolveMu serializes the named create's template resolution, as
// legacy resolves on its single tick goroutine pending an audit of the
// resolver's thread safety (build_desired_state.go, realization phase B).
var templateResolveMu sync.Mutex

// createNamed is a named plan's effect, from the spec check to the write.
func (x *createEffects) createNamed(ctx context.Context, pass *createPass, p createPlan, prog *createProgress) (session.Info, error) {
	plan, cfg := p.Named, pass.cfg
	prog.stage = createStageStalePlan
	spec, ok := findNamedSessionSpec(cfg, x.host.cityName, plan.Identity)
	if !ok || spec.SessionName != plan.SessionName || namedSessionBackingTemplate(spec) != plan.Template || spec.Mode != plan.Mode {
		return session.Info{}, fmt.Errorf("named session %q is not configured as planned (session %q, template %q, mode %q)",
			plan.Identity, plan.SessionName, plan.Template, plan.Mode)
	}
	prog.stage = createStagePrepare
	if err := validateAgentSessionTransport(&cfg.Workspace, cfg.Providers, x.host.lookPath, pass.sp, spec.Agent, plan.Identity); err != nil {
		return session.Info{}, err
	}
	prog.stage = createStageResolve
	tp, err := x.resolveNamed(cfg, spec, plan)
	if err != nil {
		return session.Info{}, err
	}
	var info session.Info
	prog.stage = createStageLock
	err = x.host.withLocks(x.host.cityPath, []string{plan.Identity, plan.SessionName}, func() error {
		prog.stage = createStageFence
		var err error
		info, err = x.writeNamed(ctx, pass, p, tp, prog)
		return err
	})
	if err == nil && plan.AdoptLive {
		x.adoptLiveIdentity(pass, p, info)
	}
	return info, err
}

// adoptLiveIdentity gives an AdoptLive row and the runtime it adopted one
// identity (LL5, v5 O2), after the identifier locks are released. Only a
// runtime that adoptableIdentity accepts on a fresh read, and that a fresh
// read after it confirms present, is stamped; any other is left alone, and
// so is the row: a token under a session ID belongs to that session. The
// row records the runtime's own GC_INSTANCE_TOKEN by CAS (recordRowToken),
// a runtime with no token getting a minted one, and only then is the
// runtime stamped (stampAdoptedRuntime)
// with the epoch the row holds: a lost CAS leaves the runtime with no session
// ID and a token that is not the row's, which the comparator reads as
// Unknown. A failure or a panic is logged and leaves the landed create
// landed.
func (x *createEffects) adoptLiveIdentity(pass *createPass, p createPlan, written session.Info) {
	defer func() {
		if r := recover(); r != nil {
			x.logf("allocator: named session %q adopted as %s, identity stamp panicked: %v\n", p.Named.Identity, written.ID, r)
		}
	}()
	if pass.sp == nil {
		return
	}
	if err := adoptLiveStamp(pass.sp, pass.store, p.Named.SessionName, written); err != nil {
		x.logf("allocator: named session %q adopted as %s, identity not stamped: %v\n", p.Named.Identity, written.ID, err)
	}
}

func adoptLiveStamp(sp runtime.Provider, store beads.Store, name string, written session.Info) error {
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if !known {
		return errors.New("the runtime's backend is unknown")
	}
	rt := readRuntimeIdentity(context.Background(), leaf, name)
	if !adoptableIdentity(rt) {
		return fmt.Errorf("the runtime's identity is not adoptable (%s)", compareIdentity(written, rt))
	}
	// Liveness first: a token read off a runtime that is gone (acp's leftover
	// sidecar) proves nothing, so presence is re-confirmed after the read.
	live, status, err := runtime.ObserveLivenessBoundedSince(context.Background(), leaf, name, nil, time.Now(), fenceProbeTimeout)
	if status != runtime.ObservationComplete || err != nil || !live.Present() {
		return errors.New("the runtime is not confirmed present after its identity read")
	}
	token, minted := strings.TrimSpace(rt.Token), ""
	if token == "" {
		minted = session.NewInstanceToken()
		token = minted
	}
	generation, err := recordRowToken(store, written, token)
	if err != nil {
		return err
	}
	return stampAdoptedRuntime(sp, name, written.ID, generation, minted)
}

// rowTokenAttempts bounds recordRowToken's CAS retries.
const rowTokenAttempts = 3

// recordRowToken writes token as the written row's instance_token by CAS at
// the revision a live read sees, retrying a lost CAS while the premise holds:
// the row is open, at the generation the create wrote, and carries the token
// the create wrote. It never writes blind. It returns the row's generation.
func recordRowToken(store beads.Store, written session.Info, token string) (string, error) {
	writer, _, err := beads.ResolveConditionalWriter(store)
	switch {
	case err != nil:
		return "", fmt.Errorf("recording instance_token: %w", err)
	case writer == nil:
		return "", errors.New("recording instance_token: the store has no conditional writer")
	}
	live := beads.HandlesFor(store).Live
	for attempt := 1; ; attempt++ {
		row, err := live.Get(written.ID)
		if err != nil {
			return "", fmt.Errorf("recording instance_token: %w", err)
		}
		generation, current := row.Metadata["generation"], row.Metadata["instance_token"]
		switch {
		case row.Status == "closed" || generation != written.Generation:
			return "", errors.New("recording instance_token: the row moved on after the create")
		case current == token:
			return generation, nil
		case current != written.InstanceToken:
			return "", errors.New("recording instance_token: the row holds a token the create did not write")
		}
		err = writer.UpdateIfMatch(written.ID, row.Revision, beads.UpdateOpts{Metadata: map[string]string{"instance_token": token}})
		var lost *beads.PreconditionFailedError
		switch {
		case err == nil:
			return generation, nil
		case !errors.As(err, &lost) || attempt == rowTokenAttempts:
			return "", fmt.Errorf("recording instance_token: %w", err)
		}
	}
}

// resolveNamed resolves the plan's template read-only, as legacy's desired
// state resolves it for a named session. With no store, the session name
// derives from config alone: the spec's runtime name, which the spec check
// matched against the plan's.
func (x *createEffects) resolveNamed(cfg *config.City, spec namedSessionSpec, plan *namedCreatePlan) (TemplateParams, error) {
	bp := newReadOnlyAgentBuildParams(x.host.cityName, x.host.cityPath, cfg, x.host.lookPath, x.host.now(), x.host.stderr)
	tp, err := func() (TemplateParams, error) {
		templateResolveMu.Lock()
		defer templateResolveMu.Unlock()
		return resolveTemplate(bp, spec.Agent, plan.Identity, buildFingerprintExtra(spec.Agent))
	}()
	if err != nil {
		return TemplateParams{}, err
	}
	applyNamedTemplateOverrides(&tp, spec, plan.Identity, plan.BoundStepID)
	return tp, nil
}

// writeNamed is the locked step: the fenced live read, then one write. The
// read covers the sessions store alone, by invariant: configured named rows
// live only in the sessions store, and C11 refuses duplicates on other legs
// at boot. A partial or failed read of it fails the create closed.
func (x *createEffects) writeNamed(ctx context.Context, pass *createPass, p createPlan, tp TemplateParams, prog *createProgress) (session.Info, error) {
	plan, cfg, store := p.Named, pass.cfg, pass.store
	now := x.host.now().UTC()
	live := liveFenceStore{Store: store, live: beads.HandlesFor(store).Live}
	rows, err := session.NamedSessionIdentityRows(live, plan.Identity)
	if err != nil {
		return session.Info{}, err
	}
	if closed, ok := session.ClosedNamedSessionBeadIn(rows, plan.SessionName); ok {
		return reopenNamed(ctx, store, live, cfg, p, closed, now, prog)
	}
	if err := session.EnsureAliasAvailableWithConfigForOwner(live, cfg, plan.Identity, "", plan.Identity); err != nil {
		return session.Info{}, fmt.Errorf("alias %q for %s unavailable: %w", plan.Identity, plan.Identity, err)
	}
	if err := session.EnsureSessionNameAvailableWithConfigForOwner(live, cfg, plan.SessionName, "", plan.Identity); err != nil {
		return session.Info{}, fmt.Errorf("session_name %q for %s unavailable: %w", plan.SessionName, plan.Identity, err)
	}
	state := string(session.StateStartPending)
	if plan.AdoptLive {
		state = string(session.StateActive)
	}
	liveHash := runtime.LiveFingerprint(templateParamsToConfig(tp))
	meta := syncCreateMetadata(tp, plan.SessionName, plan.Identity, liveHash, state, p.Token, 0, now)
	meta["alias"] = plan.Identity
	if err := checkCreateContext(ctx); err != nil {
		return session.Info{}, err
	}
	prog.writing = true
	info, err := sessionFrontDoor(store).CreateSessionInfo(session.CreateSpec{Title: plan.Identity, AgentName: plan.Identity, Metadata: meta})
	if err != nil {
		return session.Info{}, poolCreateWriteError{err: fmt.Errorf("creating named session %q: %w", plan.Identity, err)}
	}
	return info, nil
}

// reopenNamed reopens closed, the identity's closed canonical row, with
// legacy's reopen batch in one write, conditional on the revision the fenced
// read saw. A store that resolves no conditional writer refuses at the write,
// after the fence checks: legacy's last-writer-wins transaction is never a v2
// write (v5 C2, R3). The
// settlement names the row whatever the outcome; settle reads a refused
// write (a lost fence, a writer that cannot fence) as no write.
func reopenNamed(ctx context.Context, store beads.Store, live beads.Store, cfg *config.City, p createPlan, closed beads.Bead, now time.Time, prog *createProgress) (session.Info, error) {
	plan := p.Named
	prog.retarget = closed.ID
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return session.Info{}, fmt.Errorf("reopening configured named session %q: %w", plan.Identity, err)
	}
	state := "stopped"
	if plan.AdoptLive {
		state = string(session.StateActive)
	}
	batch := reopenNamedSessionBatch(state, closed.Metadata["sleep_reason"], now)
	maps.Copy(batch, startupKickoffReopenMetadata(plan.BoundStepID, now))
	open := "open"
	opts := beads.UpdateOpts{Status: &open, Metadata: batch}
	reopened, err := reopenClosedConfiguredNamedSessionBeadLocked(live, cfg, plan.Identity, plan.SessionName, closed, batch, func() error {
		if err := checkCreateContext(ctx); err != nil {
			return err
		}
		if writer == nil {
			prog.stage = createStageNoWriter
			return errNoConditionalWriter
		}
		prog.writing, prog.rowID = true, closed.ID
		return writer.UpdateIfMatch(closed.ID, closed.Revision, opts)
	})
	var written reopenWriteError
	switch {
	case err == nil:
		return session.Info{ID: reopened.ID, InstanceToken: reopened.Metadata["instance_token"], Generation: reopened.Metadata["generation"]}, nil
	case !errors.As(err, &written) || errors.Is(err, errCreateAbandoned) || errors.Is(err, errNoConditionalWriter):
		return session.Info{}, err
	}
	return session.Info{}, poolCreateWriteError{err: err, rowID: closed.ID}
}

// liveFenceStore reads the sessions store through its Live handle, so the
// fenced read sees a row another process wrote that the cache has not caught
// up with (a cached read can miss an out-of-process reopen).
type liveFenceStore struct {
	beads.Store
	live beads.LiveReader
}

func (s liveFenceStore) Get(id string) (beads.Bead, error) { return s.live.Get(id) }

func (s liveFenceStore) List(q beads.ListQuery) ([]beads.Bead, error) { return s.live.List(q) }
