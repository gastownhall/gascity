// session_resolve.go provides CLI-level session resolution.
// The core resolution logic lives in internal/session.ResolveSessionID.
package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// resolveSessionID delegates to session.ResolveSessionID.
func resolveSessionID(store beads.Store, identifier string) (string, error) {
	return session.ResolveSessionID(store, identifier)
}

func resolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return session.ResolveSessionIDAllowClosed(store, identifier)
}

type namedSessionResolveOptions struct {
	allowClosed         bool
	materialize         bool
	materializeMetadata map[string]string
}

const templateTargetPrefix = "template:"

type templateTarget struct {
	template   string
	forceFresh bool
}

var errNamedSessionConflict = errors.New("configured named session conflict")

func resolveSessionIDByExactID(store beads.Store, identifier string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	b, err := store.Get(identifier)
	if err == nil && session.IsSessionBeadOrRepairable(b) {
		session.RepairEmptyType(store, &b)
		return b.ID, nil
	}
	if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return "", fmt.Errorf("looking up session %q: %w", identifier, err)
	}
	return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}

func resolveConfiguredNamedSessionID(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	identifier string,
	opts namedSessionResolveOptions,
) (string, bool, error) {
	if cfg == nil || store == nil {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
	spec, ok, err := findNamedSessionSpecForTarget(cfg, cityName, identifier)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return "", true, err
	}
	if bead, ok := findCanonicalNamedSessionBead(snapshot, spec); ok {
		return bead.ID, true, nil
	}
	// When materializing, check for a closed bead with this identity and
	// reopen it (preserves bead ID for reference continuity).
	if opts.materialize {
		if bead, ok := reopenClosedConfiguredNamedSessionBead(
			cityPath, store, cfg, cityName, spec.Identity, spec.SessionName, "stopped", time.Now().UTC(), opts.materializeMetadata, io.Discard,
		); ok {
			return bead.ID, true, nil
		}
	}
	if bead, conflict := findNamedSessionConflict(snapshot, spec); conflict {
		return "", true, fmt.Errorf("%w: %q conflicts with configured named session %q via live bead %s", errNamedSessionConflict, identifier, spec.Identity, bead.ID)
	}
	if !opts.materialize {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	id, err := ensureSessionIDForTemplateWithOptions(cityPath, cfg, store, spec.Identity, io.Discard, ensureSessionForTemplateOptions{
		materializeMetadata: opts.materializeMetadata,
	})
	return id, true, err
}

func resolveSessionIDWithConfig(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{})
}

func resolveSessionIDAllowClosedWithConfig(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{allowClosed: true})
}

func resolveSessionIDMaterializingNamed(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{materialize: true})
}

func resolveSessionIDMaterializingNamedWithMetadata(cityPath string, cfg *config.City, store beads.Store, identifier string, metadata map[string]string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{
		materialize:         true,
		materializeMetadata: metadata,
	})
}

func allowImplicitTemplateMaterialization(cfg *config.City, identifier string) bool {
	if cfg == nil {
		return true
	}
	agentCfg, ok := resolveSessionTemplate(cfg, identifier, currentRigContext(cfg))
	if !ok {
		return true
	}
	return !isMultiSessionCfgAgent(&agentCfg)
}

func parseTemplateTarget(identifier string) (templateTarget, bool) {
	identifier = strings.TrimSpace(identifier)
	if !strings.HasPrefix(identifier, templateTargetPrefix) {
		return templateTarget{}, false
	}
	name := normalizeNamedSessionTarget(strings.TrimSpace(strings.TrimPrefix(identifier, templateTargetPrefix)))
	if name == "" {
		return templateTarget{}, false
	}
	return templateTarget{
		template:   name,
		forceFresh: true,
	}, true
}

func resolveSessionIDWithOptions(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	identifier string,
	opts namedSessionResolveOptions,
) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	if _, ok := parseTemplateTarget(identifier); ok {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	if id, err := resolveSessionIDByExactID(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, matched, err := resolveConfiguredNamedSessionID(cityPath, cfg, store, identifier, opts); err == nil {
		return id, nil
	} else if matched || !errors.Is(err, session.ErrSessionNotFound) {
		return "", fmt.Errorf("resolving configured named session %q: %w", identifier, err)
	}
	if id, err := session.ResolveSessionID(store, identifier); err == nil {
		if cfg != nil {
			if bead, getErr := store.Get(id); getErr == nil && isNamedSessionBead(bead) {
				identity := namedSessionIdentity(bead)
				if identity != "" && config.FindNamedSession(cfg, identity) == nil {
					return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
				}
			}
		}
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if opts.allowClosed {
		if cfg != nil {
			cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
			if _, ok, err := findNamedSessionSpecForTarget(cfg, cityName, identifier); err != nil {
				return "", err
			} else if ok {
				return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
			}
		}
		if id, err := session.ResolveSessionIDAllowClosed(store, identifier); err == nil {
			return id, nil
		} else if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}
