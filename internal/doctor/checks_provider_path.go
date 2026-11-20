package doctor

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// ProviderPathCheck verifies that the provider command behind every template
// a session can be built from actually resolves on PATH.
//
// It exists because of ob-woag: on a host where a referenced provider binary
// was absent, the controller dropped every session using that provider from
// its desired state and no surface said so. `gc start` reported "City started
// under supervisor.", `gc status` showed the agents as "unknown (partial
// status)", `gc agent list` showed them "active", and `gc doctor` passed 100
// checks — not one of which asked whether a configured provider's command
// exists. The city came up completely empty and the only command that named
// the cause was `gc session new <template>`, which is the one command an
// operator has no reason to run when start already claimed success.
//
// The neighboring provider checks all stop short of this question by design:
// provider-catalog checks that every referenced provider is *declared*,
// provider-catalog-local-readiness checks that locally-configured CLIs are
// listed explicitly, and provider-parity inspects capability fields using a
// stub PATH lookup (providerParityLookPath) because it is auditing config
// semantics, not the host. This check is the one that touches the host.
//
// Reachability is "every configured agent template", not "every template with
// demand right now". A pool template sitting at min_active_sessions = 0 is one
// routed bead away from needing its provider, so a provider that cannot
// resolve is a fault whether or not anything wants a session this second.
// Agents that pin start_command are skipped: they bypass ProviderSpec
// entirely and ResolveProvider never consults PATH for them.
type ProviderPathCheck struct {
	cfg      *config.City
	lookPath config.LookPathFunc
}

// NewProviderPathCheck creates a check that resolves every configured agent's
// provider against the supplied PATH lookup (production passes exec.LookPath).
func NewProviderPathCheck(cfg *config.City, lookPath config.LookPathFunc) *ProviderPathCheck {
	return &ProviderPathCheck{cfg: cfg, lookPath: lookPath}
}

// Name returns the check identifier.
func (c *ProviderPathCheck) Name() string { return "provider-path" }

// providerPathFailure is one provider whose command did not resolve, with the
// templates that would be dropped because of it.
type providerPathFailure struct {
	provider  string
	command   string
	templates []string
}

// Run resolves each configured agent's provider and reports the ones whose
// command is not on PATH.
//
// Only ErrProviderNotInPATH is reported. Every other resolution error
// (unknown provider, an unresolvable base in a provider chain, a bad option
// key) is a config fault that config-valid, config-refs and provider-catalog
// already own; repeating them here would make one broken reference fail four
// checks and bury the PATH signal this check exists to raise.
func (c *ProviderPathCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil || c.lookPath == nil {
		r.Status = StatusOK
		r.Message = "no config; nothing to check"
		return r
	}

	failures := c.collectFailures()
	if len(failures) == 0 {
		r.Status = StatusOK
		r.Message = "every referenced provider command resolves on PATH"
		return r
	}

	details := make([]string, 0, len(failures))
	for _, f := range failures {
		details = append(details, fmt.Sprintf(
			"provider %q command %q is not on PATH: every session for %s is silently dropped from the controller's desired state",
			f.provider, f.command, strings.Join(f.templates, ", "),
		))
	}

	// Blocking, not advisory: an unresolvable provider is not a degraded
	// mode. Every session that names it fails to build, so the templates
	// listed above cannot start at all, and letting a consumer proceed past
	// that is exactly the silence ob-woag reported.
	r.Status = StatusError
	r.Severity = SeverityBlocking
	r.Message = fmt.Sprintf("%d provider command(s) not on PATH", len(failures))
	r.Details = details
	r.FixHint = "install the provider CLI (or point [providers.<name>].command at an installed binary) and re-run; `gc session new <template>` reports the same error for a single template"
	return r
}

// collectFailures resolves every reachable agent and groups the PATH failures
// by provider so a single missing binary shared by twenty agents reports as
// one finding naming twenty templates, not twenty findings.
func (c *ProviderPathCheck) collectFailures() []providerPathFailure {
	byProvider := map[string]*providerPathFailure{}

	record := func(template string, agent config.Agent) {
		_, err := config.ResolveProvider(&agent, &c.cfg.Workspace, c.cfg.Providers, c.lookPath)
		if err == nil {
			return
		}
		var notInPath *config.ProviderNotInPATHError
		if !errors.As(err, &notInPath) {
			return
		}
		key := notInPath.Provider + "\x00" + notInPath.Command
		f, ok := byProvider[key]
		if !ok {
			f = &providerPathFailure{provider: notInPath.Provider, command: notInPath.Command}
			byProvider[key] = f
		}
		if template != "" {
			f.templates = append(f.templates, template)
		}
	}

	checkedAgent := false
	for _, a := range c.cfg.Agents {
		if a.StartCommand != "" {
			continue
		}
		if strings.TrimSpace(a.Provider) == "" && strings.TrimSpace(c.cfg.Workspace.Provider) == "" {
			continue
		}
		checkedAgent = true
		record(a.QualifiedName(), a)
	}
	// A city can declare workspace.provider with no agents yet (a fresh
	// `gc init`, or one whose agents all arrive from a pack that has not
	// been installed). The default provider still has to resolve before the
	// first template can start, so probe it under its own identity.
	if !checkedAgent && strings.TrimSpace(c.cfg.Workspace.Provider) != "" {
		record("workspace default provider", config.Agent{})
	}

	failures := make([]providerPathFailure, 0, len(byProvider))
	for _, f := range byProvider {
		sort.Strings(f.templates)
		failures = append(failures, *f)
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].provider != failures[j].provider {
			return failures[i].provider < failures[j].provider
		}
		return failures[i].command < failures[j].command
	})
	return failures
}

// CanFix returns false — installing a provider CLI is not something the
// doctor can do on the operator's behalf.
func (c *ProviderPathCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ProviderPathCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns true. `gc start` runs the warm-up-eligible checks and
// prints a failure line before handing off to the supervisor, which is the
// specific moment ob-woag reported as falsely clean: a city that cannot start
// any of its always-on sessions announced "City started under supervisor." and
// nothing else. The check is pure config reads plus one PATH lookup per
// distinct provider, so it costs nothing against the warm-up deadline.
func (c *ProviderPathCheck) WarmupEligible() bool { return true }
