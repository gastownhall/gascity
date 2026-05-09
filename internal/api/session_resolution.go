package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
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

func apiIsNamedSessionBead(b beads.Bead) bool {
	return session.IsNamedSessionBead(b)
}

func apiNamedSessionIdentity(b beads.Bead) string {
	return session.NamedSessionIdentity(b)
}

func apiNamedSessionContinuityEligible(b beads.Bead) bool {
	return session.NamedSessionContinuityEligible(b)
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

func (s *Server) retireContinuityIneligibleNamedSessionIdentifiers(store beads.Store, spec apiNamedSessionSpec) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	all, err := session.ExactMetadataSessionCandidates(store, false, map[string]string{
		session.NamedSessionIdentityMetadata: spec.Identity,
	})
	if err != nil {
		return nil, fmt.Errorf("listing named session candidates: %w", err)
	}
	retired := make([]beads.Bead, 0)
	now := time.Now().UTC()
	for _, b := range all {
		if b.Status == "closed" || !apiIsNamedSessionBead(b) || apiNamedSessionIdentity(b) != spec.Identity || apiNamedSessionContinuityEligible(b) {
			continue
		}
		if session.LifecycleIdentityReleased(b.Status, b.Metadata) {
			retired = append(retired, b)
			continue
		}
		if sessionName := strings.TrimSpace(b.Metadata["session_name"]); sessionName != "" && s.state.SessionProvider() != nil {
			if handle, err := s.workerHandleForSession(store, b.ID); err == nil {
				_ = handle.Kill(context.Background())
			}
		}
		patch := session.RetireNamedSessionPatch(now, "continuity-ineligible-replacement", spec.Identity)
		patch["alias_history"] = ""
		if err := store.SetMetadataBatch(b.ID, patch); err != nil {
			return nil, fmt.Errorf("retiring continuity-ineligible named session identifiers on %s: %w", b.ID, err)
		}
		retired = append(retired, b)
	}
	return retired, nil
}

func (s *Server) reassignContinuityIneligibleNamedSessionState(ctx context.Context, store beads.Store, retired []beads.Bead, replacementID string) error {
	if store == nil || strings.TrimSpace(replacementID) == "" {
		return nil
	}
	now := time.Now().UTC()
	for _, b := range retired {
		if err := reassignOpenWorkAssignedToSession(store, b.ID, replacementID); err != nil {
			return err
		}
		if err := session.ReassignWaits(store, b.ID, replacementID); err != nil {
			return fmt.Errorf("reassign waits from retired session %s to %s: %w", b.ID, replacementID, err)
		}
		if err := extmsg.ReassignSessionBindings(ctx, store, b.ID, replacementID, now); err != nil {
			return fmt.Errorf("reassign external message bindings from retired session %s to %s: %w", b.ID, replacementID, err)
		}
	}
	return nil
}

func reassignOpenWorkAssignedToSession(store beads.Store, oldID, newID string) error {
	if store == nil || strings.TrimSpace(oldID) == "" || strings.TrimSpace(newID) == "" {
		return nil
	}
	for _, status := range []string{"open", "in_progress"} {
		work, err := store.List(beads.ListQuery{Assignee: oldID, Status: status, Live: true})
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
		return lookup.Canonical.ID, true, nil
	}
	if lookup.HasConflict {
		return "", true, fmt.Errorf("%w: %q conflicts with configured named session %q via live bead %s", errConfiguredNamedSessionConflict, identifier, spec.Identity, lookup.Conflict.ID)
	}

	if !opts.materialize {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	id, err := s.materializeNamedSessionWithContext(ctx, store, spec)
	return id, true, err
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
	mgr := s.sessionManager(store)
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
	mcpServers, err := s.sessionMCPServers(qualifiedTemplate, resolved.Name, spec.Identity, workDir, transport, "")
	if err != nil {
		return "", err
	}
	if transport == "acp" {
		extraMeta, err = session.WithStoredMCPMetadata(extraMeta, spec.Identity, mcpServers)
		if err != nil {
			return "", err
		}
	}
	hints := sessionCreateHints(resolved, mcpServers)
	var info session.Info
	err = session.WithCitySessionIdentifierLocks(s.state.CityPath(), []string{spec.Identity, spec.SessionName}, func() error {
		if err := session.EnsureAliasAvailableWithConfigForOwner(store, s.state.Config(), spec.Identity, "", spec.Identity); err != nil {
			return err
		}
		if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, s.state.Config(), spec.SessionName, "", spec.Identity); err != nil {
			return err
		}
		var createErr error
		info, createErr = mgr.CreateAliasedNamedWithTransportAndMetadata(
			ctx,
			spec.Identity,
			spec.SessionName,
			qualifiedTemplate,
			spec.Identity,
			launchCommand.Command,
			workDir,
			resolved.Name,
			transport,
			resolved.Env,
			resume,
			hints,
			extraMeta,
		)
		return createErr
	})
	if err == nil {
		if err := s.reassignContinuityIneligibleNamedSessionState(ctx, store, retired, info.ID); err != nil {
			return "", err
		}
		s.state.Poke()
		return info.ID, nil
	}
	if bead, ok, lookupErr := s.findCanonicalNamedSession(store, spec); lookupErr == nil && ok {
		if err := s.reassignContinuityIneligibleNamedSessionState(ctx, store, retired, bead.ID); err != nil {
			return "", err
		}
		return bead.ID, nil
	}
	return "", err
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
// Configured named-session beads are skipped (apiIsNamedSessionBead) so
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
	all, err := store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		return "", false, fmt.Errorf("resolveLiveSessionByPathAlias: listing sessions: %w", err)
	}
	var best beads.Bead
	found := false
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		if apiIsNamedSessionBead(b) {
			continue
		}
		if strings.TrimSpace(b.Title) != identifier {
			continue
		}
		state := session.State(b.Metadata["state"])
		if state != session.StateActive && state != session.StateAwake && state != session.StateNone {
			continue
		}
		if !found || b.CreatedAt.After(best.CreatedAt) {
			best = b
			found = true
		}
	}
	if !found {
		return "", false, nil
	}
	return best.ID, true, nil
}

func (s *Server) resolveSessionTargetIDWithContext(ctx context.Context, store beads.Store, identifier string, opts apiSessionResolveOptions) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	if _, ok := parseAPITemplateTarget(identifier); ok {
		return "", apiSessionTargetNotFound(identifier)
	}
	if id, err := session.ResolveSessionIDByExactID(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, matched, err := s.resolveConfiguredNamedSessionIDWithContext(ctx, store, identifier, opts); err == nil {
		return id, nil
	} else if matched || !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, err := session.ResolveSessionID(store, identifier); err == nil {
		if cfg := s.state.Config(); cfg != nil {
			if bead, getErr := store.Get(id); getErr == nil && apiIsNamedSessionBead(bead) {
				identity := apiNamedSessionIdentity(bead)
				if identity != "" && config.FindNamedSession(cfg, identity) == nil {
					return "", apiSessionTargetRejectedByConfig(identifier)
				}
			}
		}
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, ok, err := resolveLiveSessionByPathAlias(store, identifier); err != nil {
		return "", err
	} else if ok {
		return id, nil
	}
	if opts.allowClosed {
		if _, ok, err := s.findNamedSessionSpecForTarget(store, identifier); err != nil {
			return "", err
		} else if ok {
			return "", apiSessionTargetNotFound(identifier)
		}
		if id, err := session.ResolveSessionIDAllowClosed(store, identifier); err == nil {
			return id, nil
		} else if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	return "", apiSessionTargetNotFound(identifier)
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

func (s *Server) resolveSessionIDMaterializingNamed(store beads.Store, identifier string) (string, error) {
	return s.resolveSessionTargetID(store, identifier, apiSessionResolveOptions{materialize: true})
}

func (s *Server) resolveSessionIDMaterializingNamedWithContext(ctx context.Context, store beads.Store, identifier string) (string, error) {
	return s.resolveSessionTargetIDWithContext(ctx, store, identifier, apiSessionResolveOptions{materialize: true})
}

func (s *Server) submitMessageToSession(ctx context.Context, store beads.Store, id, message string, intent session.SubmitIntent) (session.SubmitOutcome, error) {
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return session.SubmitOutcome{}, err
	}
	result, err := handle.Message(ctx, worker.MessageRequest{
		Text:     message,
		Delivery: workerDeliveryIntent(intent),
	})
	if err != nil {
		return session.SubmitOutcome{}, err
	}
	return session.SubmitOutcome{Queued: result.Queued}, nil
}

// sendBackgroundMessageToSession preserves the default provider nudge semantics
// for system-driven messages that should respect wait-idle behavior when the
// runtime supports it.
func (s *Server) sendBackgroundMessageToSession(ctx context.Context, store beads.Store, id, message string) error {
	handle, err := s.workerHandleForSession(store, id)
	if err != nil {
		return err
	}
	_, err = handle.Nudge(ctx, worker.NudgeRequest{Text: message})
	return err
}

// sendUserMessageToSession keeps POST /messages as a compatibility alias for
// the semantic default submit path.
func (s *Server) sendUserMessageToSession(ctx context.Context, store beads.Store, id, message string) error {
	_, err := s.submitMessageToSession(ctx, store, id, message, session.SubmitIntentDefault)
	return err
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
