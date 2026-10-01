package doctor

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// failLookPath is a PATH lookup where nothing resolves — the host condition
// ob-woag reported, without depending on what is installed on the machine
// running the tests.
func failLookPath(string) (string, error) { return "", errors.New("not found") }

// okLookPath is a PATH lookup where everything resolves.
func okLookPath(cmd string) (string, error) { return "/usr/bin/" + cmd, nil }

func providerPathCity() *config.City {
	return &config.City{
		Agents: []config.Agent{{Name: "worker", Provider: "ghost"}},
		Providers: map[string]config.ProviderSpec{
			"ghost": {Command: "ghost-cli"},
		},
	}
}

func TestProviderPathCheck_NoConfig(t *testing.T) {
	r := NewProviderPathCheck(nil, exec.LookPath).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want OK", r.Status)
	}
}

func TestProviderPathCheck_ResolvesCleanly(t *testing.T) {
	r := NewProviderPathCheck(providerPathCity(), okLookPath).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", r.Status, r.Message)
	}
}

// TestProviderPathCheck_FlagsMissingCommand is the check ob-woag asked for:
// `gc doctor` passed 100 checks on a city whose provider binary was absent.
func TestProviderPathCheck_FlagsMissingCommand(t *testing.T) {
	r := NewProviderPathCheck(providerPathCity(), failLookPath).Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %v (%s), want Error", r.Status, r.Message)
	}
	if r.Severity != SeverityBlocking {
		t.Errorf("severity = %v, want blocking; the listed templates cannot start at all", r.Severity)
	}
	if len(r.Details) != 1 {
		t.Fatalf("details = %v, want one line", r.Details)
	}
	if !strings.Contains(r.Details[0], `"ghost"`) || !strings.Contains(r.Details[0], `"ghost-cli"`) {
		t.Errorf("detail = %q, want the provider and its command named", r.Details[0])
	}
	if !strings.Contains(r.Details[0], "worker") {
		t.Errorf("detail = %q, want the affected template named", r.Details[0])
	}
	if r.FixHint == "" {
		t.Error("want a fix hint; the operator has to know what to install")
	}
}

// TestProviderPathCheck_GroupsByProvider keeps the finding proportional to the
// fault: one uninstalled binary is one problem, however many agents name it.
func TestProviderPathCheck_GroupsByProvider(t *testing.T) {
	cfg := providerPathCity()
	cfg.Agents = append(cfg.Agents,
		config.Agent{Name: "second", Provider: "ghost"},
		config.Agent{Name: "third", Provider: "ghost"},
	)
	r := NewProviderPathCheck(cfg, failLookPath).Run(&CheckContext{})
	if len(r.Details) != 1 {
		t.Fatalf("details = %v, want one grouped finding for one missing binary", r.Details)
	}
	for _, name := range []string{"worker", "second", "third"} {
		if !strings.Contains(r.Details[0], name) {
			t.Errorf("detail = %q, want %q listed", r.Details[0], name)
		}
	}
}

// TestProviderPathCheck_SkipsStartCommandAgents mirrors provider-parity:
// start_command bypasses ProviderSpec entirely, so ResolveProvider never
// consults PATH for those agents and this check has nothing to say about them.
func TestProviderPathCheck_SkipsStartCommandAgents(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", StartCommand: "ghost-cli --serve"}},
		Providers: map[string]config.ProviderSpec{
			"ghost": {Command: "ghost-cli"},
		},
	}
	r := NewProviderPathCheck(cfg, failLookPath).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", r.Status, r.Message)
	}
}

// TestProviderPathCheck_ChecksWorkspaceDefaultWithoutAgents covers the fresh
// city: workspace.provider is set, the agents have not arrived yet, and the
// default still has to resolve before the first template can start.
func TestProviderPathCheck_ChecksWorkspaceDefaultWithoutAgents(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "ghost"},
		Providers: map[string]config.ProviderSpec{
			"ghost": {Command: "ghost-cli"},
		},
	}
	r := NewProviderPathCheck(cfg, failLookPath).Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %v (%s), want Error", r.Status, r.Message)
	}
}

// TestProviderPathCheck_IgnoresNonPathResolutionErrors keeps this check
// single-purpose. An undeclared provider reference is a config fault that
// config-refs and provider-catalog already report; repeating it here would
// make one bad reference fail several checks and bury the PATH signal.
func TestProviderPathCheck_IgnoresNonPathResolutionErrors(t *testing.T) {
	cfg := &config.City{
		Agents:    []config.Agent{{Name: "worker", Provider: "undeclared"}},
		Providers: map[string]config.ProviderSpec{},
	}
	r := NewProviderPathCheck(cfg, failLookPath).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK — unknown-provider is not this check's finding", r.Status, r.Message)
	}
}

// TestProviderPathCheck_WarmupEligible pins the opt-in. `gc start` runs the
// warm-up-eligible checks, and the reported symptom was a start that announced
// success on a city that could not bring up a single session.
func TestProviderPathCheck_WarmupEligible(t *testing.T) {
	if !NewProviderPathCheck(providerPathCity(), okLookPath).WarmupEligible() {
		t.Error("provider-path must run in the gc start warm-up scan")
	}
}

func TestProviderPathCheck_CannotFix(t *testing.T) {
	c := NewProviderPathCheck(providerPathCity(), failLookPath)
	if c.CanFix() {
		t.Error("CanFix() = true; installing a provider CLI is not the doctor's job")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil", err)
	}
}
