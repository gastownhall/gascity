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
)

const (
	apiTemplateTargetPrefix    = "template:"
	apiNamedSessionMetadataKey = session.NamedSessionMetadataKey
	apiNamedSessionIdentityKey = session.NamedSessionIdentityMetadata
	apiNamedSessionModeKey     = session.NamedSessionModeMetadata
)

var errConfiguredNamedSessionConflict = errors.New("configured named session conflict")

type apiSessionResolveOptions struct {
	allowClosed bool
	materialize bool
}

type apiNamedSessionSpec = session.NamedSessionSpec

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
	return session.FindNamedSessionSpecForTarget(cfg, cityName, target, "")
}

func (s *Server) findCanonicalNamedSession(store beads.Store, spec apiNamedSessionSpec) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("listing sessions: %w", err)
	}
	bead, ok := session.FindCanonicalNamedSessionBead(all, spec)
	return bead, ok, nil
}

func (s *Server) retireContinuityIneligibleNamedSessionIdentifiers(store beads.Store, spec apiNamedSessionSpec) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	all, err := store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
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
			_ = s.state.SessionProvider().Stop(sessionName)
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
		work, err := store.List(beads.ListQuery{Assignee: oldID, Status: status})
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
	bead, hasCanonical, err := s.findCanonicalNamedSession(store, spec)
	if err != nil {
		return "", true, err
	}
	if hasCanonical {
		return bead.ID, true, nil
	}

	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
	})
	if err != nil {
		return "", true, fmt.Errorf("listing sessions: %w", err)
	}
	if bead, conflict := session.FindNamedSessionConflict(all, spec); conflict {
		return "", true, fmt.Errorf("%w: %q conflicts with configured named session %q via live bead %s", errConfiguredNamedSessionConflict, identifier, spec.Identity, bead.ID)
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

	resolved, workDir, transport, qualifiedTemplate, err := s.resolveSessionTemplate(spec.Agent.QualifiedName())
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
	hints := sessionCreateHints(resolved)
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
			resolved.CommandString(),
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

func (s *Server) resolveSessionTargetIDWithContext(ctx context.Context, store beads.Store, identifier string, opts apiSessionResolveOptions) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	if _, ok := parseAPITemplateTarget(identifier); ok {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
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
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if opts.allowClosed {
		if _, ok, err := s.findNamedSessionSpecForTarget(store, identifier); err != nil {
			return "", err
		} else if ok {
			return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
		}
		if id, err := session.ResolveSessionIDAllowClosed(store, identifier); err == nil {
			return id, nil
		} else if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if !opts.materialize {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
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
	mgr := s.sessionManager(store)
	info, err := mgr.Get(id)
	if err != nil {
		return session.SubmitOutcome{}, err
	}
	resumeCommand, hints := s.buildSessionResume(info)
	return mgr.Submit(ctx, id, message, resumeCommand, hints, intent)
}

// sendBackgroundMessageToSession preserves the default provider nudge semantics
// for system-driven messages that should respect wait-idle behavior when the
// runtime supports it.
func (s *Server) sendBackgroundMessageToSession(ctx context.Context, store beads.Store, id, message string) error {
	mgr := s.sessionManager(store)
	info, err := mgr.Get(id)
	if err != nil {
		return err
	}
	resumeCommand, hints := s.buildSessionResume(info)
	if err := mgr.Send(ctx, id, message, resumeCommand, hints); err != nil {
		return err
	}
	return nil
}

// sendUserMessageToSession keeps POST /messages as a compatibility alias for
// the semantic default submit path.
func (s *Server) sendUserMessageToSession(ctx context.Context, store beads.Store, id, message string) error {
	_, err := s.submitMessageToSession(ctx, store, id, message, session.SubmitIntentDefault)
	return err
}
