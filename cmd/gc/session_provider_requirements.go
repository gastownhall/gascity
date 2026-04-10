package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
)

const execSessionProviderInstallHintPrefix = "ensure the exec session provider script exists and is executable"

type sessionProviderDependency struct {
	name        string
	lookupName  string
	installHint string
	provider    string
}

// effectiveSessionProviderForCity returns the effective session backend name
// for a city after applying the GC_SESSION override. When config cannot be
// loaded, it falls back to the default provider contract.
func effectiveSessionProviderForCity(cityPath string) string {
	configured := ""
	if cfg, err := loadCityConfig(cityPath); err == nil {
		configured = cfg.Session.Provider
	}
	return effectiveProviderName(configured)
}

func sessionProviderDependencies(name string) []sessionProviderDependency {
	if strings.HasPrefix(name, "exec:") {
		script := strings.TrimSpace(strings.TrimPrefix(name, "exec:"))
		displayName := script
		if displayName == "" {
			displayName = "exec session provider script"
		}
		return []sessionProviderDependency{{
			name:        displayName,
			lookupName:  script,
			installHint: execSessionProviderInstallHint(name),
			provider:    name,
		}}
	}
	if sessionProviderRequiresTmux(name) {
		return []sessionProviderDependency{{
			name:        "tmux",
			lookupName:  "tmux",
			installHint: "https://github.com/tmux/tmux/wiki/Installing",
			provider:    name,
		}}
	}
	return nil
}

func execSessionProviderInstallHint(provider string) string {
	example := strings.TrimSpace(provider)
	if example == "" || example == "exec:" {
		example = "exec:/path/to/script"
	}
	return fmt.Sprintf(
		"%s; set GC_SESSION=%s or [session].provider = %q",
		execSessionProviderInstallHintPrefix,
		example,
		example,
	)
}

// sessionProviderRequiresTmux reports whether the effective session backend
// depends on a local tmux installation.
func sessionProviderRequiresTmux(name string) bool {
	if strings.HasPrefix(name, "exec:") {
		return false
	}
	switch name {
	case "subprocess", "acp", "k8s", "fake", "fail":
		return false
	case "", "tmux", "hybrid":
		return true
	default:
		// Unknown provider names currently fall back to the tmux adapter.
		return true
	}
}

type providerDependencyCheck struct {
	dependency sessionProviderDependency
	lookPath   func(string) (string, error)
}

func newProviderDependencyCheck(dep sessionProviderDependency, lookPath func(string) (string, error)) *providerDependencyCheck {
	return &providerDependencyCheck{dependency: dep, lookPath: lookPath}
}

func (c *providerDependencyCheck) Name() string {
	base := c.dependency.lookupName
	if base == "" {
		base = c.dependency.name
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "session-provider"
	}
	sanitized := strings.NewReplacer("/", "-", ":", "-", " ", "-", ".", "-").Replace(base)
	return "session-provider-" + sanitized
}

func (c *providerDependencyCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	if c.dependency.lookupName == "" {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("exec session provider %q is missing a script path", c.dependency.provider)
		r.FixHint = c.dependency.installHint
		return r
	}
	path, err := c.lookPath(c.dependency.lookupName)
	if err != nil {
		r.Status = doctor.StatusError
		if strings.HasPrefix(c.dependency.provider, "exec:") {
			r.Message = fmt.Sprintf("exec session provider %q could not find script %q", c.dependency.provider, c.dependency.lookupName)
		} else {
			r.Message = fmt.Sprintf("%s not found", c.dependency.lookupName)
		}
		r.FixHint = c.dependency.installHint
		return r
	}
	r.Status = doctor.StatusOK
	r.Message = fmt.Sprintf("found %s", path)
	return r
}

func (c *providerDependencyCheck) CanFix() bool { return false }

func (c *providerDependencyCheck) Fix(_ *doctor.CheckContext) error { return nil }
