package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	apiTemplateTargetPrefix    = "template:"
	apiNamedSessionMetadataKey = session.NamedSessionMetadataKey
	apiNamedSessionIdentityKey = session.NamedSessionIdentityMetadata
	apiNamedSessionModeKey     = session.NamedSessionModeMetadata
)

var (
	errConfiguredNamedSessionConflict = errors.New("configured named session conflict")
	errSessionTargetRejectedByConfig  = errors.New("session target rejected by config")
)

type apiSessionTargetNotFoundError struct {
	identifier       string
	rejectedByConfig bool
}

func (e apiSessionTargetNotFoundError) Error() string {
	return fmt.Sprintf("%v: %q", session.ErrSessionNotFound, e.identifier)
}

func (e apiSessionTargetNotFoundError) Unwrap() error {
	return session.ErrSessionNotFound
}

func (e apiSessionTargetNotFoundError) Is(target error) bool {
	return target == session.ErrSessionNotFound || (e.rejectedByConfig && target == errSessionTargetRejectedByConfig)
}

func apiSessionTargetNotFound(identifier string) error {
	return apiSessionTargetNotFoundError{identifier: identifier}
}

func apiSessionTargetRejectedByConfig(identifier string) error {
	return apiSessionTargetNotFoundError{identifier: identifier, rejectedByConfig: true}
}

type apiSessionResolveOptions struct {
	allowClosed bool
	materialize bool
}

type apiNamedSessionSpec = session.NamedSessionSpec

func apiResolvedProviderFamilyMetadata(resolved *config.ResolvedProvider) string {
	if resolved == nil {
		return ""
	}
	name := strings.TrimSpace(resolved.Name)
	if family := strings.TrimSpace(resolved.BuiltinAncestor); family != "" && family != name {
		return family
	}
	return ""
}

func apiNormalizeSessionTarget(target string) string {
	return session.NormalizeNamedSessionTarget(target)
}

func apiCityName(cfg *config.City, cityPath string) string {
	return config.EffectiveCityName(cfg, filepath.Base(cityPath))
}

func (s *Server) findNamedSessionSpecForTarget(_ beads.Store, target string) (apiNamedSessionSpec, bool, error) {
	cfg := s.state.Config()
	target = apiNormalizeSessionTarget(target)
	if cfg == nil || target == "" {
		return apiNamedSessionSpec{}, false, nil
	}
	cityName := apiCityName(cfg, s.state.CityPath())
	spec, ok, err := session.FindNamedSessionSpecForTarget(cfg, cityName, target, "")
	if err != nil || ok || strings.Contains(target, "/") {
		return spec, ok, err
	}

	rigMatches := map[string]bool{}
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		if ns == nil {
			continue
		}
		name := strings.TrimSpace(ns.Name)
		if name == "" {
			name = strings.TrimSpace(ns.Template)
		}
		if name != target {
			continue
		}
		rigMatches[strings.TrimSpace(ns.Dir)] = true
	}
	switch len(rigMatches) {
	case 0:
		return apiNamedSessionSpec{}, false, nil
	case 1:
		var rigContext string
		for rig := range rigMatches {
			rigContext = rig
		}
		return session.FindNamedSessionSpecForTarget(cfg, cityName, target, rigContext)
	default:
		return apiNamedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
	}
}

func (s *Server) findCanonicalNamedSession(store beads.Store, spec apiNamedSessionSpec) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, nil
	}
	bead, ok, err := session.FindCanonicalConfiguredNamedSessionBead(store, spec)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("looking up canonical named session: %w", err)
	}
	return bead, ok, nil
}

func (s *Server) retireContinuityIneligibleNamedSessionIdentifiers(store beads.Store, spec apiNamedSessionSpec) ([]session.Info, error) {
	if store == nil {
		return nil, nil
	}
	// Typed candidate feed: ExactMetadataSessionCandidatesInfo projects each
	// candidate through the codec ONCE inside the session edge, so this retire
	// lane reads only session.Info fields — no raw bead is cracked here and no
	// b.Metadata key is inlined (the census-honest replacement for the old raw
	// codec projection of SessionNameMetadata per candidate).
	all, err := session.ExactMetadataSessionCandidatesInfo(store, false, map[string]string{
		session.NamedSessionIdentityMetadata: spec.Identity,
	})
	if err != nil {
		return nil, fmt.Errorf("listing named session candidates: %w", err)
	}
	retired := make([]session.Info, 0)
	now := time.Now().UTC()
	for _, info := range all {
		if !continuityIneligibleNamedSession(info, spec.Identity) {
			continue
		}
		if session.LifecycleIdentityReleasedInfo(info) {
			retired = append(retired, info)
			continue
		}
		if sessionName := strings.TrimSpace(info.SessionNameMetadata); sessionName != "" && s.state.SessionProvider() != nil {
			if handle, err := s.workerHandleForSession(store, info.ID); err == nil {
				_ = handle.Kill(context.Background(), s.actor(session.ActorOperator))
			}
		}
		patch := session.CellIntoInfo(info, session.ActorController, session.DispEvent{Kind: session.EvRetire}, now, session.RetireNamedSessionPatch(now, "continuity-ineligible-replacement", spec.Identity))
		patch["alias_history"] = ""
		if err := store.SetMetadataBatch(info.ID, patch); err != nil {
			return nil, fmt.Errorf("retiring continuity-ineligible named session identifiers on %s: %w", info.ID, err)
		}
		retired = append(retired, info)
	}
	return retired, nil
}

// continuityIneligibleNamedSession reports whether info is an open row of the
// named-session identity that can no longer carry the identity's continuity:
// one the identity's next materialization retires, or one already retired.
func continuityIneligibleNamedSession(info session.Info, identity string) bool {
	return !info.Closed && session.IsNamedSessionInfo(info) && session.NamedSessionIdentityInfo(info) == identity && !session.NamedSessionInfoContinuityEligible(info)
}

// reassignContinuityIneligibleNamedSessionState hands what the retired rows
// hold to replacementID once the replacement exists, reading every work store
// live (reassignOpenWorkAssignedToSession).
func (s *Server) reassignContinuityIneligibleNamedSessionState(ctx context.Context, store beads.Store, retired []session.Info, replacementID string) error {
	return s.handOffRetiredNamedSessionState(ctx, store, retired, replacementID, reassignOpenWorkAssignedToSession)
}

// handOffRetiredNamedSessionState moves what each retired row still holds to
// replacementID: its open work in every store that can hold it
// (retiredSessionWorkStores), through moveWork, its waits, and its
// external-message bindings and participants. Every step runs whatever an
// earlier one did, and the failures are joined. A store that cannot be read (a
// rig store that failed to open answers every read with an error until the
// controller reopens its stores) must not keep the binding's graph steps, the
// waits or the bindings on an id no session answers to; what that store holds
// moves on a later materializing resolve of the identity
// (handOffReleasedNamedSessionPredecessors).
func (s *Server) handOffRetiredNamedSessionState(ctx context.Context, store beads.Store, retired []session.Info, replacementID string, moveWork func(store beads.Store, oldID, newID string) error) error {
	if store == nil || strings.TrimSpace(replacementID) == "" || len(retired) == 0 {
		return nil
	}
	now := time.Now().UTC()
	workStores := s.retiredSessionWorkStores(store)
	var errs []error
	for _, info := range retired {
		for _, workStore := range workStores {
			if err := moveWork(workStore, info.ID, replacementID); err != nil {
				errs = append(errs, err)
			}
		}
		if err := session.NewStore(beads.SessionStore{Store: store}).ReassignWaits(info.ID, replacementID); err != nil {
			errs = append(errs, fmt.Errorf("reassign waits from retired session %s to %s: %w", info.ID, replacementID, err))
		}
		if err := extmsg.ReassignSessionBindings(ctx, store, info.ID, replacementID, now); err != nil {
			errs = append(errs, fmt.Errorf("reassign external message bindings from retired session %s to %s: %w", info.ID, replacementID, err))
		}
		if err := extmsg.ReassignSessionParticipants(ctx, store, info.ID, replacementID); err != nil {
			errs = append(errs, fmt.Errorf("reassign external message participants from retired session %s to %s: %w", info.ID, replacementID, err))
		}
	}
	return errors.Join(errs...)
}

// handOffReleasedNamedSessionPredecessors re-runs the hand-off for the
// retired rows of the identity whose canonical session is canonicalID. The
// hand-off that replaced a row runs once, right after the replacement is
// created, and nothing else runs it again: the controller reassigns a retired
// row's work only when it repairs a duplicate continuity-eligible row, and
// named-session work is not pool-routed, so the orphan lane passes it by.
// Without this, work a store could not answer for then, or that a suspended
// rig held, would stay on the retired id for good.
//
// It runs on every materializing resolve that finds the canonical session, so
// its cost is bounded by what the retired rows still hold: one read lists the
// identity's rows (the exact-metadata read the canonical lookup makes), an
// identity with no retired row stops there, the waits and external-message
// reads are cached label reads, and a work store is read live only where a
// cached read shows a retired row still holds work in it
// (reassignHeldOpenWorkAssignedToSession). A failure is logged and does not
// fail the resolve, whose answer does not depend on it; the next materializing
// resolve tries again.
func (s *Server) handOffReleasedNamedSessionPredecessors(ctx context.Context, store beads.Store, spec apiNamedSessionSpec, canonicalID string) {
	retired, err := releasedNamedSessionPredecessors(store, spec.Identity, canonicalID)
	if err == nil {
		err = s.handOffRetiredNamedSessionState(ctx, store, retired, canonicalID, reassignHeldOpenWorkAssignedToSession)
	}
	if err != nil {
		log.Printf("gc api: handing what the retired rows of named session %q hold to %s: %v", spec.Identity, canonicalID, err)
	}
}

// releasedNamedSessionPredecessors lists the identity's retired rows other
// than canonicalID: open rows that lost continuity and released their
// identifiers.
func releasedNamedSessionPredecessors(store beads.Store, identity, canonicalID string) ([]session.Info, error) {
	all, err := session.ExactMetadataSessionCandidatesInfo(store, false, map[string]string{
		session.NamedSessionIdentityMetadata: identity,
	})
	if err != nil {
		return nil, fmt.Errorf("listing named session candidates: %w", err)
	}
	var retired []session.Info
	for _, info := range all {
		if info.ID != canonicalID && continuityIneligibleNamedSession(info, identity) && session.LifecycleIdentityReleasedInfo(info) {
			retired = append(retired, info)
		}
	}
	return retired, nil
}

// retiredSessionWorkStores lists, once each, every store that can hold work
// assigned to a session.
//
// On a city that relocates nothing it is the sessions store alone — the one
// store this sweep always read, exactly as before, rigs or no rigs.
//
// On a split city the sessions store is the class binding, which holds graph
// steps but no task: listing work there alone moved the retired session's graph
// steps to its successor and left every task in the city and rig work stores
// assigned to an id no session answers to (write-audit B7). Named-session work
// is not pool-routed, so the orphan lane never rescues it. So a split city
// sweeps the city work store, the store of every rig not suspended by effective
// state, the graph store and the sessions store. A rig suspended by effective
// state (cacheColdRigNames) is left untouched, as status and the active-bead
// lookup leave it: it has no cache, and any read of its store restarts its
// retired proxy and Dolt. A store it shares with a rig that is not suspended,
// as in legacy shared-file mode, is swept through that rig. Its work moves on
// the first materializing resolve of the identity after it resumes
// (handOffReleasedNamedSessionPredecessors).
//
// residency:allow — a write sweep over every store that can hold work assigned
// to one session id, not a residency answer: each store is listed by assignee,
// so a store that holds nothing for the id costs one query and changes nothing.
func (s *Server) retiredSessionWorkStores(sessions beads.Store) []beads.Store {
	if relocatedGraphStore(s.state) == nil {
		return []beads.Store{sessions} // residency:allow — a city that relocates nothing sweeps the one sessions store it always swept
	}
	var out []beads.Store
	add := func(store beads.Store) {
		if store == nil || slices.Contains(out, store) {
			return
		}
		out = append(out, store)
	}
	add(s.state.CityBeadStore())
	cold := s.loadCacheColdRigs()
	stores := s.state.BeadStores() // residency:allow — the reassign sweep above: every store that can hold the retired session's work
	// Cold names drop before add dedupes the stores, so a store a cold rig
	// shares stays reachable through a rig that is not.
	for _, rigName := range slices.Sorted(maps.Keys(stores)) {
		if !cold[rigName] {
			add(stores[rigName])
		}
	}
	add(s.state.GraphBeadStore().Store)
	add(sessions)
	return out
}

// retiredSessionWorkStatuses are the statuses of the work a retired session
// hands to its successor.
var retiredSessionWorkStatuses = []string{"open", "in_progress"}

// reassignHeldOpenWorkAssignedToSession is reassignOpenWorkAssignedToSession
// for a store that most likely holds nothing for oldID any more. It asks
// first through the cache (a CachingStore, as every store the controller
// serves is, answers from memory) and reads live and writes only when that
// read shows work still assigned to oldID that the live read would move. Work
// a cache has not seen yet is missed this time and found by a later pass.
//
// Parked work (bd's blocked, deferred and the rest) reads as "open" through a
// cache but keeps bd's status in SourceStatus, and the live "open" read, bd's
// raw open, leaves it out (nativeListPushesRawOpen), so it stays assigned. It
// does not count as held: a retired row holding only parked work would
// otherwise pay the live read on every pass and move nothing.
func reassignHeldOpenWorkAssignedToSession(store beads.Store, oldID, newID string) error {
	if store == nil || strings.TrimSpace(oldID) == "" || strings.TrimSpace(newID) == "" {
		return nil
	}
	for _, status := range retiredSessionWorkStatuses {
		work, err := store.List(beads.ListQuery{Assignee: oldID, Status: status, TierMode: beads.TierBoth})
		if err != nil {
			return fmt.Errorf("looking up work held by retired session %s: %w", oldID, err)
		}
		held := func(item beads.Bead) bool {
			parked := item.SourceStatus != "" && item.SourceStatus != status
			return !parked && !session.IsSessionBeadOrRepairable(item)
		}
		if slices.ContainsFunc(work, held) {
			return reassignOpenWorkAssignedToSession(store, oldID, newID)
		}
	}
	return nil
}

func reassignOpenWorkAssignedToSession(store beads.Store, oldID, newID string) error {
	if store == nil || strings.TrimSpace(oldID) == "" || strings.TrimSpace(newID) == "" {
		return nil
	}
	for _, status := range retiredSessionWorkStatuses {
		work, err := store.List(beads.ListQuery{Assignee: oldID, Status: status, Live: true, TierMode: beads.TierBoth})
		if err != nil {
			return fmt.Errorf("listing work assigned to retired session %s: %w", oldID, err)
		}
		for _, item := range work {
			if session.IsSessionBeadOrRepairable(item) {
				continue
			}
			if err := store.Update(item.ID, beads.UpdateOpts{Assignee: &newID}); err != nil {
				return fmt.Errorf("reassign work %s from retired session %s to %s: %w", item.ID, oldID, newID, err)
			}
		}
	}
	return nil
}

func (s *Server) resolveConfiguredNamedSessionIDWithContext(ctx context.Context, store beads.Store, identifier string, opts apiSessionResolveOptions) (string, bool, error) {
	spec, ok, err := s.findNamedSessionSpecForTarget(store, identifier)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	lookup, err := session.LookupConfiguredNamedSession(store, spec)
	if err != nil {
		return "", true, fmt.Errorf("looking up configured named session: %w", err)
	}
	if lookup.HasCanonical {
		if opts.materialize {
			s.handOffReleasedNamedSessionPredecessors(ctx, store, spec, lookup.Canonical.ID)
		}
		return lookup.Canonical.ID, true, nil
	}
	if lookup.HasConflict {
		return "", true, fmt.Errorf("%w: %q conflicts with configured named session %q via live bead %s", errConfiguredNamedSessionConflict, identifier, spec.Identity, lookup.Conflict.ID)
	}

	if !opts.materialize {
		// The identifier maps to a configured named session with no
		// canonical bead. When it names the reserved identity directly
		// (configured identity or its runtime session name), report
		// matched=true so non-materializing callers short-circuit to
		// not-found instead of falling through to ordinary live-session
		// matching, where a rogue session whose session_name, alias, or
		// path-alias title equals the reserved name could hijack the
		// target (ga-4of1nc). Bare-leaf convenience tokens keep falling
		// through so ordinary sessions can still own those aliases.
		return "", namedSessionTargetIsReservedIdentity(spec, identifier), fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	id, err := s.materializeNamedSessionWithContext(ctx, store, spec)
	return id, true, err
}

// namedSessionTargetIsReservedIdentity reports whether identifier names a
// configured named-session identity directly — by its configured identity or
// by its runtime session name — as opposed to a bare-leaf convenience token
// that merely resolves to the spec.
func namedSessionTargetIsReservedIdentity(spec apiNamedSessionSpec, identifier string) bool {
	target := apiNormalizeSessionTarget(identifier)
	return target != "" && (target == spec.Identity || target == spec.SessionName)
}

func parseAPITemplateTarget(identifier string) (string, bool) {
	identifier = strings.TrimSpace(identifier)
	if !strings.HasPrefix(identifier, apiTemplateTargetPrefix) {
		return "", false
	}
	name := apiNormalizeSessionTarget(strings.TrimSpace(strings.TrimPrefix(identifier, apiTemplateTargetPrefix)))
	if name == "" {
		return "", false
	}
	return name, true
}

func (s *Server) materializeNamedSessionWithContext(ctx context.Context, store beads.Store, spec apiNamedSessionSpec) (string, error) {
	if bead, ok, err := s.findCanonicalNamedSession(store, spec); err != nil {
		return "", err
	} else if ok {
		s.handOffReleasedNamedSessionPredecessors(ctx, store, spec, bead.ID)
		return bead.ID, nil
	}
	retired, err := s.retireContinuityIneligibleNamedSessionIdentifiers(store, spec)
	if err != nil {
		return "", err
	}

	resolved, _, transport, qualifiedTemplate, err := s.resolveSessionTemplateForCreate(spec.Agent.QualifiedName())
	if err != nil {
		return "", err
	}
	transport, err = validateSessionTransport(resolved, transport, s.state.SessionProvider())
	if err != nil {
		return "", err
	}
	var workDir string
	workDirQualifiedName := workdirutil.SessionQualifiedName(s.state.CityPath(), *spec.Agent, s.state.Config().Rigs, spec.Identity, "")
	workDir, err = s.resolveSessionWorkDir(*spec.Agent, workDirQualifiedName)
	if err != nil {
		return "", err
	}
	launchCommand, err := config.BuildProviderLaunchCommand(s.state.CityPath(), resolved, nil, transport)
	if err != nil {
		return "", err
	}
	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}
	extraMeta := map[string]string{
		apiNamedSessionMetadataKey: "true",
		apiNamedSessionIdentityKey: spec.Identity,
		apiNamedSessionModeKey:     spec.Mode,
		"session_origin":           "named",
	}
	if family := apiResolvedProviderFamilyMetadata(resolved); family != "" {
		extraMeta["provider_kind"] = family
	}
	if resolved.BuiltinAncestor != "" && resolved.BuiltinAncestor != resolved.Name {
		extraMeta["builtin_ancestor"] = resolved.BuiltinAncestor
	}
	mcpServers, err := s.sessionMCPServers(qualifiedTemplate, resolved.Name, spec.Identity, workDir, transport, "", nil)
	if err != nil {
		return "", err
	}
	if transport == "acp" {
		extraMeta, err = session.WithStoredMCPMetadata(extraMeta, spec.Identity, mcpServers)
		if err != nil {
			return "", err
		}
	}
	sessionEnv := cityAnchoredSessionEnv(s.state.CityPath(), configuredWorkspaceSessionEnv(s.state.Config()), resolved.Env)
	hints := sessionCreateHints(resolved, sessionEnv, mcpServers)
	// Route the named-session create through the worker.Handle boundary
	// (worker-boundary migration) rather than calling session.Manager directly.
	// SessionSpecForResolvedRuntime maps this config 1:1 onto the same
	// CreateAliasedNamedWithTransportAndMetadata call createStartedLocked makes
	// (alias, name, template, title, command, workdir, provider, transport, env,
	// resume, hints, metadata), so the created session is identical; the handle
	// additionally emits the uniform worker create-operation event.
	resolvedCfg := worker.ResolvedSessionConfig{
		Alias:        spec.Identity,
		ExplicitName: spec.SessionName,
		Template:     qualifiedTemplate,
		Title:        spec.Identity,
		Transport:    transport,
		Metadata:     extraMeta,
		Runtime: worker.ResolvedRuntime{
			// Backfill an empty command with the provider name, matching the
			// sibling boundary consumer (resolvedSessionConfigForProvider) and
			// cmd/gc/worker_handle.go. A command-less custom provider otherwise
			// hard-fails NormalizeResolvedRuntime ("command is required") where
			// the old direct path minted a (doomed) session — the backfill keeps
			// the create succeeding and converges this path with the adhoc one.
			Command:    firstNonEmptyString(launchCommand.Command, resolved.Name),
			WorkDir:    workDir,
			Provider:   resolved.Name,
			SessionEnv: sessionEnv,
			Resume:     resume,
			Hints:      hints,
		},
	}
	var info session.Info
	err = session.WithCitySessionIdentifierLocks(s.state.CityPath(), []string{spec.Identity, spec.SessionName}, func() error {
		if err := session.EnsureAliasAvailableWithConfigForOwner(store, s.state.Config(), spec.Identity, "", spec.Identity); err != nil {
			return err
		}
		if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, s.state.Config(), spec.SessionName, "", spec.Identity); err != nil {
			return err
		}
		handle, herr := s.newResolvedWorkerSessionHandle(store, resolvedCfg)
		if herr != nil {
			return herr
		}
		var createErr error
		info, createErr = handle.Create(ctx, worker.CreateModeStarted)
		return createErr
	})
	if err == nil {
		s.handOffToReplacement(ctx, store, spec, retired, info.ID)
		// The retired rows just handed their work, waits and bindings to
		// the replacement, all of it but what a failed step left; their keys
		// ride along so a keyed reconciler revisits them too. Legacy: one
		// poke.
		keys := []reconcilekey.Key{reconcilekey.Session(info.ID)}
		for _, r := range retired {
			if strings.TrimSpace(r.ID) != "" && r.ID != info.ID {
				keys = append(keys, reconcilekey.Session(r.ID))
			}
		}
		s.state.Enqueue(keys...)
		return info.ID, nil
	}
	if bead, ok, lookupErr := s.findCanonicalNamedSession(store, spec); lookupErr == nil && ok {
		s.handOffToReplacement(ctx, store, spec, retired, bead.ID)
		return bead.ID, nil
	}
	return "", err
}

// handOffToReplacement hands what the retired rows hold to replacementID, a
// session that exists. The resolve answers the replacement whether or not
// every step lands: by now it is created and started, and failing the request
// would report that side effect as a failure (a wake answering 500, a message
// dropped as resolve_failed) to a caller whose next request finds it
// canonical. A step that failed is logged, as the canonical branch's re-run
// logs it, and what it left on a retired row moves on a later materializing
// resolve of the identity (handOffReleasedNamedSessionPredecessors).
func (s *Server) handOffToReplacement(ctx context.Context, store beads.Store, spec apiNamedSessionSpec, retired []session.Info, replacementID string) {
	if err := s.reassignContinuityIneligibleNamedSessionState(ctx, store, retired, replacementID); err != nil {
		log.Printf("gc api: handing what the retired rows of named session %q hold to its replacement %s: %v", spec.Identity, replacementID, err)
	}
}

func (s *Server) materializeNamedSession(store beads.Store, spec apiNamedSessionSpec) (string, error) {
	return s.materializeNamedSessionWithContext(context.Background(), store, spec)
}

// resolveLiveSessionByPathAlias matches identifier against the Title of an
// active pool-session bead. Pool sessions surface their stable path-alias
// under Title (the same string `gc session list` shows under TARGET /
// TITLE) while their session_name is a synthetic internal id (s-gc-NNN),
// so they are invisible to session.ResolveSessionID's session_name/alias
// indexes.
//
// State filter accepts {active, awake, none}. Empty state (StateNone)
// is treated as active for legacy/upgrade beads — matches the convention
// in internal/session/manager.go:741,813 where reconciler paths normalize
// `current == StateNone` to StateActive. Excluded states intentionally
// fall through to apiSessionTargetNotFound:
//   - asleep: not running, can't receive messages.
//   - draining: on its way out, shouldn't get new external messages.
//   - creating: runtime still booting; sendBackgroundMessageToSession
//     would deliver against an incomplete provider, worse than not-found.
//     Once the reconciler flips state=active, subsequent inbounds resolve.
//
// Configured named-session beads are skipped (session.IsNamedSessionInfo) so
// session.ResolveSessionID still owns those identifiers via its
// orphan-rejection path. This step is wired AFTER session.ResolveSessionID
// in the resolver chain so session_name/alias matches always win when both
// could apply.
//
// Tiebreaker on duplicate active-pool Titles (rare misconfiguration):
// most-recently-created bead wins; ties on CreatedAt resolve to the first
// match in store iteration order.
func resolveLiveSessionByPathAlias(store beads.Store, identifier string) (string, bool, error) {
	if store == nil {
		return "", false, nil
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", false, nil
	}
	all, err := session.NewStore(beads.SessionStore{Store: store}).ListAll(session.ListAllOptions{})
	if err != nil {
		return "", false, fmt.Errorf("resolveLiveSessionByPathAlias: listing sessions: %w", err)
	}
	var best session.Info
	found := false
	for _, info := range all {
		// ListAll already filters via IsSessionBeadOrRepairable.
		if session.IsNamedSessionInfo(info) {
			continue
		}
		if strings.TrimSpace(info.Title) != identifier {
			continue
		}
		// MetadataState is the RAW state mirror; Info.State is normalizeInfoState-
		// folded (awake->active), which would change this predicate.
		state := session.State(info.MetadataState)
		if state != session.StateActive && state != session.StateAwake && state != session.StateNone {
			continue
		}
		if !found || info.CreatedAt.After(best.CreatedAt) {
			best = info
			found = true
		}
	}
	if !found {
		return "", false, nil
	}
	return best.ID, true, nil
}

// resolveSessionTargetIDWithContext drives session.DecideSessionTarget: the
// classifier owns the precedence ladder, this adapter performs the lookup
// each gather action names and keeps the matched ID or error for the step a
// terminal decision cites. Lookups run exactly when the old inline ladder
// ran them — an exact-ID hit never reaches the live scan.
func (s *Server) resolveSessionTargetIDWithContext(ctx context.Context, store beads.Store, identifier string, opts apiSessionResolveOptions) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	_, templateForm := parseAPITemplateTarget(identifier)
	facts := session.TargetFacts{
		TemplateForm: templateForm,
		AllowClosed:  opts.allowClosed,
	}
	selected := map[session.TargetStep]string{}
	failed := map[session.TargetStep]error{}
	// The classifier gathers each of the ladder's six facts at most once, so
	// classification terminates within seven decisions. The bound (matching
	// gatherSequence in the classifier tests) turns a classifier regression
	// that re-requests a gathered fact into an internal error instead of a
	// spinning request goroutine.
	for range 16 {
		dec := session.DecideSessionTarget(facts)
		if dec.Action == session.TargetDone {
			switch dec.Result {
			case session.TargetSelected:
				return selected[dec.Source], nil
			case session.TargetRejectedByConfig:
				return "", apiSessionTargetRejectedByConfig(identifier)
			case session.TargetError:
				return "", failed[dec.Source]
			default:
				return "", apiSessionTargetNotFound(identifier)
			}
		}
		switch dec.Gather {
		case session.TargetStepExactID:
			id, err := session.ResolveSessionIDByExactID(store, identifier)
			facts.ExactID = lookupFact(err)
			selected[dec.Gather], failed[dec.Gather] = id, err
		case session.TargetStepConfiguredName:
			id, matched, err := s.resolveConfiguredNamedSessionIDWithContext(ctx, store, identifier, opts)
			switch {
			case err == nil:
				facts.ConfiguredName = session.NamedLookupMatch
				selected[dec.Gather] = id
			case matched || !errors.Is(err, session.ErrSessionNotFound):
				facts.ConfiguredName = session.NamedLookupTerminalError
				failed[dec.Gather] = err
			default:
				facts.ConfiguredName = session.NamedLookupNoMatch
			}
		case session.TargetStepLive:
			id, err := session.ResolveSessionID(store, identifier)
			facts.Live = lookupFact(err)
			selected[dec.Gather], failed[dec.Gather] = id, err
			if err == nil {
				facts.LiveConfigOrphan = s.liveSessionMatchIsConfigOrphan(store, id)
			}
		case session.TargetStepPathAlias:
			id, ok, err := resolveLiveSessionByPathAlias(store, identifier)
			switch {
			case err != nil:
				facts.PathAlias = session.LookupError
				failed[dec.Gather] = err
			case ok:
				facts.PathAlias = session.LookupMatch
				selected[dec.Gather] = id
			default:
				facts.PathAlias = session.LookupNoMatch
			}
		case session.TargetStepClosedNamedSpec:
			_, ok, err := s.findNamedSessionSpecForTarget(store, identifier)
			switch {
			case err != nil:
				facts.ClosedNamedSpec = session.LookupError
				failed[dec.Gather] = err
			case ok:
				facts.ClosedNamedSpec = session.LookupMatch
			default:
				facts.ClosedNamedSpec = session.LookupNoMatch
			}
		case session.TargetStepClosed:
			id, err := session.ResolveSessionIDAllowClosed(store, identifier)
			facts.Closed = lookupFact(err)
			selected[dec.Gather], failed[dec.Gather] = id, err
		default:
			return "", fmt.Errorf("session target classifier requested unknown step %v", dec.Gather)
		}
	}
	return "", fmt.Errorf("session target classifier did not terminate for %q", identifier)
}

// lookupFact maps a Resolve* error to the classifier's tri-state lookup
// fact: nil is a match, ErrSessionNotFound is a miss, anything else is a
// terminal lookup error.
func lookupFact(err error) session.TargetLookupFact {
	switch {
	case err == nil:
		return session.LookupMatch
	case errors.Is(err, session.ErrSessionNotFound):
		return session.LookupNoMatch
	default:
		return session.LookupError
	}
}

// liveSessionMatchIsConfigOrphan reports whether a live-resolved bead is a
// named-session bead whose configured identity is absent from current config.
// Lookup failures fail open: the match stands (any error → false). The read
// routes through the session front door and the session.Info twins
// (IsNamedSessionInfo / NamedSessionIdentityInfo), so no raw bead is cracked here
// — the Info projections mirror the bead accessors exactly.
//
// Byte-identical to the old raw store.Get for every real input: absent id →
// false; present non-session bead → false; present session bead (named or not) →
// the same verdict via the mirrored projections. ONE design-prescribed direction
// change, and only under double corruption: a bead carrying
// configured_named_session="true" that has lost BOTH its session type AND its
// gc:session label was previously classified an orphan (match rejected); it now
// fails the front door's IsSessionBeadOrRepairable check → ErrSessionNotFound →
// false (the match stands). A named bead that is merely type-lost stays
// repairable and reaches the identity check unchanged.
func (s *Server) liveSessionMatchIsConfigOrphan(store beads.Store, id string) bool {
	cfg := s.state.Config()
	if cfg == nil {
		return false
	}
	info, err := session.NewStore(beads.SessionStore{Store: store}).Get(id)
	if err != nil || !session.IsNamedSessionInfo(info) {
		return false
	}
	identity := session.NamedSessionIdentityInfo(info)
	return identity != "" && config.FindNamedSession(cfg, identity) == nil
}

func (s *Server) resolveSessionTargetID(store beads.Store, identifier string, opts apiSessionResolveOptions) (string, error) {
	return s.resolveSessionTargetIDWithContext(context.Background(), store, identifier, opts)
}

func (s *Server) resolveSessionIDWithConfig(store beads.Store, identifier string) (string, error) {
	return s.resolveSessionTargetID(store, identifier, apiSessionResolveOptions{})
}

func (s *Server) resolveSessionIDAllowClosedWithConfig(store beads.Store, identifier string) (string, error) {
	return s.resolveSessionTargetID(store, identifier, apiSessionResolveOptions{allowClosed: true})
}

// sessionTargetDeliverable reports whether a message/submit target is
// deliverable: it resolves to an existing session without materializing, or
// names a configured named session the materializing async path can wake.
// The async command handlers (POST /session/{id}/messages, /submit) used to
// accept ANY identifier with 202 and only discover resolve_failed inside the
// post-accept goroutine, surfacing it solely as an event — callers treating
// 202 as delivery proof black-holed messages to typo'd/drifted session names
// (2026-07-18: three drifted Slack company-room bindings dropped cross-city
// wakes for days). This gate restores the declared-404 contract for targets
// that can never deliver, while keeping the accept-then-work model for slow
// paths (cold named-session wakes).
//
// It returns the resolved session id, or "" for a configured named session
// that has no row yet (the async path materializes it).
func (s *Server) sessionTargetDeliverable(ctx context.Context, store beads.Store, identifier string) (string, error) {
	if id, err := s.resolveSessionTargetIDWithContext(ctx, store, identifier, apiSessionResolveOptions{}); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if _, ok, specErr := s.findNamedSessionSpecForTarget(store, identifier); specErr == nil && ok {
		return "", nil
	}
	return "", apiSessionTargetNotFound(identifier)
}

// rejectPendingInteraction refuses a text delivery synchronously while the
// target session is waiting on a pending interaction such as a tool
// permission prompt (#2892). The delivery itself runs asynchronously after
// the 202, so without this check the caller would be told "accepted" and the
// refusal would surface only as a request.failed event -- or, where the
// provider could not see the prompt, the text's Enter would answer it. A
// configured named session that has not been materialized yet has nothing
// pending, and neither has a session that is not running: the worker handle
// does not look at its runtime. A look that cannot reach the runtime refuses
// nothing; the delivery looks again under the session lock.
func (s *Server) rejectPendingInteraction(ctx context.Context, store beads.Store, identifier string) error {
	id, err := s.resolveSessionTargetIDWithContext(ctx, store, identifier, apiSessionResolveOptions{})
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil
		}
		return humaResolveError(err)
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return humaSessionManagerError(err)
	}
	pending, _, err := handle.PendingStatus(ctx)
	if errors.Is(err, runtime.ErrRuntimeUnavailable) {
		log.Printf("gc api: accepting text for %s without a look for a pending interaction: %v", id, err)
		return nil
	}
	if err != nil {
		return humaSessionManagerError(err)
	}
	if pending != nil {
		interaction := runtimePendingInteraction(pending)
		return humaSessionManagerError(session.PendingInteractionError(&interaction))
	}
	return nil
}

func (s *Server) resolveSessionIDMaterializingNamed(store beads.Store, identifier string) (string, error) {
	return s.resolveSessionTargetID(store, identifier, apiSessionResolveOptions{materialize: true})
}

func (s *Server) resolveSessionIDMaterializingNamedWithContext(ctx context.Context, store beads.Store, identifier string) (string, error) {
	return s.resolveSessionTargetIDWithContext(ctx, store, identifier, apiSessionResolveOptions{materialize: true})
}

// messageOutcome is a delivered or queued message and, for one the resume
// policy queued, whether the controller will start the session for it.
type messageOutcome struct {
	session.SubmitOutcome
	willStart          *bool
	willNotStartReason string
}

// submitMessageToSession submits message to session id. resume is the
// request's `resume: true`: only then may the message start or resume the
// session, here in the controller's process (CONTRACT v5.9 D8, owner ruling
// 2026-10-08). Otherwise a session that is not running gets the message
// queued and the start is left to the controller (deferToController).
func (s *Server) submitMessageToSession(ctx context.Context, store beads.Store, id, message string, intent session.SubmitIntent, resume bool) (messageOutcome, error) {
	return s.submitMessageToSessionFor(ctx, store, id, message, intent, resume, messageOpDelivery{})
}

// messageOpDelivery is how a recorded message op's delivery is keyed: op is
// its id, key the runtime delivery key every execution uses
// (legacyop.Admission.DeliveryKey), and redelivery whether this execution
// re-runs an adopted op whose first execution may have delivered. The zero
// value is an unrecorded request: unkeyed, with no lookup.
type messageOpDelivery struct {
	op, key    string
	redelivery bool
}

// opDeliveryOf is adm's delivery: zero unless adm is a recorded Execute.
func opDeliveryOf(adm *legacyop.Admission) messageOpDelivery {
	key := adm.DeliveryKey()
	if key == "" {
		return messageOpDelivery{}
	}
	return messageOpDelivery{op: adm.Op.ID, key: key, redelivery: adm.Adopted}
}

// submitMessageToSessionFor is submitMessageToSession for a recorded op: the
// request carries od's key and redelivery. A result the runtime replayed (an
// adopted op whose first execution delivered) is logged and returned as a
// delivered, unqueued outcome: nothing was sent again.
func (s *Server) submitMessageToSessionFor(ctx context.Context, store beads.Store, id, message string, intent session.SubmitIntent, resume bool, od messageOpDelivery) (messageOutcome, error) {
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return messageOutcome{}, err
	}
	by := s.actor(session.ActorBackground)
	if resume {
		by = s.actor(session.ActorOperator)
	}
	result, err := handle.Message(ctx, by, worker.MessageRequest{
		Text:        message,
		Delivery:    workerDeliveryIntent(intent),
		DeliveryKey: od.key,
		Redelivery:  od.redelivery,
	})
	if err != nil {
		return messageOutcome{}, err
	}
	if result.Replayed {
		log.Printf("gc api: op %s: session %s's runtime already holds the message an earlier execution delivered; nothing sent again", od.op, id)
		return messageOutcome{}, nil
	}
	out := messageOutcome{SubmitOutcome: session.SubmitOutcome{Queued: result.Queued, Deferred: result.Deferred, Interrupt: result.Interrupt}}
	if result.Deferred {
		out.willStart, out.willNotStartReason = s.deferToController(store, id)
	}
	return out, nil
}

// deferToController asks the controller to start a session a message was
// queued for (D8 rule 1): it records the wake on an asleep or drained row
// nothing holds and pokes; a held row gets no wake and no poke. It reports
// whether the controller will start the session, nil when it cannot tell (a
// lost wake CAS, a row that is not dormant, a pool seat that starts only for
// demand) and, if not, why, with the remedy.
func (s *Server) deferToController(store beads.Store, id string) (*bool, string) {
	no := false
	outcome, err := session.NewStore(beads.SessionStore{Store: store}).RequestWakeUnlessHeld(id, false, time.Now().UTC())
	d := deferralFor(outcome, err)
	switch d {
	case deferFailed:
		log.Printf("gc api: recording the wake for a message queued to %s: %v", id, err)
		return nil, ""
	case deferHeld:
		return &no, "an operator holds the session; resend with resume: true to resume it"
	}
	s.state.Enqueue(reconcilekey.Session(id))
	if d == deferUnknown {
		return nil, ""
	}
	certain := true
	if refuser, ok := s.state.(WakeStartRefuser); ok {
		if info, err := session.NewStore(beads.SessionStore{Store: store}).Get(id); err == nil {
			var why string
			if why, certain = refuser.WakeStartRefusal(info); why != "" {
				return &no, why + "; resend with resume: true to start it now"
			}
		}
	}
	if !certain {
		return nil, ""
	}
	yes := true
	return &yes, ""
}

// deferral is what deferToController does with a wake request's answer.
type deferral uint8

const (
	deferFailed  deferral = iota // the wake was not recorded: log, no poke, unknown
	deferHeld                    // held: no poke, will not start
	deferUnknown                 // poke; whether it starts is unknown
	deferAsk                     // wake recorded: poke, and ask the refuser
)

// deferralFor maps RequestWakeUnlessHeld's answer. A lost CAS still pokes
// (another writer may have recorded the wake); a row that is not dormant (an
// active row whose runtime died, a create in flight) is the controller's to
// handle, and neither claims a start.
func deferralFor(outcome session.WakeRequestOutcome, err error) deferral {
	switch {
	case errors.Is(err, session.ErrWakeRequestContended):
		return deferUnknown
	case err != nil:
		return deferFailed
	case outcome == session.WakeHeld:
		return deferHeld
	case outcome == session.WakeRecorded:
		return deferAsk
	}
	return deferUnknown
}

// sendBackgroundMessageToSession preserves the default provider nudge semantics
// for system-driven messages that should respect wait-idle behavior when the
// runtime supports it. It never starts a session in the controller's process:
// one that is not running gets the nudge queued and the start left to the
// controller (CONTRACT v5.9 D8 7(e)). When the city's lane host executes
// message, it proposes a lane notice instead (proposeExtmsgNotice).
func (s *Server) sendBackgroundMessageToSession(ctx context.Context, store beads.Store, id, message string) error {
	if s.laneExecutes(opjournal.KindMessage) {
		return s.proposeExtmsgNotice(id, message)
	}
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return err
	}
	result, err := handle.Nudge(ctx, s.actor(session.ActorBackground), worker.NudgeRequest{Text: message})
	if err == nil && result.Undelivered == worker.NudgeQueued {
		s.deferToController(store, id)
	}
	return err
}

// sendUserMessageToSession keeps POST /messages as a compatibility alias for
// the semantic default submit path.
func (s *Server) sendUserMessageToSession(ctx context.Context, store beads.Store, id, message string, resume bool) (messageOutcome, error) {
	return s.submitMessageToSession(ctx, store, id, message, session.SubmitIntentDefault, resume)
}

func (s *Server) workerHandleForSession(store beads.Store, id string) (worker.Handle, error) {
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	return factory.SessionByID(id)
}

func (s *Server) workerHandleForSessionTarget(store beads.Store, target string) (worker.Handle, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, session.ErrSessionNotFound
	}
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	if store != nil {
		if id, err := s.resolveSessionIDWithConfig(store, target); err == nil {
			return factory.SessionByID(id)
		} else if !errors.Is(err, session.ErrSessionNotFound) || errors.Is(err, errSessionTargetRejectedByConfig) {
			return nil, err
		}
	}
	return factory.HandleForTarget(target, nil)
}

func (s *Server) newResolvedWorkerSessionHandle(store beads.Store, cfg worker.ResolvedSessionConfig) (worker.Handle, error) {
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	return factory.SessionForResolvedRuntime(cfg)
}

func workerDeliveryIntent(intent session.SubmitIntent) worker.DeliveryIntent {
	switch intent {
	case session.SubmitIntentFollowUp:
		return worker.DeliveryIntentFollowUp
	case session.SubmitIntentInterruptNow:
		return worker.DeliveryIntentInterruptNow
	default:
		return worker.DeliveryIntentDefault
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
