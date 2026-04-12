package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

var errTemplateTargetNotFound = errors.New("template target not found")

type ensureSessionForTemplateOptions struct {
	forceFresh          bool
	materializeMetadata map[string]string
}

func ensureSessionForTemplate(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	templateName string,
	stderr io.Writer,
) (string, error) {
	return ensureSessionForTemplateWithOptions(cityPath, cfg, store, templateName, stderr, ensureSessionForTemplateOptions{})
}

func ensureSessionForTemplateWithOptions(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	templateName string,
	stderr io.Writer,
	opts ensureSessionForTemplateOptions,
) (string, error) {
	return materializeSessionForTemplateWithOptions(cityPath, cfg, store, templateName, stderr, opts)
}

func materializeSessionForTemplateWithOptions(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	templateName string,
	stderr io.Writer,
	opts ensureSessionForTemplateOptions,
) (string, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	templateName = normalizeNamedSessionTarget(templateName)
	if templateName == "" {
		return "", fmt.Errorf("%w: %q", errTemplateTargetNotFound, templateName)
	}
	if store == nil {
		return "", fmt.Errorf("session store unavailable for template %q", templateName)
	}
	if cfg == nil {
		return "", fmt.Errorf("city config unavailable for template %q", templateName)
	}
	cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))

	var (
		found    config.Agent
		foundTpl bool
		spec     namedSessionSpec
		hasNamed bool
	)
	if !opts.forceFresh {
		var err error
		spec, hasNamed, err = findNamedSessionSpecForTarget(cfg, cityName, templateName)
		if err != nil {
			return "", err
		}
	}
	if !hasNamed {
		found, foundTpl = resolveSessionTemplate(cfg, templateName, currentRigContext(cfg))
		if !foundTpl {
			return "", fmt.Errorf("%w: %q", errTemplateTargetNotFound, templateName)
		}
		if !opts.forceFresh {
			if resolvedSpec, foundNamed := findNamedSessionSpec(cfg, cityName, found.QualifiedName()); foundNamed {
				spec = resolvedSpec
				hasNamed = true
			}
		}
	}

	if hasNamed {
		if snapshot, err := loadSessionBeadSnapshot(store); err == nil {
			if bead, ok := findCanonicalNamedSessionBead(snapshot, spec); ok {
				if sn := bead.Metadata["session_name"]; sn != "" {
					return sn, nil
				}
			}
			// No open bead found. Check for a closed bead with this
			// identity and reopen it rather than creating a new one.
			// This preserves the bead ID so existing references (slings,
			// convoys, messages) continue to work. Supersedes PR #204.
			if bead, ok := reopenClosedConfiguredNamedSessionBead(
				cityPath, store, cfg, cityName, spec.Identity, spec.SessionName, "stopped", time.Now().UTC(), opts.materializeMetadata, stderr,
			); ok {
				if sn := strings.TrimSpace(bead.Metadata["session_name"]); sn != "" {
					snapshot.add(bead)
					return sn, nil
				}
			}
		}

		resolved, err := config.ResolveProvider(spec.Agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
		if err != nil {
			return "", err
		}
		workDir, err := resolveWorkDir(cityPath, cfg, spec.Agent)
		if err != nil {
			return "", err
		}

		sp := newSessionProvider()
		mgr := newSessionManager(store, sp)
		title := spec.Identity
		templateIdentity := namedSessionBackingTemplate(spec)
		extraMeta := map[string]string{
			namedSessionMetadataKey:      boolMetadata(true),
			namedSessionIdentityMetadata: spec.Identity,
			namedSessionModeMetadata:     spec.Mode,
			"session_origin":             "named",
		}
		for k, v := range opts.materializeMetadata {
			extraMeta[k] = v
		}
		if resolved.Kind != "" && resolved.Kind != resolved.Name {
			extraMeta["provider_kind"] = resolved.Kind
		}
		resume := session.ProviderResume{
			ResumeFlag:    resolved.ResumeFlag,
			ResumeStyle:   resolved.ResumeStyle,
			ResumeCommand: resolved.ResumeCommand,
			SessionIDFlag: resolved.SessionIDFlag,
		}

		if cityUsesManagedReconciler(cityPath) {
			if pokeErr := pokeController(cityPath); pokeErr == nil {
				var info session.Info
				createErr := session.WithCitySessionIdentifierLocks(cityPath, []string{spec.Identity, spec.SessionName}, func() error {
					if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, spec.Identity, "", spec.Identity); err != nil {
						return err
					}
					if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, spec.SessionName, "", spec.Identity); err != nil {
						return err
					}
					var err error
					info, err = mgr.CreateAliasedBeadOnlyNamedWithMetadata(
						spec.Identity,
						spec.SessionName,
						templateIdentity,
						title,
						resolved.CommandString(),
						workDir,
						resolved.Name,
						spec.Agent.Session,
						resume,
						extraMeta,
					)
					return err
				})
				if createErr == nil {
					_ = pokeController(cityPath)
					return info.SessionName, nil
				}
				if snapshot, err := loadSessionBeadSnapshot(store); err == nil {
					if bead, ok := findCanonicalNamedSessionBead(snapshot, spec); ok {
						if sn := bead.Metadata["session_name"]; sn != "" {
							return sn, nil
						}
					}
				} else if stderr != nil {
					fmt.Fprintf(stderr, "session materialize: reloading canonical named session %q after create failure: %v\n", spec.Identity, err) //nolint:errcheck
				}
				return "", createErr
			}
		}

		hints := runtime.Config{
			ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
			ReadyDelayMs:           resolved.ReadyDelayMs,
			ProcessNames:           resolved.ProcessNames,
			EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		}
		var info session.Info
		err = session.WithCitySessionIdentifierLocks(cityPath, []string{spec.Identity, spec.SessionName}, func() error {
			if err := session.EnsureAliasAvailableWithConfigForOwner(store, cfg, spec.Identity, "", spec.Identity); err != nil {
				return err
			}
			if err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, spec.SessionName, "", spec.Identity); err != nil {
				return err
			}
			var createErr error
			info, createErr = mgr.CreateAliasedNamedWithTransportAndMetadata(
				context.Background(),
				spec.Identity,
				spec.SessionName,
				templateIdentity,
				title,
				resolved.CommandString(),
				workDir,
				resolved.Name,
				spec.Agent.Session,
				resolved.Env,
				resume,
				hints,
				extraMeta,
			)
			return createErr
		})
		if err == nil {
			return info.SessionName, nil
		}
		if snapshot, snapErr := loadSessionBeadSnapshot(store); snapErr == nil {
			if bead, ok := findCanonicalNamedSessionBead(snapshot, spec); ok {
				if sn := bead.Metadata["session_name"]; sn != "" {
					return sn, nil
				}
			}
		} else if stderr != nil {
			fmt.Fprintf(stderr, "session materialize: reloading canonical named session %q after transport create failure: %v\n", spec.Identity, snapErr) //nolint:errcheck
		}
		return "", err
	}

	return materializeSessionForAgentConfig(cityPath, cfg, store, &found)
}

func ensureSessionIDForTemplate(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	templateName string,
	stderr io.Writer,
) (string, error) {
	return ensureSessionIDForTemplateWithOptions(cityPath, cfg, store, templateName, stderr, ensureSessionForTemplateOptions{})
}

func ensureSessionIDForTemplateWithOptions(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	templateName string,
	stderr io.Writer,
	opts ensureSessionForTemplateOptions,
) (string, error) {
	sessionName, err := materializeSessionForTemplateWithOptions(cityPath, cfg, store, templateName, stderr, opts)
	if err != nil {
		return "", err
	}
	sessionID, err := session.ResolveSessionID(store, sessionName)
	if err != nil {
		return "", fmt.Errorf("resolving materialized session %q: %w", sessionName, err)
	}
	return sessionID, nil
}

func materializeSessionForAgentConfig(cityPath string, cfg *config.City, store beads.Store, agentCfg *config.Agent) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("city config unavailable")
	}
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	if agentCfg == nil {
		return "", fmt.Errorf("agent config unavailable")
	}

	resolved, err := config.ResolveProvider(agentCfg, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return "", err
	}
	workDir, err := resolveWorkDir(cityPath, cfg, agentCfg)
	if err != nil {
		return "", err
	}

	sp := newSessionProvider()
	mgr := newSessionManager(store, sp)
	title := agentCfg.QualifiedName()
	resume := session.ProviderResume{
		ResumeFlag:    resolved.ResumeFlag,
		ResumeStyle:   resolved.ResumeStyle,
		ResumeCommand: resolved.ResumeCommand,
		SessionIDFlag: resolved.SessionIDFlag,
	}

	if cityUsesManagedReconciler(cityPath) {
		if pokeErr := pokeController(cityPath); pokeErr == nil {
			info, createErr := mgr.CreateAliasedBeadOnlyNamedWithMetadata(
				"",
				"",
				agentCfg.QualifiedName(),
				title,
				resolved.CommandString(),
				workDir,
				resolved.Name,
				agentCfg.Session,
				resume,
				map[string]string{"session_origin": "ephemeral"},
			)
			if createErr == nil {
				_ = pokeController(cityPath)
				return info.SessionName, nil
			}
			return "", createErr
		}
	}

	hints := runtime.Config{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
	}
	info, err := mgr.CreateAliasedNamedWithTransportAndMetadata(
		context.Background(),
		"",
		"",
		agentCfg.QualifiedName(),
		title,
		resolved.CommandString(),
		workDir,
		resolved.Name,
		agentCfg.Session,
		resolved.Env,
		resume,
		hints,
		map[string]string{"session_origin": "ephemeral"},
	)
	if err == nil {
		return info.SessionName, nil
	}
	return "", err
}
