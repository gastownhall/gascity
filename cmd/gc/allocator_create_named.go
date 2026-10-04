package main

import (
	"errors"
	"fmt"
	"maps"
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
// on the create executor and settles like a pool create, with these
// differences:
//
//   - it resolves the template read-only, serialized (AM-N3), and resolution
//     gates the reopen as legacy's skipped spec does;
//   - it takes the city flock over exactly {identity, session_name}, the lock
//     set of legacy sync and the CLI, once, and fails closed on a lock error
//     (AM-N1);
//   - inside the lock it reads the sessions store live once (the identity's
//     rows, alias and session-name availability) and writes once: a reopen
//     conditional on the revision it read (AM-N4), or a create;
//   - a reopen retargets its ledger entry to the row before the write
//     (AM-N2) and keeps the row's instance_token and generation;
//   - the row's state comes from the plan, decided from the observation cache
//     (AM-N6): the effect never probes the provider and writes no file.

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
func (x *createEffects) createNamed(pass *createPass, p createPlan, token string, prog *createProgress) (session.Info, error) {
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
		info, err = x.writeNamed(pass, p, tp, token, prog)
		return err
	})
	return info, err
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

// writeNamed is the locked step: the fenced live read, then one write.
func (x *createEffects) writeNamed(pass *createPass, p createPlan, tp TemplateParams, token string, prog *createProgress) (session.Info, error) {
	plan, cfg, store := p.Named, pass.cfg, pass.store
	now := x.host.now().UTC()
	live := liveFenceStore{Store: store, live: beads.HandlesFor(store).Live}
	rows, err := session.NamedSessionIdentityRows(live, plan.Identity)
	if err != nil {
		return session.Info{}, err
	}
	if closed, ok := session.ClosedNamedSessionBeadIn(rows, plan.SessionName); ok {
		return x.reopenNamed(store, live, cfg, p, closed, now, prog)
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
	meta := syncCreateMetadata(tp, plan.SessionName, plan.Identity, liveHash, state, token, 0, now)
	meta["alias"] = plan.Identity
	prog.writing = true
	info, err := sessionFrontDoor(store).CreateSessionInfo(session.CreateSpec{Title: plan.Identity, AgentName: plan.Identity, Metadata: meta})
	if err != nil {
		return session.Info{}, poolCreateWriteError{err: fmt.Errorf("creating named session %q: %w", plan.Identity, err)}
	}
	return info, nil
}

// reopenNamed reopens closed, the identity's closed canonical row, with
// legacy's reopen batch in one write: conditional on the revision the
// fenced read saw where the store resolves a conditional writer, else
// legacy's transaction (last writer wins, design §4a N3). The entry names
// the row, and the revision read, before anything is checked or written;
// settle reads a refused write (a lost fence, a writer that cannot fence)
// as no write.
func (x *createEffects) reopenNamed(store beads.Store, live beads.Store, cfg *config.City, p createPlan, closed beads.Bead, now time.Time, prog *createProgress) (session.Info, error) {
	plan := p.Named
	writer, _, err := beads.ResolveConditionalWriter(store)
	if err != nil {
		return session.Info{}, fmt.Errorf("reopening configured named session %q: %w", plan.Identity, err)
	}
	if !x.host.ledger.Retarget(p.EntryID, closed.ID, closed.Revision) {
		return session.Info{}, fmt.Errorf("reopening configured named session %q: create entry %s is no longer issued", plan.Identity, p.EntryID)
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
		prog.writing, prog.rowID = true, closed.ID
		if writer != nil {
			return writer.UpdateIfMatch(closed.ID, closed.Revision, opts)
		}
		return store.Tx("gc: reopen configured named session "+closed.ID, func(tx beads.Tx) error {
			return tx.Update(closed.ID, opts)
		})
	})
	var written reopenWriteError
	switch {
	case err == nil:
		return session.Info{ID: reopened.ID}, nil
	case !errors.As(err, &written):
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
