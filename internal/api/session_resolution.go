package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	apiTemplateTargetPrefix    = "template:"
	apiNamedSessionMetadataKey = "configured_named_session"
	apiNamedSessionIdentityKey = "configured_named_identity"
	apiNamedSessionModeKey     = "configured_named_mode"
)

var errConfiguredNamedSessionConflict = errors.New("configured named session conflict")

type apiSessionResolveOptions struct {
	allowClosed bool
	materialize bool
}

type apiNamedSessionSpec struct {
	Named       *config.NamedSession
	Agent       *config.Agent
	Identity    string
	SessionName string
	Mode        string
}

func apiNormalizeSessionTarget(target string) string {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, "/")
	return target
}

func apiTargetBasename(target string) string {
	target = apiNormalizeSessionTarget(target)
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

func apiCityName(cfg *config.City, cityPath string) string {
	return config.EffectiveCityName(cfg, filepath.Base(cityPath))
}

func apiFindNamedSessionSpec(cfg *config.City, cityName, identity string) (apiNamedSessionSpec, bool) {
	identity = apiNormalizeSessionTarget(identity)
	if cfg == nil || identity == "" {
		return apiNamedSessionSpec{}, false
	}
	named := config.FindNamedSession(cfg, identity)
	if named == nil {
		return apiNamedSessionSpec{}, false
	}
	agentCfg := config.FindAgent(cfg, identity)
	if agentCfg == nil {
		return apiNamedSessionSpec{}, false
	}
	return apiNamedSessionSpec{
		Named:       named,
		Agent:       agentCfg,
		Identity:    identity,
		SessionName: config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity),
		Mode:        named.ModeOrDefault(),
	}, true
}

func apiIsNamedSessionBead(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[apiNamedSessionMetadataKey]) == "true"
}

func apiNamedSessionIdentity(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[apiNamedSessionIdentityKey])
}

func apiBeadConflictsWithNamedSession(b beads.Bead, spec apiNamedSessionSpec) bool {
	if apiIsNamedSessionBead(b) && apiNamedSessionIdentity(b) == spec.Identity {
		return false
	}
	if strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName {
		return true
	}
	if strings.TrimSpace(b.Metadata["alias"]) == spec.Identity {
		return true
	}
	for _, alias := range session.AliasHistory(b.Metadata) {
		if alias == spec.Identity {
			return true
		}
	}
	return false
}

func apiResolveSessionIDByExactID(store beads.Store, identifier string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	b, err := store.Get(identifier)
	if err == nil && b.Type == session.BeadType {
		return b.ID, nil
	}
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return "", fmt.Errorf("looking up session %q: %w", identifier, err)
	}
	return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}

func (s *Server) findNamedSessionSpecForTarget(store beads.Store, target string) (apiNamedSessionSpec, bool, error) {
	cfg := s.state.Config()
	target = apiNormalizeSessionTarget(target)
	if cfg == nil || target == "" {
		return apiNamedSessionSpec{}, false, nil
	}
	cityName := apiCityName(cfg, s.state.CityPath())

	if spec, ok := apiFindNamedSessionSpec(cfg, cityName, target); ok {
		return spec, true, nil
	}
	if agentCfg, ok := resolveSessionTemplateAgent(cfg, target); ok {
		if spec, ok := apiFindNamedSessionSpec(cfg, cityName, agentCfg.QualifiedName()); ok {
			return spec, true, nil
		}
	}

	var matched apiNamedSessionSpec
	found := false
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := apiFindNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if spec.SessionName == target {
			if found && matched.Identity != spec.Identity {
				return apiNamedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
			}
			matched = spec
			found = true
		}
	}
	if found {
		return matched, true, nil
	}

	if strings.Contains(target, "/") {
		return apiNamedSessionSpec{}, false, nil
	}
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := apiFindNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if apiTargetBasename(spec.Identity) != target {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return apiNamedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
		}
		matched = spec
		found = true
	}
	if found {
		return matched, true, nil
	}

	if store == nil {
		return apiNamedSessionSpec{}, false, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
		Type:  session.BeadType,
	})
	if err != nil {
		return apiNamedSessionSpec{}, false, fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if b.Status == "closed" || !apiIsNamedSessionBead(b) {
			continue
		}
		matchesHistory := false
		for _, alias := range session.AliasHistory(b.Metadata) {
			if alias == target {
				matchesHistory = true
				break
			}
		}
		if !matchesHistory {
			continue
		}
		spec, ok := apiFindNamedSessionSpec(cfg, cityName, apiNamedSessionIdentity(b))
		if !ok {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return apiNamedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
		}
		matched = spec
		found = true
	}
	return matched, found, nil
}

func (s *Server) findCanonicalNamedSession(store beads.Store, identity string) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
		Type:  session.BeadType,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("listing sessions: %w", err)
	}
	identity = apiNormalizeSessionTarget(identity)
	for _, b := range all {
		if b.Status == "closed" {
			continue
		}
		if apiIsNamedSessionBead(b) && apiNamedSessionIdentity(b) == identity {
			return b, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

func (s *Server) resolveConfiguredNamedSessionID(store beads.Store, identifier string, opts apiSessionResolveOptions) (string, bool, error) {
	spec, ok, err := s.findNamedSessionSpecForTarget(store, identifier)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	bead, hasCanonical, err := s.findCanonicalNamedSession(store, spec.Identity)
	if err != nil {
		return "", true, err
	}
	if hasCanonical {
		return bead.ID, true, nil
	}

	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
		Type:  session.BeadType,
	})
	if err != nil {
		return "", true, fmt.Errorf("listing sessions: %w", err)
	}
	for _, b := range all {
		if b.Status == "closed" {
			continue
		}
		if apiBeadConflictsWithNamedSession(b, spec) {
			return "", true, fmt.Errorf("%w: %q conflicts with configured named session %q via live bead %s", errConfiguredNamedSessionConflict, identifier, spec.Identity, b.ID)
		}
	}

	if !opts.materialize {
		return "", true, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	id, err := s.materializeNamedSession(store, spec)
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

func apiAllowImplicitTemplateMaterialization(cfg *config.City, identifier string) bool {
	if cfg == nil {
		return true
	}
	agentCfg, ok := resolveSessionTemplateAgent(cfg, identifier)
	if !ok {
		return true
	}
	maxSess := agentCfg.EffectiveMaxActiveSessions()
	return maxSess != nil && *maxSess == 1
}

func (s *Server) materializeTemplateSession(store beads.Store, template string) (string, error) {
	resolved, workDir, transport, qualifiedTemplate, err := s.resolveSessionTemplate(template)
	if err != nil {
		if errors.Is(err, errSessionTemplateNotFound) {
			return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, template)
		}
		return "", err
	}
	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}
	mgr := s.sessionManager(store)
	hints := sessionCreateHints(resolved)
	info, err := mgr.CreateAliasedNamedWithTransport(
		context.Background(),
		"",
		"",
		qualifiedTemplate,
		qualifiedTemplate,
		resolved.CommandString(),
		workDir,
		resolved.Name,
		transport,
		resolved.Env,
		resume,
		hints,
	)
	if err != nil {
		return "", err
	}
	s.state.Poke()
	return info.ID, nil
}

func (s *Server) materializeNamedSession(store beads.Store, spec apiNamedSessionSpec) (string, error) {
	if bead, ok, err := s.findCanonicalNamedSession(store, spec.Identity); err != nil {
		return "", err
	} else if ok {
		return bead.ID, nil
	}

	resolved, workDir, transport, qualifiedTemplate, err := s.resolveSessionTemplate(spec.Identity)
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
			context.Background(),
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
		s.state.Poke()
		return info.ID, nil
	}
	if bead, ok, lookupErr := s.findCanonicalNamedSession(store, spec.Identity); lookupErr == nil && ok {
		return bead.ID, nil
	}
	return "", err
}

func (s *Server) materializeSessionTarget(store beads.Store, identifier string) (string, error) {
	identifier = apiNormalizeSessionTarget(identifier)
	if identifier == "" {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	if spec, ok, err := s.findNamedSessionSpecForTarget(store, identifier); err != nil {
		return "", err
	} else if ok {
		return s.materializeNamedSession(store, spec)
	}
	return s.materializeTemplateSession(store, identifier)
}

func (s *Server) resolveSessionTargetID(store beads.Store, identifier string, opts apiSessionResolveOptions) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	if templateName, ok := parseAPITemplateTarget(identifier); ok {
		if !opts.materialize {
			return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
		}
		return s.materializeTemplateSession(store, templateName)
	}
	if id, err := apiResolveSessionIDByExactID(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, err := session.ResolveSessionID(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, matched, err := s.resolveConfiguredNamedSessionID(store, identifier, opts); err == nil {
		return id, nil
	} else if matched || !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if opts.allowClosed {
		if id, err := session.ResolveSessionIDAllowClosed(store, identifier); err == nil {
			return id, nil
		} else if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if !opts.materialize {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	if !apiAllowImplicitTemplateMaterialization(s.state.Config(), identifier) {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	return s.materializeSessionTarget(store, identifier)
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

func (s *Server) sendMessageToSession(ctx context.Context, store beads.Store, id, message string) error {
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
