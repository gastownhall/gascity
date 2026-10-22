package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func isolationIntPtr(v int) *int { return &v }

// newIsolationCity builds a city whose rig "app" lives at an absolute path,
// the shape buildDoctorChecks hands the check after resolveRigPaths.
func newIsolationCity(t *testing.T) (string, string) {
	t.Helper()
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "rigs", "app")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	return cityPath, rigRoot
}

// isolationGit runs git in dir, failing the test on error.
func isolationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// isolationGitRig initializes the rig root as a git repository, optionally
// with a commit so HEAD resolves.
func isolationGitRig(t *testing.T, rigRoot string, withCommit bool) {
	t.Helper()
	isolationGit(t, rigRoot, "init", "-b", "main")
	isolationGit(t, rigRoot, "config", "user.email", "test@example.com")
	isolationGit(t, rigRoot, "config", "user.name", "Test")
	if !withCommit {
		return
	}
	if err := os.WriteFile(filepath.Join(rigRoot, "hello.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing rig file: %v", err)
	}
	isolationGit(t, rigRoot, "add", "hello.txt")
	isolationGit(t, rigRoot, "commit", "-m", "init")
}

// isolationRigs returns the rig list for the fixture city.
func isolationRigs(rigRoot string) []config.Rig {
	return []config.Rig{{Name: "app", Path: rigRoot}}
}

// runIsolationCheck runs the check over cfg with a host-local session
// provider, returning its result.
func runIsolationCheck(cfg *config.City, cityPath string) *CheckResult {
	return NewPoolWorkDirIsolationCheck(cfg, cityPath, "test-city", "tmux", true).Run(nil)
}

// assertIsolationWarning fails unless the result is the expected advisory
// warning naming the agent, and returns its details for per-case assertions.
func assertIsolationWarning(t *testing.T, r *CheckResult, agent string) []string {
	t.Helper()
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; message=%q", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want advisory", r.Severity)
	}
	if len(r.Details) == 0 {
		t.Fatalf("warning %q carries no details", r.Message)
	}
	for _, d := range r.Details {
		if strings.Contains(d, agent) {
			return r.Details
		}
	}
	t.Fatalf("warning does not name agent %q; details=%v", agent, r.Details)
	return nil
}

// TestPoolWorkDirIsolationReportsOKWhenEveryPoolIsIsolated is AC-7's OK arm:
// a marked implicit agent on a committed git rig probes to distinct slot
// worktrees, so the check passes.
func TestPoolWorkDirIsolationReportsOKWhenEveryPoolIsIsolated(t *testing.T) {
	cityPath, rigRoot := newIsolationCity(t)
	isolationGitRig(t, rigRoot, true)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      isolationRigs(rigRoot),
		Agents: []config.Agent{{
			Name: "claude", Dir: "app", Implicit: true, PoolSlotWorktrees: true,
		}},
	}

	r := runIsolationCheck(cfg, cityPath)
	if r.Status != StatusOK {
		t.Fatalf("status = %v (%v), want OK", r.Status, r.Details)
	}
}

// TestPoolWorkDirIsolationWarnsWhenAnImplicitAgentStillShares is AC-7: an
// implicit agent whose instances still share a directory gets a warning
// that names the agent and the reason that actually applies.
func TestPoolWorkDirIsolationWarnsWhenAnImplicitAgentStillShares(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		git        bool
		hostLocal  bool
		wantReason string
	}{
		{
			name:       "implicit_isolation none opts out",
			mode:       config.ImplicitIsolationNone,
			wantReason: `implicit_isolation = "none"`,
		},
		{
			name:       "non-git rig mints no slots",
			wantReason: "no git repository",
		},
		{
			name:       "remote runtime skips slot provisioning",
			git:        true,
			hostLocal:  false,
			wantReason: "not host-local",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigRoot := newIsolationCity(t)
			if tc.git {
				isolationGitRig(t, rigRoot, true)
			}
			agent := config.Agent{Name: "claude", Dir: "app", Implicit: true, PoolSlotWorktrees: tc.git}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city", ImplicitIsolation: tc.mode},
				Rigs:      isolationRigs(rigRoot),
				Agents:    []config.Agent{agent},
			}
			r := NewPoolWorkDirIsolationCheck(cfg, cityPath, "test-city", "k8s", tc.hostLocal).Run(nil)

			details := assertIsolationWarning(t, r, "app/claude")
			for _, d := range details {
				if strings.Contains(d, tc.wantReason) {
					return
				}
			}
			t.Fatalf("warning does not name reason %q; details=%v", tc.wantReason, details)
		})
	}
}

// TestPoolWorkDirIsolationWarnsWhenExplicitPoolWorkDirDoesNotVary is AC-7:
// an explicit pool whose work_dir resolves every instance to one directory
// gets a warning that suggests the {{.AgentBase}} template; a per-instance
// template passes.
func TestPoolWorkDirIsolationWarnsWhenExplicitPoolWorkDirDoesNotVary(t *testing.T) {
	cityPath, rigRoot := newIsolationCity(t)
	shared := config.Agent{
		Name: "polecat", Dir: "app", MaxActiveSessions: isolationIntPtr(2),
	}
	varied := config.Agent{
		Name: "ant", Dir: "app", MaxActiveSessions: isolationIntPtr(2),
		WorkDir: ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      isolationRigs(rigRoot),
		Agents:    []config.Agent{shared, varied},
	}

	r := runIsolationCheck(cfg, cityPath)
	details := assertIsolationWarning(t, r, "app/polecat")
	for _, d := range details {
		if strings.Contains(d, "{{.AgentBase}}") && strings.Contains(d, "app/polecat") {
			return
		}
	}
	t.Fatalf("warning does not suggest {{.AgentBase}} for app/polecat; details=%v", details)
}

// TestPoolWorkDirIsolationWarnsWhenAMarkedRigHasAnUnbornHead is AC-7: a
// marked agent on a git rig with no commits probes to distinct slots but
// cannot provision them — HEAD has nothing to branch from — so the check
// warns, and stops warning once the rig has a commit.
func TestPoolWorkDirIsolationWarnsWhenAMarkedRigHasAnUnbornHead(t *testing.T) {
	cityPath, rigRoot := newIsolationCity(t)
	isolationGitRig(t, rigRoot, false)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      isolationRigs(rigRoot),
		Agents: []config.Agent{{
			Name: "claude", Dir: "app", Implicit: true, PoolSlotWorktrees: true,
		}},
	}

	r := runIsolationCheck(cfg, cityPath)
	details := assertIsolationWarning(t, r, "app/claude")
	for _, d := range details {
		if strings.Contains(d, "unborn") {
			return
		}
	}
	t.Fatalf("warning does not name the unborn HEAD; details=%v", details)
}

// TestPoolWorkDirIsolationIgnoresNonPoolAgents pins the check's filter:
// ordinary singleton agents sharing a rig root are today's intended shape
// and stay silent.
func TestPoolWorkDirIsolationIgnoresNonPoolAgents(t *testing.T) {
	cityPath, rigRoot := newIsolationCity(t)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      isolationRigs(rigRoot),
		Agents: []config.Agent{
			{Name: "solo", Dir: "app"},
			{Name: "singleton", Dir: "app", MaxActiveSessions: isolationIntPtr(1)},
		},
	}

	r := runIsolationCheck(cfg, cityPath)
	if r.Status != StatusOK {
		t.Fatalf("status = %v (%v), want OK for non-pool agents", r.Status, r.Details)
	}
}
