//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// gasTownAgent describes an agent for Gas Town integration tests.
// Extends agentConfig with pool, dir, and pre_start settings.
type gasTownAgent struct {
	Name         string
	StartCommand string
	Dir          string // rig directory (working dir for agent)
	Isolation    string // "worktree" or ""
	Pool         *poolConfig
	Suspended    bool
	Env          map[string]string // custom environment variables
}

// poolConfig mirrors config.PoolConfig for test setup.
type poolConfig struct {
	Min   int
	Max   int
	Check string
}

// setupGasTownCity creates a city from gastown-style config and registers
// cleanup. Returns the city directory path.
func setupGasTownCity(t *testing.T, guard *tmuxtest.Guard, agents []gasTownAgent) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	var cityName string
	if guard != nil {
		cityName = guard.CityName()
	} else {
		cityName = uniqueCityName()
	}

	cityDir := filepath.Join(t.TempDir(), cityName)
	configPath := filepath.Join(t.TempDir(), cityName+".toml")
	if err := os.WriteFile(configPath, []byte(renderGasTownToml(cityName, agents)), 0o644); err != nil {
		t.Fatalf("writing init config: %v", err)
	}

	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	waitForExpectedTmuxSessions(t, cityDir, gasTownExpectedSessions(agents))

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCWithEnv(env, "", "stop", cityDir)      //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop") //nolint:errcheck // best-effort cleanup
	})

	time.Sleep(200 * time.Millisecond)
	return cityDir
}

// setupGasTownCityNoGuard creates a Gas Town city without a tmux guard.
func setupGasTownCityNoGuard(t *testing.T, agents []gasTownAgent) string {
	t.Helper()
	return setupGasTownCity(t, nil, agents)
}

func gasTownExpectedSessions(agents []gasTownAgent) []string {
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		if agent.Suspended {
			continue
		}
		names = append(names, agent.Name)
	}
	return names
}

// renderGasTownToml renders a city.toml with gastown-style agents including
// current pool config fields, dir, and env settings.
func renderGasTownToml(cityName string, agents []gasTownAgent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[workspace]\nname = %s\n", quote(cityName))
	fmt.Fprintf(&b, "\n[beads]\nprovider = \"file\"\n")
	fmt.Fprintf(&b, "\n[daemon]\npatrol_interval = \"100ms\"\n")

	for _, a := range agents {
		fmt.Fprintf(&b, "\n[[agent]]\nname = %s\n", quote(a.Name))
		fmt.Fprintf(&b, "start_command = %s\n", quote(a.StartCommand))
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
		if a.Suspended {
			fmt.Fprintf(&b, "suspended = true\n")
		}
		if a.Pool != nil {
			fmt.Fprintf(&b, "min_active_sessions = %d\n", a.Pool.Min)
			fmt.Fprintf(&b, "max_active_sessions = %d\n", a.Pool.Max)
			fmt.Fprintf(&b, "scale_check = %s\n", quote(a.Pool.Check))
		}
		if len(a.Env) > 0 {
			b.WriteString("\n[agent.env]\n")
			for k, v := range a.Env {
				fmt.Fprintf(&b, "%s = %s\n", k, quote(v))
			}
		}
	}

	for _, a := range agents {
		if a.Pool != nil {
			continue
		}
		fmt.Fprintf(&b, "\n[[named_session]]\ntemplate = %s\nmode = \"always\"\n", quote(a.Name))
		if a.Dir != "" {
			fmt.Fprintf(&b, "dir = %s\n", quote(a.Dir))
		}
	}

	return b.String()
}

// waitForBeadStatus polls until a bead reaches the expected status or times out.
// The comparison is case-insensitive to handle bd output format variations.
func waitForBeadStatus(t *testing.T, cityDir, beadID, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := bd(cityDir, "show", beadID)
		if strings.Contains(strings.ToLower(out), strings.ToLower(status)) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := bd(cityDir, "show", beadID)
	t.Fatalf("timed out waiting for bead %s to reach status %q:\n%s", beadID, status, out)
}

// waitForMail polls an agent's inbox until a message matching the pattern arrives.
func waitForMail(t *testing.T, cityDir, recipient, pattern string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := gc(cityDir, "mail", "inbox", recipient)
		if strings.Contains(out, pattern) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := gc(cityDir, "mail", "inbox", recipient)
	t.Fatalf("timed out waiting for mail to %s matching %q:\n%s", recipient, pattern, out)
}

func sessionAssigneeForTemplate(t *testing.T, cityDir, template string) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := gc(cityDir, "session", "list", "--json", "--template", template)
		if err == nil {
			var sessions []struct {
				Template    string
				Closed      bool
				State       string
				SessionName string
			}
			if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &sessions); jsonErr == nil {
				for _, session := range sessions {
					if session.Closed || strings.TrimSpace(session.Template) != template {
						continue
					}
					state := strings.TrimSpace(strings.ToLower(session.State))
					if state != "active" && state != "awake" {
						continue
					}
					if assignee := strings.TrimSpace(session.SessionName); assignee != "" {
						return assignee
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	sessionList, _ := gc(cityDir, "session", "list", "--json", "--template", template)
	out, _ := bd(cityDir, "list", "--all")
	supervisorLog := ""
	if env := parseEnvList(commandEnvForDir(cityDir, false)); env["GC_HOME"] != "" {
		if data, err := os.ReadFile(filepath.Join(env["GC_HOME"], "supervisor.log")); err == nil {
			supervisorLog = tailText(string(data), 120)
		}
	}
	t.Fatalf("timed out waiting for session assignee for template %q\nsessions:\n%s\nbeads:\n%s\nsupervisor log tail:\n%s", template, sessionList, out, supervisorLog)
	return ""
}

func tailText(s string, maxLines int) string {
	if maxLines <= 0 || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n")
}

// initBd initializes a bd database in the given directory when a test
// explicitly needs a standalone beads workspace rather than file-backed
// city.toml configuration.
func initBd(t *testing.T, dir string) string {
	t.Helper()
	prefix := uniqueCityName()
	cmd := exec.Command(bdBinary, "init", "-p", prefix, "--skip-hooks", "-q")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init in %s failed: %v\noutput: %s", dir, err, out)
	}
	return prefix
}

// createBead creates a bead and returns its ID.
func createBead(t *testing.T, cityDir, title string) string {
	t.Helper()
	out, err := bd(cityDir, "create", title)
	if err != nil {
		t.Fatalf("bd create %q failed: %v\noutput: %s", title, err, out)
	}
	return extractBeadID(t, out)
}

// claimBead assigns a bead to an agent.
func claimBead(t *testing.T, cityDir, agent, beadID string) {
	t.Helper()
	out, err := bd(cityDir, "update", beadID, "--assignee="+agent)
	if err != nil {
		t.Fatalf("bd update %s --assignee=%s failed: %v\noutput: %s", beadID, agent, err, out)
	}
}

// sendMail sends a message to a recipient.
func sendMail(t *testing.T, cityDir, to, body string) {
	t.Helper()
	out, err := gc(cityDir, "mail", "send", to, body)
	if err != nil {
		t.Fatalf("gc mail send %s %q failed: %v\noutput: %s", to, body, err, out)
	}
}

// verifyEvents checks that events of the given type exist in the event log.
func verifyEvents(t *testing.T, cityDir, eventType string) {
	t.Helper()
	out, err := gc(cityDir, "events", "--type", eventType)
	if err != nil {
		t.Fatalf("gc events --type %s failed: %v\noutput: %s", eventType, err, out)
	}
	if strings.Contains(out, "No events.") {
		t.Errorf("expected events of type %s, got 'No events.'", eventType)
	}
}

// setupBareGitRepo creates a bare git repo with an initial commit.
// Returns the path to the bare repo.
func setupBareGitRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "bare.git")

	cmds := []struct {
		dir  string
		args []string
	}{
		{"", []string{"git", "init", "--bare", bare}},
	}

	// Create a temp working dir to make the initial commit
	work := filepath.Join(t.TempDir(), "init-work")
	cmds = append(cmds,
		struct {
			dir  string
			args []string
		}{"", []string{"git", "clone", bare, work}},
		struct {
			dir  string
			args []string
		}{work, []string{"git", "config", "user.email", "test@test.com"}},
		struct {
			dir  string
			args []string
		}{work, []string{"git", "config", "user.name", "Test"}},
		struct {
			dir  string
			args []string
		}{work, []string{"touch", "README.md"}},
		struct {
			dir  string
			args []string
		}{work, []string{"git", "add", "README.md"}},
		struct {
			dir  string
			args []string
		}{work, []string{"git", "commit", "-m", "initial commit"}},
		struct {
			dir  string
			args []string
		}{work, []string{"git", "push", "-u", "origin", "HEAD"}},
	)

	for _, c := range cmds {
		runGitCmd(t, c.dir, c.args...)
	}

	return bare
}

// setupWorkingRepo clones a bare repo and returns the working directory path.
func setupWorkingRepo(t *testing.T, bareRepo string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	runGitCmd(t, "", "git", "clone", bareRepo, work)
	runGitCmd(t, work, "git", "config", "user.email", "test@test.com")
	runGitCmd(t, work, "git", "config", "user.name", "Test")
	return work
}

// runGitCmd runs a git command and fails the test if it errors.
func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command %v failed: %v\noutput: %s", args, err, out)
	}
}
