package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/doctor"
)

const execSessionProviderInstallHintPrefix = "ensure the exec session provider script exists and is executable"

const execSessionProviderSmokeCheckTimeout = 3 * time.Second

type coreBinaryDependencyOptions struct {
	includePackManaged bool
}

type binaryDependencyKind int

const (
	binaryDependencyKindBinary binaryDependencyKind = iota
	binaryDependencyKindExecSessionProvider
)

type binaryDependency struct {
	name        string
	lookupName  string
	installHint string
	minVersion  string
	provider    string
	kind        binaryDependencyKind
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

func needsPackManagedBinaryDependencies(beadsProvider string) bool {
	return providerUsesBdStoreContract(beadsProvider)
}

func coreBinaryDependencies(sessionProvider, beadsProvider string, opts coreBinaryDependencyOptions) []binaryDependency {
	deps := []binaryDependency{
		{
			name:        "jq",
			lookupName:  "jq",
			installHint: "brew install jq (macOS) or apt install jq (Linux)",
		},
		{
			name:        "git",
			lookupName:  "git",
			installHint: "https://git-scm.com/downloads",
		},
		{
			name:        "pgrep",
			lookupName:  "pgrep",
			installHint: "brew install proctools (macOS) or apt install procps (Linux)",
		},
		{
			name:        "lsof",
			lookupName:  "lsof",
			installHint: "brew install lsof (macOS) or apt install lsof (Linux)",
		},
	}
	if opts.includePackManaged && needsPackManagedBinaryDependencies(beadsProvider) {
		deps = append(deps,
			binaryDependency{
				name:        "dolt",
				lookupName:  "dolt",
				installHint: "https://github.com/dolthub/dolt/releases",
				minVersion:  doltMinVersion,
			},
			binaryDependency{
				name:        "bd",
				lookupName:  "bd",
				installHint: "https://github.com/gastownhall/beads/releases",
				minVersion:  bdMinVersion,
			},
			binaryDependency{
				name:        "flock",
				lookupName:  "flock",
				installHint: "brew install flock (macOS) or apt install util-linux (Linux)",
			},
		)
	}
	return append(deps, sessionProviderDependencies(sessionProvider)...)
}

func sessionProviderDependencies(name string) []binaryDependency {
	if strings.HasPrefix(name, "exec:") {
		script := strings.TrimSpace(strings.TrimPrefix(name, "exec:"))
		displayName := script
		if displayName == "" {
			displayName = "exec session provider script"
		}
		return []binaryDependency{{
			name:        displayName,
			lookupName:  script,
			installHint: execSessionProviderInstallHint(name),
			provider:    name,
			kind:        binaryDependencyKindExecSessionProvider,
		}}
	}
	if sessionProviderRequiresTmux(name) {
		return []binaryDependency{{
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
		// Keep dependency preflight aligned with newSessionProviderByName:
		// unknown session provider names currently fall back to the tmux adapter.
		return true
	}
}

var runExecSessionProviderSmokeCheck = execSessionProviderSmokeCheck

func validateBinaryDependency(dep binaryDependency, resolvedPath string) error {
	switch dep.kind {
	case binaryDependencyKindExecSessionProvider:
		return runExecSessionProviderSmokeCheck(resolvedPath)
	default:
		return nil
	}
}

func execSessionProviderSmokeCheck(scriptPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), execSessionProviderSmokeCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, scriptPath, "validate")
	// Match the runtime provider's bounded-exec behavior so preflight cannot hang
	// if a buggy script leaves descendants holding pipes open.
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = io.Discard

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("validation timed out after %s", execSessionProviderSmokeCheckTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		// Unknown operations are defined as a no-op success in the exec-provider
		// protocol, so this still proves the script is runnable by the runtime.
		return nil
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("validate operation failed: %s", msg)
}

type binaryDependencyCheck struct {
	dependency binaryDependency
	lookPath   func(string) (string, error)
}

func newBinaryDependencyCheck(dep binaryDependency, lookPath func(string) (string, error)) *binaryDependencyCheck {
	return &binaryDependencyCheck{dependency: dep, lookPath: lookPath}
}

func (c *binaryDependencyCheck) Name() string {
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

func (c *binaryDependencyCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
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
	if err := validateBinaryDependency(c.dependency, path); err != nil {
		r.Status = doctor.StatusError
		if c.dependency.kind == binaryDependencyKindExecSessionProvider {
			r.Message = fmt.Sprintf("exec session provider %q is not runnable: %v", c.dependency.provider, err)
		} else {
			r.Message = fmt.Sprintf("%s failed validation: %v", c.dependency.lookupName, err)
		}
		r.FixHint = c.dependency.installHint
		return r
	}
	r.Status = doctor.StatusOK
	r.Message = fmt.Sprintf("found %s", path)
	return r
}

func (c *binaryDependencyCheck) CanFix() bool { return false }

func (c *binaryDependencyCheck) Fix(_ *doctor.CheckContext) error { return nil }
